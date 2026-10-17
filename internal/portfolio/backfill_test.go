package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

var bfNow = time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)

func eod(d time.Time) int64 { return d.Add(24*time.Hour - time.Second).Unix() }

func bfSnap() Snapshot {
	return Snapshot{
		UpdatedAt: bfNow,
		Wallets: []WalletView{{ID: "w", Rows: []HoldingRow{
			{Holding: core.Holding{WalletID: "w", AssetKey: "SOL", Amount: 10}},
			{Holding: core.Holding{WalletID: "w", AssetKey: "spl:X", Mint: "X", Amount: 5}},
		}}},
		Prices: map[string]core.Price{"SOL": {USD: 100}, "spl:X": {USD: 2}},
	}
}

func bfPrice(_ context.Context, key, _ string, _ time.Time) (float64, bool, error) {
	if key == "SOL" {
		return 50, true, nil
	}
	return 0, false, nil
}

func bfRows(t *testing.T, h *History) map[int64]historyRow {
	t.Helper()
	rows, err := h.DB.Query(`SELECT ts, prices, amounts FROM history`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]historyRow{}
	for rows.Next() {
		var ts int64
		var pj, aj string
		if err := rows.Scan(&ts, &pj, &aj); err != nil {
			t.Fatal(err)
		}
		var r historyRow
		json.Unmarshal([]byte(pj), &r.Prices)
		json.Unmarshal([]byte(aj), &r.Amounts)
		out[ts] = r
	}
	return out
}

func TestBackfillWritesDailyRows(t *testing.T) {
	h := &History{DB: openDB(t)}
	n, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, bfPrice)
	if err != nil || n != 8 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	rows := bfRows(t, h)
	day := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 8; i++ {
		r, ok := rows[eod(day.AddDate(0, 0, -i))]
		if !ok {
			t.Fatalf("missing day -%d", i)
		}
		if !near(r.Amounts["w"]["SOL"], 10) || !near(r.Prices["SOL"], 50) {
			t.Fatalf("day -%d: %+v", i, r)
		}
	}
}

func TestBackfillRollsBackActivity(t *testing.T) {
	h := &History{DB: openDB(t)}
	txs := []core.Tx{{SourceID: "w", Time: bfNow.AddDate(0, 0, -2), Legs: []core.Leg{{AssetKey: "SOL", Amount: 3}}}}
	if _, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), txs, nil, bfPrice); err != nil {
		t.Fatal(err)
	}
	rows := bfRows(t, h)
	day := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	// tx at Oct 14 15:00: the Oct 14 row is stamped 23:59:59 (end of day) and
	// counts txs before the next midnight, so it holds 10; Oct 13 and older hold 7.
	if got := rows[eod(day.AddDate(0, 0, -3))].Amounts["w"]["SOL"]; !near(got, 7) {
		t.Fatalf("Oct 13 = %v", got)
	}
	if got := rows[eod(day.AddDate(0, 0, -2))].Amounts["w"]["SOL"]; !near(got, 10) {
		t.Fatalf("Oct 14 = %v", got)
	}
}

func TestBackfillIdempotent(t *testing.T) {
	h := &History{DB: openDB(t)}
	if _, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, bfPrice); err != nil {
		t.Fatal(err)
	}
	n, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, bfPrice)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestBackfillKeepsRecordedRows(t *testing.T) {
	h := &History{DB: openDB(t)}
	rec := bfNow.AddDate(0, 0, -3) // Oct 13 15:00
	if _, err := h.DB.Exec(`INSERT INTO history (ts, prices, amounts) VALUES (?, '{"SOL":1}', '{"w":{"SOL":1}}')`, rec.Unix()); err != nil {
		t.Fatal(err)
	}
	n, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, bfPrice)
	if err != nil || n != 5 { // Oct 8..12
		t.Fatalf("n=%d err=%v", n, err)
	}
	rows := bfRows(t, h)
	if len(rows) != 6 || !near(rows[rec.Unix()].Amounts["w"]["SOL"], 1) {
		t.Fatalf("rows=%v", rows)
	}
	if _, ok := rows[eod(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))]; ok {
		t.Fatal("wrote a day after the recorded row")
	}
}

func TestBackfillPriceFallback(t *testing.T) {
	h := &History{DB: openDB(t)}
	if _, err := h.Backfill(context.Background(), 2, bfNow, bfSnap(), nil, nil, bfPrice); err != nil {
		t.Fatal(err)
	}
	for _, r := range bfRows(t, h) {
		if !near(r.Prices["spl:X"], 2) {
			t.Fatalf("prices=%v", r.Prices)
		}
	}
}

func TestBackfillMakesSeriesStartEarlier(t *testing.T) {
	h := &History{DB: openDB(t)}
	snap := bfSnap()
	if _, err := h.Backfill(context.Background(), 7, bfNow, snap, nil, nil, bfPrice); err != nil {
		t.Fatal(err)
	}
	pts, err := h.Series(context.Background(), bfNow.AddDate(0, 0, -7), nil, &snap)
	if err != nil || len(pts) < 7 {
		t.Fatalf("pts=%d err=%v", len(pts), err)
	}
}

func TestBackfillRollbackRules(t *testing.T) {
	day := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	at := bfNow.AddDate(0, 0, -1) // Oct 15 15:00; rolled back from Oct 14 and older
	fee := func(a float64) *core.Leg { return &core.Leg{AssetKey: "SOL", Amount: -a} }
	cases := []struct {
		name string
		tx   core.Tx
		want float64 // SOL on Oct 14 (10 now)
	}{
		{"failed fee only", core.Tx{Status: core.TxFailed, Legs: []core.Leg{{AssetKey: "SOL", Amount: -4}}, Fee: fee(0.5)}, 10.5},
		{"stake zero-net, fee counted", core.Tx{Kind: core.TxStake, Legs: []core.Leg{{AssetKey: "SOL", Amount: -4}}, Fee: fee(0.5)}, 10.5},
		{"position zero-net, fee counted", core.Tx{Kind: core.TxPosition, Legs: []core.Leg{{AssetKey: "SOL", Amount: -4}}, Fee: fee(0.5)}, 10.5},
		{"send with fee", core.Tx{Legs: []core.Leg{{AssetKey: "SOL", Amount: -2}}, Fee: fee(0.5)}, 12.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &History{DB: openDB(t)}
			c.tx.SourceID, c.tx.Time = "w", at
			if _, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), []core.Tx{c.tx}, nil, bfPrice); err != nil {
				t.Fatal(err)
			}
			rows := bfRows(t, h)
			if got := rows[eod(day.AddDate(0, 0, -2))].Amounts["w"]["SOL"]; !near(got, c.want) {
				t.Fatalf("Oct 14 = %v, want %v", got, c.want)
			}
			if got := rows[eod(day.AddDate(0, 0, -1))].Amounts["w"]["SOL"]; !near(got, 10) {
				t.Fatalf("Oct 15 = %v, want 10", got)
			}
		})
	}
}

func TestBackfillMakesChangeValid(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 10, 16, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 16, 23, 30, 0, 0, time.UTC),
	} {
		h := &History{DB: openDB(t)}
		snap := bfSnap()
		snap.UpdatedAt = now
		if _, err := h.Backfill(context.Background(), 7, now, snap, nil, nil, bfPrice); err != nil {
			t.Fatal(err)
		}
		pts, err := h.Series(context.Background(), RangeStart("1W", now), nil, &snap)
		if err != nil {
			t.Fatal(err)
		}
		if c := ChangeFor("1W", pts, now); !c.Valid {
			t.Fatalf("now=%v: not valid, first=%v", now, pts[0].T)
		}
	}
}

func TestChangeForCoverageTolerance(t *testing.T) {
	now := bfNow
	at := func(d time.Duration) Change {
		return ChangeFor("1W", []Point{{T: now.Add(-d), V: 1}, {T: now, V: 2}}, now)
	}
	if !at(7*24*time.Hour - 20*time.Hour).Valid {
		t.Fatal("1W first point at now-7d+20h should be valid")
	}
	if at(7*24*time.Hour - 30*time.Hour).Valid {
		t.Fatal("1W first point at now-7d+30h should not be valid")
	}
	c := ChangeFor("1D", []Point{{T: now.Add(-21 * time.Hour), V: 1}, {T: now, V: 2}}, now)
	if c.Valid {
		t.Fatal("1D first point at now-1d+3h should not be valid")
	}
}

func TestBackfillCancelledWritesNothing(t *testing.T) {
	h := &History{DB: openDB(t)}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	price := func(c context.Context, key, mint string, d time.Time) (float64, bool, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return bfPrice(c, key, mint, d)
	}
	if _, err := h.Backfill(ctx, 7, bfNow, bfSnap(), nil, nil, price); err == nil {
		t.Fatal("want error")
	}
	if n := len(bfRows(t, h)); n != 0 {
		t.Fatalf("rows after cancel = %d", n)
	}
	n, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, bfPrice)
	if err != nil || n != 8 {
		t.Fatalf("rerun n=%d err=%v", n, err)
	}
}

func TestBackfillRejectsBadPrices(t *testing.T) {
	h := &History{DB: openDB(t)}
	bad := func(context.Context, string, string, time.Time) (float64, bool, error) { return math.NaN(), true, nil }
	if _, err := h.Backfill(context.Background(), 1, bfNow, bfSnap(), nil, nil, bad); err != nil {
		t.Fatal(err)
	}
	for _, r := range bfRows(t, h) {
		if !near(r.Prices["SOL"], 100) {
			t.Fatalf("prices=%v", r.Prices)
		}
	}
}

func TestBackfillPriceFnMayUseTheDB(t *testing.T) {
	h := &History{DB: openDB(t)}
	h.DB.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	price := func(c context.Context, key, mint string, d time.Time) (float64, bool, error) {
		var one int
		if err := h.DB.QueryRowContext(c, `SELECT 1`).Scan(&one); err != nil {
			return 0, false, nil
		}
		return bfPrice(c, key, mint, d)
	}
	n, err := h.Backfill(ctx, 7, bfNow, bfSnap(), nil, nil, price)
	if err != nil || n != 8 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestBackfillRefusesWalletNotSyncing(t *testing.T) {
	for _, st := range []Status{StatusError, StatusPending} {
		h := &History{DB: openDB(t)}
		snap := bfSnap()
		snap.Wallets = append(snap.Wallets, WalletView{ID: "cb", Label: "Invented Exchange", Status: st}) // no rows to roll back from
		send := core.Tx{ID: "s", SourceID: "cb", Time: bfNow.AddDate(0, 0, -1), Kind: core.TxSend, Status: core.TxConfirmed,
			Legs: []core.Leg{{AssetKey: "SOL", Amount: -3}}}
		_, err := h.Backfill(context.Background(), 7, bfNow, snap, []core.Tx{send}, nil, bfPrice)
		var ns *NotSyncedError
		if !errors.As(err, &ns) || ns.Label != "Invented Exchange" {
			t.Fatalf("%s: err = %v, want a NotSyncedError naming the wallet", st, err)
		}
		if n := len(bfRows(t, h)); n != 0 {
			t.Fatalf("%s: %d rows written without the wallet", st, n)
		}
	}
}

func TestBackfillPriceErrorWritesNothing(t *testing.T) {
	h := &History{DB: openDB(t)}
	boom := errors.New("upstream 503")
	price := func(_ context.Context, key, _ string, d time.Time) (float64, bool, error) {
		if key == "SOL" && d.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
			return 0, false, boom
		}
		return 50, true, nil
	}
	_, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, price)
	var pe *PriceError
	if !errors.As(err, &pe) || !errors.Is(err, boom) || pe.Asset != "SOL" {
		t.Fatalf("err = %v, want a PriceError for SOL", err)
	}
	if n := len(bfRows(t, h)); n != 0 {
		t.Fatalf("%d rows written with a fallback price", n)
	}
}

func TestBackfillLooksUpOnlyWritableDays(t *testing.T) {
	h := &History{DB: openDB(t)}
	rec := bfNow.AddDate(0, 0, -3) // Oct 13 15:00: only Oct 8..12 can be written
	if _, err := h.DB.Exec(`INSERT INTO history (ts, prices, amounts) VALUES (?, '{"SOL":1}', '{"w":{"SOL":1}}')`, rec.Unix()); err != nil {
		t.Fatal(err)
	}
	var asked []time.Time
	price := func(c context.Context, key, mint string, d time.Time) (float64, bool, error) {
		asked = append(asked, d)
		return bfPrice(c, key, mint, d)
	}
	if n, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), nil, nil, price); err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, d := range asked {
		if !d.Before(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("priced %s, a day that cannot be written", d.Format(time.DateOnly))
		}
	}

	// Nothing writable at all: no lookups.
	asked = nil
	if n, err := h.Backfill(context.Background(), 2, bfNow, bfSnap(), nil, nil, price); err != nil || n != 0 || len(asked) != 0 {
		t.Fatalf("n=%d err=%v lookups=%d", n, err, len(asked))
	}
}

func TestBackfillSkipsZeroAmounts(t *testing.T) {
	h := &History{DB: openDB(t)}
	// All 5 X arrived on Oct 15: on Oct 14 and before there was none.
	txs := []core.Tx{{SourceID: "w", Time: bfNow.AddDate(0, 0, -1), Legs: []core.Leg{{AssetKey: "spl:X", Mint: "X", Amount: 5}}}}
	var askedX []string
	price := func(c context.Context, key, mint string, d time.Time) (float64, bool, error) {
		if key == "spl:X" {
			askedX = append(askedX, d.Format(time.DateOnly))
		}
		return bfPrice(c, key, mint, d)
	}
	if _, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), txs, nil, price); err != nil {
		t.Fatal(err)
	}
	if len(askedX) != 1 || askedX[0] != "2026-10-15" {
		t.Errorf("X priced on %v, want only 2026-10-15", askedX)
	}
	if _, held := bfRows(t, h)[eod(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))].Amounts["w"]["spl:X"]; held {
		t.Error("a zero amount was written")
	}
}

func TestBackfillHonoursPositionTags(t *testing.T) {
	h := &History{DB: openDB(t)}
	// A send into the user's own position, tagged as a position move: SOL stays held.
	send := core.Tx{ID: "p", SourceID: "w", Time: bfNow.AddDate(0, 0, -1), Kind: core.TxSend, Legs: []core.Leg{{AssetKey: "SOL", Amount: -4}}}
	isPosition := func(id string) bool { return id == "p" }
	if _, err := h.Backfill(context.Background(), 7, bfNow, bfSnap(), []core.Tx{send}, isPosition, bfPrice); err != nil {
		t.Fatal(err)
	}
	if got := bfRows(t, h)[eod(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))].Amounts["w"]["SOL"]; !near(got, 10) {
		t.Errorf("Oct 14 SOL = %v, want 10 (a tagged position move keeps its units)", got)
	}
}
