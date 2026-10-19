package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/web/templates"
)

// ActivityPageSize is how many rows one page of the preview feed holds. It is small on
// purpose, so the "Load older activity" control is easy to reach.
const ActivityPageSize = 8

// walletActivityRows is how many rows the Activity tab of a wallet shows.
const walletActivityRows = 25

func leg(key, sym string, amount, usd float64) core.Leg {
	return core.Leg{AssetKey: key, Symbol: sym, Amount: amount, USD: ptr(round2(usd))}
}

// noUSD is a leg whose value at the time is not known.
func noUSD(key, sym string, amount float64) core.Leg {
	return core.Leg{AssetKey: key, Symbol: sym, Amount: amount}
}

// fakeHash is a deterministic 64-character hex hash. A non-empty head and tail replace its
// first and last four characters, so a fixture can read like the board ("3f9a…c21e").
func fakeHash(seed, head, tail string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:])
	if head != "" {
		h = head + h[len(head):]
	}
	if tail != "" {
		h = h[:len(h)-len(tail)] + tail
	}
	return h
}

const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// fakeSignature is a deterministic 88-character Solana-looking signature.
func fakeSignature(seed string) string {
	out := make([]byte, 0, 88)
	for i := 0; len(out) < 88; i++ {
		sum := sha256.Sum256([]byte(seed + "#" + strconv.Itoa(i)))
		for _, b := range sum {
			if len(out) < 88 {
				out = append(out, base58[int(b)%len(base58)])
			}
		}
	}
	return string(out)
}

func uuidLike(seed string) string {
	h := fakeHash(seed, "", "")
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func btcTx(source, seed string, at time.Time, kind core.TxKind, status core.TxStatus, legs ...core.Leg) core.Tx {
	h := fakeHash(seed, "", "")
	return core.Tx{
		ID: core.TxID(source, h), SourceID: source, ExternalID: h, Time: at, Kind: kind, Status: status,
		Legs: legs, TxHash: h, ExplorerURL: "https://mempool.space/tx/" + h,
	}
}

func solTx(seed string, at time.Time, kind core.TxKind, status core.TxStatus, legs ...core.Leg) core.Tx {
	sig := fakeSignature(seed)
	return core.Tx{
		ID: core.TxID(IDPhantom, sig), SourceID: IDPhantom, ExternalID: sig, Time: at, Kind: kind, Status: status,
		Legs: legs, TxHash: sig, ExplorerURL: "https://solscan.io/tx/" + sig,
	}
}

func exchangeTx(source, seed string, at time.Time, kind core.TxKind, legs ...core.Leg) core.Tx {
	ext := uuidLike(seed)
	return core.Tx{ID: core.TxID(source, ext), SourceID: source, ExternalID: ext, Time: at, Kind: kind, Status: core.TxConfirmed, Legs: legs}
}

// activityTxs returns the fixture activity, newest first: BTC receives and sends with
// confirmations, two transfers matched between a source and Cold storage, a Solana swap,
// staking rewards, a failed transaction, trades on Coinbase and Kraken, and years of
// older buys (enough that Coinbase has more than one page).
func activityTxs(now time.Time) []core.Tx {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	var txs []core.Tx

	// A transfer from Coinbase to Cold storage that is still confirming.
	hash := fakeHash("coinbase-to-cold", "3f9a", "c21e")
	out := core.Tx{
		ID: core.TxID(IDCoinbase, uuidLike("cb-send")), SourceID: IDCoinbase, ExternalID: uuidLike("cb-send"),
		Time: ago(32 * time.Minute), Kind: core.TxTransfer, Status: core.TxConfirmed,
		Legs: []core.Leg{leg("BTC", "BTC", -0.05, -3206.00)},
		Fee:  ptr(leg("BTC", "BTC", -0.0000184, -1.18)), TxHash: hash, ExplorerURL: "https://mempool.space/tx/" + hash,
		PeerID: core.TxID(IDCold, hash), PeerSource: IDCold,
	}
	in := core.Tx{
		ID: core.TxID(IDCold, hash), SourceID: IDCold, ExternalID: hash,
		Time: ago(28 * time.Minute), Kind: core.TxTransfer, Status: core.TxPending, Confirmations: 2,
		Legs:   []core.Leg{leg("BTC", "BTC", 0.05, 3206.00)},
		TxHash: hash, ExplorerURL: "https://mempool.space/tx/" + hash,
		PeerID: out.ID, PeerSource: IDCoinbase,
	}
	txs = append(txs, out, in)

	// A Solana swap, a failed send, a staking reward and an LP deposit.
	swap := solTx("sol-swap", ago(3*time.Hour), core.TxSwap, core.TxConfirmed,
		leg("SOL", "SOL", -2, -296.44), leg("USDC", "USDC", 296.12, 296.12))
	swap.Fee = ptr(leg("SOL", "SOL", -0.000005, -0.0007))
	txs = append(txs, swap)

	reward := solTx("sol-reward-1", ago(26*time.Hour), core.TxReward, core.TxConfirmed, leg("SOL", "SOL", 0.0312, 4.62))
	txs = append(txs, reward)
	reward2 := solTx("sol-reward-0", ago(2*day+26*time.Hour), core.TxReward, core.TxConfirmed, leg("SOL", "SOL", 0.0308, 4.55))
	txs = append(txs, reward2)

	failed := solTx("sol-failed", ago(5*day+4*time.Hour), core.TxSend, core.TxFailed, leg("SOL", "SOL", -5, -741.10))
	failed.Fee = ptr(leg("SOL", "SOL", -0.000005, -0.0007))
	failed.Counterparty = "9wQmRk2t7VdC5nXy3hYfPzE8uLb4aJ6sKq1gT3xNvB2e"
	txs = append(txs, failed)

	program := solTx("sol-program", ago(6*day+2*time.Hour), core.TxOther, core.TxConfirmed)
	program.Fee = ptr(leg("SOL", "SOL", -0.00001, -0.0015))
	program.Note = "Program interaction"
	txs = append(txs, program)

	lp := solTx("sol-lp", ago(11*day), core.TxOther, core.TxConfirmed,
		leg("SOL", "SOL", -2.5, -370.55), leg("USDC", "USDC", -370.55, -370.55))
	lp.Note = "Orca whirlpool deposit"
	txs = append(txs, lp)

	airdrop := solTx("sol-airdrop", ago(20*day), core.TxReceive, core.TxConfirmed, leg("JUP", "JUP", 1250, 1375.00))
	txs = append(txs, airdrop)

	stake := solTx("sol-stake", ago(34*day), core.TxStake, core.TxConfirmed, leg("SOL", "SOL", -10, -1482.20))
	txs = append(txs, stake)

	// Coinbase and Kraken trades, a staking reward and a fee.
	sold := exchangeTx(IDCoinbase, "cb-sold-eth", ago(2*day+5*time.Hour), core.TxTrade,
		leg("ETH", "ETH", -0.5, -1590.20), leg("USDC", "USDC", 1582.25, 1582.25))
	sold.Fee = ptr(leg("USD", "USD", -7.95, -7.95))
	txs = append(txs, sold)

	ethReward := exchangeTx(IDCoinbase, "cb-eth-reward", ago(3*day+9*time.Hour), core.TxReward, leg("ETH", "ETH", 0.0021, 6.68))
	txs = append(txs, ethReward)

	krBuy := exchangeTx(IDKraken, "kr-buy-btc", ago(30*time.Hour), core.TxTrade,
		leg("BTC", "BTC", 0.005, 320.60), leg("USD", "USD", -320.60, -320.60))
	krBuy.ExternalID = "TQWE7H-ABCD2-XYZ123"
	krBuy.ID = core.TxID(IDKraken, krBuy.ExternalID)
	krBuy.Fee = ptr(leg("USD", "USD", -0.83, -0.83))
	txs = append(txs, krBuy)

	krSell := exchangeTx(IDKraken, "kr-sell-dot", ago(9*day), core.TxTrade,
		leg("DOT", "DOT", -60, -405.60), leg("USD", "USD", 405.60, 405.60))
	krSell.ExternalID = "TZ4K2M-QRST7-LMN890"
	krSell.ID = core.TxID(IDKraken, krSell.ExternalID)
	txs = append(txs, krSell)

	linkBuy := exchangeTx(IDCoinbase, "cb-buy-link", ago(12*day), core.TxTrade,
		leg("LINK", "LINK", 37.38, 525.62), leg("USDC", "USDC", -525.62, -525.62))
	txs = append(txs, linkBuy)

	cbFee := exchangeTx(IDCoinbase, "cb-fee", ago(15*day), core.TxFee, leg("USD", "USD", -1.99, -1.99))
	txs = append(txs, cbFee)

	// A second matched transfer, from Kraken to Cold storage, and plain BTC sends and
	// receives on Cold storage.
	krHash := fakeHash("kraken-to-cold", "81bd", "0f4a")
	krOut := core.Tx{
		ID: core.TxID(IDKraken, "WD-KR-8841"), SourceID: IDKraken, ExternalID: "WD-KR-8841",
		Time: ago(18*day + 3*time.Hour), Kind: core.TxTransfer, Status: core.TxConfirmed,
		Legs:   []core.Leg{leg("BTC", "BTC", -0.08, -5129.60)},
		TxHash: krHash, ExplorerURL: "https://mempool.space/tx/" + krHash,
		PeerID: core.TxID(IDCold, krHash), PeerSource: IDCold,
	}
	krIn := core.Tx{
		ID: core.TxID(IDCold, krHash), SourceID: IDCold, ExternalID: krHash,
		Time: ago(18*day + 2*time.Hour), Kind: core.TxTransfer, Status: core.TxConfirmed, Confirmations: 6,
		Legs:   []core.Leg{leg("BTC", "BTC", 0.08, 5129.60)},
		TxHash: krHash, ExplorerURL: "https://mempool.space/tx/" + krHash,
		PeerID: krOut.ID, PeerSource: IDKraken,
	}
	txs = append(txs, krOut, krIn)

	send := btcTx(IDCold, "cold-send", ago(44*day), core.TxSend, core.TxConfirmed, leg("BTC", "BTC", -0.02, -1282.40))
	send.Counterparty = "bc1qx2k9d7p4m8w3vz6c5n0r1t8y2u4h7j9m8rt"
	send.Fee = ptr(leg("BTC", "BTC", -0.0000184, -1.18))
	txs = append(txs, send)
	recv := btcTx(IDCold, "cold-recv-1", ago(66*day), core.TxReceive, core.TxConfirmed, leg("BTC", "BTC", 0.10, 6412.00))
	recv.Counterparty = "bc1qp7w4c9n2xk5v8m3d6h0s1y4t7r2e9u3l5a8z0"
	txs = append(txs, recv)
	older := btcTx(IDCold, "cold-recv-0", ago(119*day), core.TxReceive, core.TxConfirmed, leg("BTC", "BTC", 0.06006114, 3851.12))
	txs = append(txs, older)

	// Years of older buys, so Coinbase has more than 25 entries and the feed has pages.
	for i := range 34 {
		price := 51000 + float64(i%9)*870
		amount := 0.002 + float64(i%5)*0.0005
		usd := amount * price
		t := exchangeTx(IDCoinbase, fmt.Sprintf("cb-dca-%d", i), ago(time.Duration(52+i*9)*day+time.Duration(i%7)*time.Hour), core.TxTrade,
			leg("BTC", "BTC", amount, usd), leg("USD", "USD", -usd, -usd))
		txs = append(txs, t)
	}
	for i := range 6 {
		price := 2800 + float64(i)*140
		amount := 0.4 + float64(i)*0.05
		usd := amount * price
		t := exchangeTx(IDKraken, fmt.Sprintf("kr-dca-%d", i), ago(time.Duration(60+i*31)*day), core.TxTrade,
			leg("ETH", "ETH", amount, usd), leg("USD", "USD", -usd, -usd))
		t.ExternalID = fmt.Sprintf("TKR%02d-ABCDE-FGH%03d", i, i*7)
		t.ID = core.TxID(IDKraken, t.ExternalID)
		txs = append(txs, t)
	}

	sort.SliceStable(txs, func(i, j int) bool { return txs[i].Time.After(txs[j].Time) })
	return txs
}

// sourceRefs maps a source id to its label and kind, as the real handler does from the registry.
func sourceRefs(now time.Time) (labels map[string]string, kinds map[string]core.Kind) {
	labels, kinds = map[string]string{}, map[string]core.Kind{}
	for _, w := range Wallets(now) {
		labels[w.ID], kinds[w.ID] = w.Label, w.Kind
	}
	return labels, kinds
}

func activityRows(txs []core.Tx, now time.Time) []templates.ActivityRow {
	labels, kinds := sourceRefs(now)
	rows := make([]templates.ActivityRow, 0, len(txs))
	for _, tx := range txs {
		rows = append(rows, templates.ActivityRow{
			Tx: tx, SourceLabel: labels[tx.SourceID], SourceKind: kinds[tx.SourceID], PeerLabel: labels[tx.PeerSource],
		})
	}
	return rows
}

// ActivityRows returns every fixture activity row, newest first.
func ActivityRows(now time.Time) []templates.ActivityRow { return activityRows(activityTxs(now), now) }

// ActivityCursor encodes the time of the last row of a page, which is where the next page
// starts: a "before" cursor as the Activity page and /activity/list pass it around.
func ActivityCursor(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func parseActivityCursor(s string) (time.Time, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

func hasAsset(tx core.Tx, key string) bool {
	for _, l := range tx.Legs {
		if l.AssetKey == key {
			return true
		}
	}
	return tx.Fee != nil && tx.Fee.AssetKey == key
}

// Activity returns the Activity page for some filters ("" = all) and a "before" cursor
// ("" = the first page). A page holds ActivityPageSize rows; More is set while older rows
// remain.
func Activity(source, asset, kind, before string, now time.Time) templates.ActivityPage {
	cut, hasCut := parseActivityCursor(before)
	var keep []core.Tx
	for _, tx := range activityTxs(now) {
		switch {
		case source != "" && tx.SourceID != source,
			asset != "" && !hasAsset(tx, asset),
			kind != "" && string(tx.Kind) != kind,
			hasCut && !tx.Time.Before(cut):
			continue
		}
		keep = append(keep, tx)
	}
	more := ""
	if len(keep) > ActivityPageSize {
		keep = keep[:ActivityPageSize]
		more = ActivityCursor(keep[len(keep)-1].Time)
	}
	p := templates.ActivityPage{
		Shell:    Shell("Activity", "activity", "all", ""),
		Rows:     activityRows(keep, now),
		More:     more,
		SourceID: source, AssetKey: asset, Kind: kind,
		Sync: SyncRows(now),
		Now:  now,
	}
	for _, w := range Wallets(now) {
		if w.Kind != core.KindManual && w.Kind != core.KindBrokerage {
			p.Sources = append(p.Sources, templates.WalletOption{ID: w.ID, Label: w.Label, Type: w.Type, Kind: w.Kind})
		}
	}
	p.Assets = assetOptions(now)
	return p
}

// ActivityEmpty is the Activity page before anything has synced.
func ActivityEmpty(now time.Time) templates.ActivityPage {
	p := Activity("", "", "", "", now)
	p.Rows, p.More = nil, ""
	for i := range p.Sync {
		p.Sync[i].Count, p.Sync[i].BackfillDone, p.Sync[i].LastSync, p.Sync[i].LastError = 0, false, time.Time{}, ""
	}
	return p
}

// assetOptions are the held assets, by value, for the filter and form pickers.
func assetOptions(now time.Time) []templates.AssetOption {
	var out []templates.AssetOption
	for _, a := range Snapshot(now).Assets {
		name := a.Name
		if name == "" {
			name = a.Symbol
		}
		out = append(out, templates.AssetOption{Key: a.AssetKey, Symbol: a.Symbol, Name: name, ValueUSD: a.ValueUSD})
	}
	return out
}

// SyncRows is the per-source activity sync state: Coinbase and Cold storage are up to
// date, Kraken is still backfilling and failing (rate limited), Phantom is syncing now.
func SyncRows(now time.Time) []templates.SyncRow {
	const every = 15 * time.Minute
	return []templates.SyncRow{
		{ID: IDCold, Label: "Cold storage", Type: "bitcoin", Kind: core.KindHardware, Count: 38, BackfillDone: true, LastSync: now.Add(-2 * time.Minute), Every: every},
		{ID: IDCoinbase, Label: "Coinbase", Type: "coinbase", Kind: core.KindExchange, Count: 1204, BackfillDone: true, LastSync: now.Add(-2 * time.Minute), Every: every},
		{ID: IDKraken, Label: "Kraken", Type: "kraken", Kind: core.KindExchange, Count: 212, BackfillDone: false, LastSync: now.Add(-3 * time.Hour),
			LastError: "kraken: rate limited: EAPI:Rate limit exceeded", Every: every},
		{ID: IDPhantom, Label: "Phantom", Type: "solana", Kind: core.KindHot, Count: 96, BackfillDone: false, Syncing: true, LastSync: now.Add(-2 * time.Minute), Every: every},
	}
}

// walletActivity fills the Activity tab fields of a wallet page: its newest entries, and
// whether older ones exist.
func walletActivity(p *templates.WalletPage, now time.Time) {
	var txs []core.Tx
	for _, tx := range activityTxs(now) {
		if tx.SourceID == p.W.ID {
			txs = append(txs, tx)
		}
	}
	if len(txs) > walletActivityRows {
		txs, p.ActivityMore = txs[:walletActivityRows], true
	}
	p.Activity = activityRows(txs, now)
}
