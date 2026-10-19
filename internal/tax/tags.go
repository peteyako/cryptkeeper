package tax

import (
	"math"

	"cryptkeeper/internal/core"
)

// knownTag are the tags that take effect.
var knownTag = map[Tag]bool{
	TagPurchase: true, TagIncome: true, TagAirdrop: true, TagGiftReceived: true, TagOwnWallet: true,
	TagPosition: true, TagPositionGain: true, TagSpam: true, TagSale: true, TagGift: true, TagLost: true,
}

// ReceiveTags are the tags that fit a receive, in display order. TagPositionGain also fits a
// receive, but only a position move; callers add it for those.
func ReceiveTags() []Tag {
	return []Tag{TagPurchase, TagIncome, TagAirdrop, TagGiftReceived, TagOwnWallet, TagPosition, TagSpam}
}

// SendTags are the tags that fit a send, in display order.
func SendTags() []Tag {
	return []Tag{TagSale, TagGift, TagLost, TagOwnWallet, TagPosition, TagSpam}
}

// tagOf returns the tag that applies to tx. Tags apply to receive, send, other and position
// txs without a PeerID (on a position only a spam tag has an effect), and to a transfer half
// of any kind that found no usable peer (see unpaired); on any other tx they are ignored.
func (e *engine) tagOf(tx core.Tx) (Tag, bool) {
	if tx.PeerID != "" {
		if !e.unpaired(tx) {
			return "", false
		}
		t, ok := e.in.Tags[tx.ID]
		return t, ok && knownTag[t]
	}
	switch tx.Kind {
	case core.TxReceive, core.TxSend, core.TxOther, core.TxPosition:
		t, ok := e.in.Tags[tx.ID]
		return t, ok && knownTag[t] // an unknown tag string is treated as untagged
	}
	return "", false
}

// tagged applies tag to the legs of a receive, send or other tx (or an unpaired transfer
// half). A tagged tx never raises an unmatched issue. A tag that does not fit a leg's
// direction (gift on a receive, income on a send) falls back to the plain treatment: an
// acquisition or disposal at market value. TagPosition and TagPositionGain (on a non-send)
// are routed to position by taggedMove and never reach here; a position-gain on a send falls
// to the plain treatment.
func (e *engine) tagged(tx core.Tx, tag Tag, legs []*valued) {
	for _, v := range legs {
		if math.Abs(v.Amount) <= dust {
			continue
		}
		recv := v.Amount > 0
		switch {
		case tag == TagIncome && recv, tag == TagAirdrop && recv:
			value := e.usd(tx, v)
			e.acquire(tx, v.AssetKey, v.Amount, value)
			e.out.Income = append(e.out.Income, Income{
				Date: tx.Time, Asset: v.AssetKey, Amount: v.Amount, ValueUSD: value,
				Type: string(tag), Wallet: tx.SourceID, TxID: tx.ID,
			})
		case tag == TagOwnWallet && recv:
			e.fromUntracked(tx, v)
		case tag == TagGiftReceived && recv:
			e.carriedIn(tx, v, v.Amount)
		case tag == TagOwnWallet:
			e.toUntracked(tx, v.AssetKey, -v.Amount)
		case tag == TagGift && !recv, tag == TagLost && !recv:
			e.giftLost(tx, v, tag)
		default: // purchase, sale and any tag that does not fit
			e.settle(tx, v, "")
		}
	}
}

// carriedIn opens a lot of amount (≤ the leg's) for a receipt whose cost is not the market
// value: the leg's price override pro rata if one is set, else 0 with FlagMissingBasis and
// IssueMissingBasis.
func (e *engine) carriedIn(tx core.Tx, v *valued, amount float64) {
	l := Lot{Wallet: tx.SourceID, Asset: v.AssetKey, Amount: amount, Acquired: tx.Time, TxID: tx.ID}
	if p, ok := e.in.Prices[PriceKey(tx.ID, v.idx)]; ok && finite(p) {
		l.CostUSD = math.Abs(p) * amount / v.Amount
	} else {
		l.Flag = FlagMissingBasis
		e.issue(IssueMissingBasis, tx, v.AssetKey, amount)
	}
	e.addLot(l)
}

// fromUntracked books an own-wallet receive: coins coming back from a wallet CryptKeeper does
// not track. They are taken FIFO from the asset's Untracked lots, which move into the
// receiving wallet keeping their dates, cost and flags (like a transfer); only what those
// cannot cover is a new lot, as for a gift received (see carriedIn).
func (e *engine) fromUntracked(tx core.Tx, v *valued) {
	taken, short := e.queueFor(Untracked, v.AssetKey).take(v.Amount)
	for _, l := range taken {
		l.Wallet = tx.SourceID
		e.addLot(l)
	}
	if short > 0 {
		e.carriedIn(tx, v, short)
	}
}

// toUntracked moves amount of asset FIFO from tx's wallet to Untracked, keeping the lots'
// dates and cost. What the lots cannot cover arrives as a lot at cost 0 acquired at the
// send, with FlagMissingBasis, plus IssueMissingBasis. No disposal.
func (e *engine) toUntracked(tx core.Tx, asset string, amount float64) {
	taken, short := e.queueFor(tx.SourceID, asset).take(amount)
	for _, l := range taken {
		l.Wallet = Untracked
		e.addLot(l)
	}
	if short > 0 {
		e.addLot(Lot{Wallet: Untracked, Asset: asset, Amount: short, Acquired: tx.Time, TxID: tx.ID, Flag: FlagMissingBasis})
		e.issue(IssueMissingBasis, tx, asset, short)
	}
}

// giftLost removes a sent leg's lots and books a GiftsLost row per piece, with no proceeds
// and no gain.
func (e *engine) giftLost(tx core.Tx, v *valued, tag Tag) {
	note := "Gift"
	if tag == TagLost {
		note = "Lost"
	}
	for _, d := range e.consume(tx, v.AssetKey, -v.Amount, 0, note) {
		d.Note, d.GainUSD = note, 0 // a shortfall row is still a gift or loss, not "Missing cost basis"
		e.out.GiftsLost = append(e.out.GiftsLost, d)
	}
}

// settle books one prepared leg as an acquisition (positive) or a disposal (negative) at its
// value, with note on a disposal and no unmatched issue.
func (e *engine) settle(tx core.Tx, v *valued, note string) {
	switch {
	case math.Abs(v.Amount) <= dust:
	case v.Amount < 0:
		e.dispose(tx, v.AssetKey, -v.Amount, e.usd(tx, v), note)
	default:
		e.acquire(tx, v.AssetKey, v.Amount, e.usd(tx, v))
	}
}
