// Package tax computes per-wallet FIFO tax lots, disposals and income from the activity
// feed, for a Koinly-style tax report.
//
// Not tax advice: an informational estimate.
package tax

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
)

// Disposal notes, exactly as Koinly prints them.
const (
	noteFee          = "Fee"
	noteMissingBasis = "Missing cost basis"
)

// fiat are the currencies that only carry value: they are never lots.
var fiat = map[string]bool{"USD": true, "EUR": true, "GBP": true, "CAD": true, "JPY": true, "AUD": true, "CHF": true}

// Input is everything Compute needs for the open tax year.
type Input struct {
	// From is the start of the first open tax year (zero when no year is closed). Earlier
	// txs are not replayed, and earlier manual lots and write-offs are not applied: they are
	// already in Opening and OpeningParked.
	From time.Time
	// To is the exclusive end (zero = no end): txs at or after it are not replayed, though a
	// transfer half in the window still pairs with them (see transfer), and manual lots and
	// write-offs at or after it are ignored.
	To      time.Time
	Loc     *time.Location // tax timezone; nil = UTC
	Opening []Lot          // closing lots of the last closed year
	// OpeningParked is what the wallets held in positions at From, so a withdrawal of it is
	// not income.
	OpeningParked []Parked
	// PairedBeforeFrom is the IDs of the transfer halves the close of the last year carried
	// (its Year.CarriedPeers: halves at or after its end whose pair that close processed).
	// Only such a half is skipped silently when its peer is before From; any other half
	// with its peer before From raises IssueTransferUnpaired (see transfer).
	PairedBeforeFrom map[string]bool
	// Manual is the manual acquisitions, applied at Acquired. An undated one (zero Acquired)
	// applies at the start of the replay when From is zero (dated at the first replayed tx,
	// with IssueUndatedLot), and is dropped like any lot before From once a year is closed.
	Manual []Lot
	// Txs is the activity of every wallet, in any order: all of it, not only [From, To), so
	// a transfer half can find its peer across the boundaries.
	Txs    []core.Tx
	Tags   map[string]Tag     // by Tx.ID
	Prices map[string]float64 // PriceKey / FeePriceKey → USD
	Spam   map[string]bool    // asset keys ignored entirely (hidden mints)
	// Quiet is the asset keys hidden automatically (unpriced or dust tokens): they are
	// processed like any asset, except that a worthless receive of one opens its cost-0 lot
	// without an issue (see movement).
	Quiet map[string]bool
	// WriteOffs dispose of parked units at their Date (those before From are dropped); TxID
	// "writeoff:<index>", the index into WriteOffs.
	WriteOffs []WriteOff
}

// Result is the open year's lots, realized gains, income and review issues.
type Result struct {
	Lots      []Lot      // open lots, sorted by Wallet, Asset, Acquired
	Disposals []Disposal // sorted by Sold, then TxID
	Income    []Income
	GiftsLost []Disposal
	Issues    []Issue
	Parked    []Parked // non-zero balances held in positions at the end, by Wallet, Asset
	// CarriedPeers is the sorted IDs of the transfer halves at or after To whose pair this
	// computation processed (see transfer).
	CarriedPeers []string
	// Unpaired is the sorted IDs of the replayed transfer halves that found no usable peer
	// (see transfer), tagged or not: the halves a tag applies to.
	Unpaired []string
}

// Compute replays the open year over one FIFO queue per (wallet, asset), where the wallet
// is the tx's SourceID.
//
// Order: the opening lots, then every tx in [From, To) (To zero = no end), by Time then ID.
// A manual lot joins its queue before the first tx at or after its Acquired. A write-off
// (see writeOff) is applied in the same merge, after the manual lots and before the txs at
// its instant. Manual lots and write-offs at or after To are ignored. Those before From are
// ignored too when From is set, since the opening lots and parked balances (the last closed
// year's closing ones) already include them; an undated manual lot counts as before From.
// With From zero (nothing closed) an undated manual lot is applied first, dated at the first
// replayed tx (so a sale of it is short-term unless held a year past the start of the
// history), and raises IssueUndatedLot.
//
// Per tx:
//
//   - A failed tx only pays its fee. Pending txs count like confirmed ones.
//   - Legs of one asset are netted into one, and spam legs are dropped. Each remaining leg
//     is valued (PriceKey override, then |Leg.USD|, then |Amount| for pegged assets), and a
//     side's single unvalued leg takes the other side's value, fiat legs included. Only
//     then are fiat legs dropped.
//   - A tx with a PeerID, whatever its kind, is one half of a transfer between wallets. The
//     pair is processed once, when its first half comes up: lots move FIFO from the sending
//     wallet to the receiving one keeping their dates and cost, sent − received is a
//     disposal of the sender with Note "Fee", and received − sent an acquisition, except
//     that a difference the tx's own Fee already pays is not disposed twice. A value-less
//     side takes the other side's value. An asset on one half only is settled on its half
//     the same way (a "Fee" disposal or an acquisition), with no unmatched issue. The peer
//     is looked up among all of Input.Txs, so a pair can straddle From or To (see transfer):
//     a half whose peer is at or after To processes the pair when it comes up, with every
//     row dated at that half's time, and lists the peer in Result.CarriedPeers; a half in
//     Input.PairedBeforeFrom whose peer is before From changes nothing (the prior close
//     processed the pair). A half whose peer is missing, failed or not linked back, or
//     before From without the half in PairedBeforeFrom, changes no lots and raises
//     IssueTransferUnpaired, unless it is tagged: a tag on such a half applies as on a
//     receive or send (see below), by the sign of its legs.
//   - trade / swap: each negative leg is a disposal at its value, each positive leg a lot
//     with its value as cost.
//   - fee: each negative leg is a disposal at its value with Note "Fee".
//   - receive, send, reward, other and unknown kinds: each positive leg is a lot at its
//     value, each negative leg a disposal at its value. A reward's positive leg also books
//     Income of Type "reward"; the others raise IssueUnmatchedReceive / IssueUnmatchedSend
//     per leg (a reward raises none). Dust legs are ignored. A positive leg of a Quiet
//     asset worth 0 or unvalued opens its cost-0 lot with no issue at all.
//   - position: see position; the kind decides, whatever the tag (spam aside; a
//     position-gain tag turns the excess into a gain).
//   - stake, and transfer without a PeerID: no lot changes.
//   - A known Tags entry on a receive, send or other tx without a PeerID replaces the
//     movement rule (see tagged; a position tag goes to position); an unknown tag string is
//     ignored, so the tx is untagged. A spam tag ignores the tx entirely, its fee included.
//   - Last, Tx.Fee (unless fiat or spam) is a disposal at its value (FeePriceKey override
//     first) with Note "Fee".
//
// A disposal consumes lots FIFO with one row per lot piece and proceeds shared pro-rata by
// amount. What the lots cannot cover is a row at cost 0, acquired at the sale (so
// short-term), with Note "Missing cost basis", plus IssueMissingBasis. A leg with no value
// counts as 0 and raises IssueMissingPrice.
func Compute(in Input) Result {
	e := newEngine(in)
	for _, l := range in.Opening {
		e.addLot(l)
	}
	for _, p := range in.OpeningParked {
		if !(p.Amount > dust) { // also drops NaN
			continue
		}
		e.park(p.Wallet, p.Asset, p.Amount)
	}
	txs := txsBefore(txsFrom(in.Txs, in.From), in.To)
	e.index(in.Txs) // all txs: a transfer's peer may be outside [From, To)
	start := in.From
	if start.IsZero() && len(txs) > 0 {
		start = txs[0].Time
	}
	events := preTxEvents(e.manualLots(in.Manual, start), in.WriteOffs, in.From, in.To)
	next := 0
	for _, tx := range txs {
		for ; next < len(events) && !events[next].at.After(tx.Time); next++ {
			e.applyEvent(events[next])
		}
		e.apply(tx)
	}
	for _, ev := range events[next:] {
		e.applyEvent(ev)
	}
	return e.result()
}

// txsFrom returns the txs at or after from, by Time then ID.
func txsFrom(txs []core.Tx, from time.Time) []core.Tx {
	out := slices.DeleteFunc(slices.Clone(txs), func(tx core.Tx) bool { return tx.Time.Before(from) })
	slices.SortStableFunc(out, func(a, b core.Tx) int {
		return cmp.Or(a.Time.Compare(b.Time), strings.Compare(a.ID, b.ID))
	})
	return out
}

// txsBefore drops the txs at or after to; a zero to keeps them all.
func txsBefore(txs []core.Tx, to time.Time) []core.Tx {
	if to.IsZero() {
		return txs
	}
	return slices.DeleteFunc(txs, func(tx core.Tx) bool { return !tx.Time.Before(to) })
}

// event is a manual lot or a write-off, applied before the txs at the same instant.
type event struct {
	at       time.Time
	lot      Lot
	writeOff WriteOff
	index    int // of the write-off in Input.WriteOffs
	isWO     bool
}

// preTxEvents merges the manual lots (already ordered and filtered by from, see manualLots)
// and the write-offs in [from, to) (to zero = no end) by time. At one instant manual lots
// come first, then write-offs by index. A write-off before from is already in the opening
// parked balances and lots, so it is dropped (with from zero none is before it).
func preTxEvents(manual []Lot, wos []WriteOff, from, to time.Time) []event {
	var out []event
	for _, l := range manual {
		if !to.IsZero() && !l.Acquired.Before(to) {
			continue
		}
		out = append(out, event{at: l.Acquired, lot: l})
	}
	for i, w := range wos {
		if w.Date.Before(from) || (!to.IsZero() && !w.Date.Before(to)) {
			continue
		}
		out = append(out, event{at: w.Date, writeOff: w, index: i, isWO: true})
	}
	slices.SortStableFunc(out, func(a, b event) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(b2i(a.isWO), b2i(b.isWO)), cmp.Compare(a.index, b.index))
	})
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// manualLots returns the manual lots that apply, by Acquired. With From zero (nothing
// closed) that is all of them, an undated one dated start (the first replayed tx) with
// IssueUndatedLot. Otherwise a lot before From, or undated, is already in the opening lots
// and is dropped.
func (e *engine) manualLots(lots []Lot, start time.Time) []Lot {
	out := slices.Clone(lots)
	if from := e.in.From; !from.IsZero() {
		out = slices.DeleteFunc(out, func(l Lot) bool { return l.Acquired.IsZero() || l.Acquired.Before(from) })
	}
	for i := range out {
		if out[i].Acquired.IsZero() {
			out[i].Acquired = start
			l := out[i]
			e.out.Issues = append(e.out.Issues, Issue{
				Kind: IssueUndatedLot, TxID: l.TxID, Wallet: l.Wallet, Asset: l.Asset, Amount: l.Amount, Time: start,
			})
		}
	}
	slices.SortStableFunc(out, func(a, b Lot) int { return a.Acquired.Compare(b.Acquired) })
	return out
}

// applyEvent adds a manual lot or applies a write-off.
func (e *engine) applyEvent(ev event) {
	if ev.isWO {
		e.writeOff(ev.index, ev.writeOff)
		return
	}
	e.addLot(ev.lot)
}

// engine is the replay state. Each tx kind has its own method, so new treatments plug into
// apply / byKind without growing Compute.
type engine struct {
	in     Input
	lots   map[string]map[string]*queue  // wallet → asset → open lots
	peers  map[string]core.Tx            // by ID: the txs a transfer half can pair with
	paired map[string]bool               // IDs of transfer halves already processed
	parked map[string]map[string]float64 // wallet → asset → amount held in positions
	out    Result
}

func newEngine(in Input) *engine {
	return &engine{in: in, lots: map[string]map[string]*queue{}, peers: map[string]core.Tx{}, paired: map[string]bool{}, parked: map[string]map[string]float64{}}
}

// apply processes one tx: its legs as a transfer half (when it has a PeerID) or by kind,
// skipped when the tx failed, then its fee.
func (e *engine) apply(tx core.Tx) {
	if tag, ok := e.tagOf(tx); ok && tag == TagSpam {
		if tx.PeerID != "" {
			e.out.Unpaired = append(e.out.Unpaired, tx.ID) // tagOf only tags an unpaired half
		}
		return // a spam tx is ignored entirely, its fee too
	}
	switch {
	case tx.Status == core.TxFailed:
	case tx.PeerID != "":
		e.transfer(tx)
	default:
		e.byKind(tx, e.legs(tx))
	}
	e.fee(tx)
}

// byKind applies the rule for tx's kind to its prepared legs.
func (e *engine) byKind(tx core.Tx, legs []*valued) {
	switch tx.Kind {
	case core.TxTrade, core.TxSwap:
		e.trade(tx, legs)
	case core.TxFee:
		e.feeLegs(tx, legs)
	case core.TxStake, core.TxTransfer:
		// Staking, or a self-send inside one wallet (a transfer without a PeerID): no lot
		// changes, only the fee.
	case core.TxPosition:
		// The kind is authoritative: any tag but spam (handled by apply) and position-gain
		// is ignored.
		tag, _ := e.tagOf(tx)
		e.position(tx, legs, tag == TagPositionGain)
	default: // receive, send, reward, other and any unknown kind
		if tag, ok := e.tagOf(tx); ok {
			e.taggedMove(tx, tag, legs)
			return
		}
		e.movement(tx, legs)
	}
}

// taggedMove applies a tag to a receive, send or other tx, or to an unpaired transfer half
// (a send when all its legs leave the wallet).
func (e *engine) taggedMove(tx core.Tx, tag Tag, legs []*valued) {
	send := tx.Kind == core.TxSend || (tx.PeerID != "" && allOutgoing(legs))
	switch {
	case tag == TagPosition:
		e.position(tx, legs, false)
	case tag == TagPositionGain && !send:
		// position-gain means "this is a position withdrawal whose excess is a gain", so a
		// non-position receive tagged with it is treated as that withdrawal. On a send it
		// does not fit and falls back to the plain treatment.
		e.position(tx, legs, true)
	default:
		e.tagged(tx, tag, legs)
	}
}

// allOutgoing reports whether every leg leaves the wallet.
func allOutgoing(legs []*valued) bool {
	for _, v := range legs {
		if v.Amount > 0 {
			return false
		}
	}
	return len(legs) > 0
}

// movement handles an untagged receive, send, reward or other tx (a transfer half never
// comes here: see settle) by leg sign. A positive leg is an acquisition at market
// value (a reward also books income), a negative leg a disposal at market value. Every leg
// raises an unmatched issue for review unless the tx is a reward, or the leg is a Quiet
// asset received worth 0 or unvalued (a cost-0 lot with no issue). A dust leg, like a dust
// fee, is ignored.
func (e *engine) movement(tx core.Tx, legs []*valued) {
	reward := tx.Kind == core.TxReward
	for _, v := range legs {
		switch {
		case math.Abs(v.Amount) <= dust:
		case v.Amount < 0:
			e.dispose(tx, v.AssetKey, -v.Amount, e.usd(tx, v), "")
			if !reward {
				e.issue(IssueUnmatchedSend, tx, v.AssetKey, v.Amount)
			}
		case v.Amount > 0:
			if !reward && e.in.Quiet[v.AssetKey] && (!v.known || v.usd == 0) {
				// An auto-hidden token that arrived worth nothing (an airdrop): a cost-0 lot,
				// no review item.
				e.acquire(tx, v.AssetKey, v.Amount, 0)
				continue
			}
			value := e.usd(tx, v)
			e.acquire(tx, v.AssetKey, v.Amount, value)
			if reward {
				e.out.Income = append(e.out.Income, Income{
					Date: tx.Time, Asset: v.AssetKey, Amount: v.Amount, ValueUSD: value,
					Type: "reward", Wallet: tx.SourceID, TxID: tx.ID,
				})
			} else {
				e.issue(IssueUnmatchedReceive, tx, v.AssetKey, v.Amount)
			}
		}
	}
}

// trade disposes of each negative leg at its value and opens a lot for each positive leg
// with its value as cost.
func (e *engine) trade(tx core.Tx, legs []*valued) {
	for _, v := range legs {
		if v.Amount < 0 {
			e.dispose(tx, v.AssetKey, -v.Amount, e.usd(tx, v), "")
		}
	}
	for _, v := range legs {
		if v.Amount > 0 {
			e.acquire(tx, v.AssetKey, v.Amount, e.usd(tx, v))
		}
	}
}

// feeLegs disposes of a fee tx's outgoing legs as fees.
func (e *engine) feeLegs(tx core.Tx, legs []*valued) {
	for _, v := range legs {
		if v.Amount < 0 {
			e.dispose(tx, v.AssetKey, -v.Amount, e.usd(tx, v), noteFee)
		}
	}
}

// fee disposes of tx.Fee at its value with Note "Fee". A fiat or spam fee is ignored.
func (e *engine) fee(tx core.Tx) {
	f := tx.Fee
	if f == nil || !e.tracked(f.AssetKey) || math.Abs(f.Amount) <= dust {
		return
	}
	v := &valued{Leg: *f}
	v.usd, v.known = valueLeg(*f, FeePriceKey(tx.ID), e.in.Prices)
	e.dispose(tx, f.AssetKey, math.Abs(f.Amount), e.usd(tx, v), noteFee)
}

// legs prepares tx's legs for its kind rule. Legs of one asset are netted into one, which
// keeps the index of the asset's first leg for its PriceKey; assets that net to zero are
// dropped. Spam legs are dropped first, so they never take part in valuation. Each leg is
// valued, and a side's single unvalued leg is filled from the other side, fiat legs
// included. Only then are fiat legs dropped.
func (e *engine) legs(tx core.Tx) []*valued {
	all := slices.DeleteFunc(net(tx.Legs), func(v *valued) bool { return e.in.Spam[v.AssetKey] })
	var neg, pos []*valued
	for _, v := range all {
		v.usd, v.known = valueLeg(v.Leg, PriceKey(tx.ID, v.idx), e.in.Prices)
		if v.Amount < 0 {
			neg = append(neg, v)
		} else {
			pos = append(pos, v)
		}
	}
	if tx.PeerID == "" { // a transfer half is not a trade
		fillTrade(neg, pos)
	}
	return slices.DeleteFunc(all, func(v *valued) bool { return fiat[v.AssetKey] })
}

// net sums the legs of each asset into one, in order of first appearance, keeping the
// index of the asset's first leg. The sum's USD is the sum of the parts' signed USD, or nil
// when any part has none. Assets that net to zero are dropped.
func net(legs []core.Leg) []*valued {
	var out []*valued
	byAsset := map[string]*valued{}
	for i, l := range legs {
		v, ok := byAsset[l.AssetKey]
		if !ok {
			v = &valued{Leg: l, idx: i}
			byAsset[l.AssetKey] = v
			out = append(out, v)
			continue
		}
		v.Amount += l.Amount
		if usableUSD(v.USD) && usableUSD(l.USD) {
			sum := *v.USD + *l.USD // a new value: never write through to the caller's leg
			v.USD = &sum
		} else {
			v.USD = nil
		}
	}
	return slices.DeleteFunc(out, func(v *valued) bool { return math.Abs(v.Amount) <= dust })
}

// tracked reports whether an asset is held in lots: fiat only carries value and spam is
// ignored.
func (e *engine) tracked(asset string) bool { return !fiat[asset] && !e.in.Spam[asset] }

// usd is a prepared leg's value. An unvalued leg is worth 0 and raises IssueMissingPrice.
func (e *engine) usd(tx core.Tx, v *valued) float64 {
	if !v.known {
		e.issue(IssueMissingPrice, tx, v.AssetKey, v.Amount)
		return 0
	}
	return v.usd
}

// queueFor returns the open lots of asset in wallet, creating the queue on first use.
func (e *engine) queueFor(wallet, asset string) *queue {
	byAsset, ok := e.lots[wallet]
	if !ok {
		byAsset = map[string]*queue{}
		e.lots[wallet] = byAsset
	}
	q, ok := byAsset[asset]
	if !ok {
		q = &queue{}
		byAsset[asset] = q
	}
	return q
}

// addLot puts l in its wallet's queue.
func (e *engine) addLot(l Lot) { e.queueFor(l.Wallet, l.Asset).add(l) }

// acquire opens a lot in tx's wallet, acquired at tx's time.
func (e *engine) acquire(tx core.Tx, asset string, amount, cost float64) {
	e.addLot(Lot{Wallet: tx.SourceID, Asset: asset, Amount: amount, CostUSD: cost, Acquired: tx.Time, TxID: tx.ID})
}

// dispose removes amount (> 0) of asset from tx's wallet FIFO and records the rows in
// Result.Disposals.
func (e *engine) dispose(tx core.Tx, asset string, amount, proceeds float64, note string) {
	e.out.Disposals = append(e.out.Disposals, e.consume(tx, asset, amount, proceeds, note)...)
}

// consume removes amount (> 0) of asset from tx's wallet FIFO and returns one Disposal per
// consumed lot piece, sharing proceeds pro-rata by amount. What the lots cannot cover is a
// row at cost 0 acquired at the sale, with Note "Missing cost basis", plus
// IssueMissingBasis. The caller decides where the rows go.
func (e *engine) consume(tx core.Tx, asset string, amount, proceeds float64, note string) []Disposal {
	taken, short := e.queueFor(tx.SourceID, asset).take(amount)
	rows := make([]Disposal, 0, len(taken)+1)
	for _, l := range taken {
		rows = append(rows, e.disposal(tx, l, proceeds*l.Amount/amount, note))
	}
	if short > 0 {
		missing := Lot{Wallet: tx.SourceID, Asset: asset, Amount: short, Acquired: tx.Time}
		rows = append(rows, e.disposal(tx, missing, proceeds*short/amount, noteMissingBasis))
		e.issue(IssueMissingBasis, tx, asset, short)
	}
	return rows
}

// disposal builds the row for the sale of lot piece l by tx.
func (e *engine) disposal(tx core.Tx, l Lot, proceeds float64, note string) Disposal {
	return Disposal{
		Sold: tx.Time, Acquired: l.Acquired, Various: l.Various,
		Asset: l.Asset, Amount: l.Amount,
		CostUSD: l.CostUSD, ProceedsUSD: proceeds, GainUSD: proceeds - l.CostUSD,
		Wallet: tx.SourceID, Term: termOf(l.Acquired, tx.Time, e.in.Loc), Note: note, TxID: tx.ID, LotTxID: l.TxID,
	}
}

// issue records a problem with tx for the review queue. The amount is stored as its
// absolute value.
func (e *engine) issue(kind IssueKind, tx core.Tx, asset string, amount float64) {
	e.out.Issues = append(e.out.Issues, Issue{
		Kind: kind, TxID: tx.ID, Wallet: tx.SourceID, Asset: asset, Amount: math.Abs(amount), Time: tx.Time,
	})
}

// result collects the open lots by wallet, asset and acquisition, and sorts the disposals.
func (e *engine) result() Result {
	r := e.out
	for _, wallet := range slices.Sorted(maps.Keys(e.lots)) {
		byAsset := e.lots[wallet]
		for _, asset := range slices.Sorted(maps.Keys(byAsset)) {
			r.Lots = append(r.Lots, byAsset[asset].lots...)
		}
	}
	for _, wallet := range slices.Sorted(maps.Keys(e.parked)) {
		byAsset := e.parked[wallet]
		for _, asset := range slices.Sorted(maps.Keys(byAsset)) {
			if byAsset[asset] > dust {
				r.Parked = append(r.Parked, Parked{Wallet: wallet, Asset: asset, Amount: byAsset[asset]})
			}
		}
	}
	slices.SortStableFunc(r.Disposals, func(a, b Disposal) int {
		return cmp.Or(a.Sold.Compare(b.Sold), strings.Compare(a.TxID, b.TxID))
	})
	slices.Sort(r.CarriedPeers)
	r.CarriedPeers = slices.Compact(r.CarriedPeers)
	slices.Sort(r.Unpaired)
	r.Unpaired = slices.Compact(r.Unpaired)
	return r
}
