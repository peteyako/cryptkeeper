package server

import (
	"context"
	"html"
	"log/slog"
	"maps"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/tax"
)

func nowAt(t time.Time) func(*Deps) { return func(d *Deps) { d.Now = func() time.Time { return t } } }

// priced gives tx's legs a USD value so only the movement issues remain.
func priced(tx core.Tx) core.Tx {
	for i := range tx.Legs {
		v := 10.0
		tx.Legs[i].USD = &v
	}
	return tx
}

func sendTx(id, src, asset string, amt float64, at time.Time) core.Tx {
	tx := reviewRecv(id, src, asset, -amt, at)
	tx.Kind = core.TxSend
	return priced(tx)
}

func TestCloseYearRequiresYearEnd(t *testing.T) {
	e := newEnv(t, "", nowAt(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)))
	e.upsertTxs(reviewRecv("a", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	out := html.UnescapeString(e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}}))
	if !strings.Contains(out, "2026 hasn't ended yet") {
		t.Errorf("out: %s", out)
	}
	if _, ok := e.taxYears.Get(2026); ok {
		t.Error("year must not be closed")
	}
}

func TestCloseYearWritesYear(t *testing.T) {
	e := newEnv(t, "", nowAt(time.Date(2027, 2, 1, 12, 0, 0, 0, time.UTC)))
	e.ref.Force(context.Background())
	e.upsertTxs(
		reviewRecv("a", "hot", "SOL", 5, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)),
		sendTx("b", "hot", "SOL", 1, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)),
		sendTx("c", "hot", "SOL", 1, time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)),
	)
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	y, ok := e.taxYears.Get(2026)
	if !ok || y.Source != "cryptkeep" || y.ClosedAt.IsZero() {
		t.Fatalf("year: %+v ok=%v", y, ok)
	}
	if len(y.Disposals) != 1 || !y.Disposals[0].Sold.Before(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("disposals: %+v", y.Disposals)
	}
	var held float64
	for _, l := range y.ClosingLots {
		held += l.Amount
	}
	if held != 4 {
		t.Errorf("closing lots hold %v, want 4: %+v", held, y.ClosingLots)
	}
	if p := e.review(); p.OpenYear != 2027 {
		t.Errorf("open year %d", p.OpenYear)
	}
}

func TestCloseYearRecordsOpenIssues(t *testing.T) {
	e := newEnv(t, "", nowAt(time.Date(2027, 2, 1, 12, 0, 0, 0, time.UTC)))
	e.ref.Force(context.Background())
	e.upsertTxs(
		priced(reviewRecv("a", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))),
		priced(reviewRecv("b", "hot", "SOL", 1, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))),
		priced(reviewRecv("c", "hot", "SOL", 1, time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC))),
	)
	out := html.UnescapeString(e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}}))
	if !strings.Contains(out, "Close 2026? 2 review items are still open. Closed years can't be edited.") {
		t.Errorf("confirmation: %s", out)
	}
	if _, ok := e.taxYears.Get(2026); ok {
		t.Fatal("first step must not close")
	}
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	if y, _ := e.taxYears.Get(2026); y.OpenIssues != 2 {
		t.Errorf("OpenIssues %d", y.OpenIssues)
	}
}

func TestCloseYearOrder(t *testing.T) {
	e := newEnv(t, "", nowAt(time.Date(2027, 2, 1, 12, 0, 0, 0, time.UTC)))
	e.ref.Force(context.Background())
	e.upsertTxs(reviewRecv("a", "hot", "SOL", 1, time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)))
	out := e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	if !strings.Contains(out, "close 2025 first") {
		t.Errorf("out: %s", out)
	}
}

func TestReopenLatestOnly(t *testing.T) {
	e := newEnv(t, "", nowAt(time.Date(2027, 2, 1, 12, 0, 0, 0, time.UTC)))
	e.closeYear(2025, tax.Summary{})
	e.closeYear(2026, tax.Summary{})
	out := e.reviewPost("/reports/tax/reopen", url.Values{"year": {"2025"}, "confirm": {"1"}})
	if !strings.Contains(out, "only 2026 can be reopened") {
		t.Errorf("out: %s", out)
	}
	if got := e.taxYears.Years(); len(got) != 2 {
		t.Fatalf("years %v", got)
	}
	e.reviewPost("/reports/tax/reopen", url.Values{"year": {"2026"}})
	if got := e.taxYears.Years(); len(got) != 2 {
		t.Fatal("first step must not reopen")
	}
	e.reviewPost("/reports/tax/reopen", url.Values{"year": {"2026"}, "confirm": {"1"}})
	if got := e.taxYears.Years(); len(got) != 1 || got[0] != 2025 {
		t.Fatalf("years %v", got)
	}
}

func at(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func transferPair(id string, from, to string, amt float64, sent, recv time.Time) []core.Tx {
	out := sendTx(id+"s", from, "SOL", amt, sent)
	out.PeerID = to + ":" + id + "r"
	in := priced(reviewRecv(id+"r", to, "SOL", amt, recv))
	in.PeerID = from + ":" + id + "s"
	return []core.Tx{out, in}
}

type lotKey struct{ wallet, asset string }

func lotTotals(lots []tax.Lot) map[lotKey][2]float64 {
	m := map[lotKey][2]float64{}
	for _, l := range lots {
		v := m[lotKey{l.Wallet, l.Asset}]
		m[lotKey{l.Wallet, l.Asset}] = [2]float64{v[0] + l.Amount, v[1] + l.CostUSD}
	}
	return m
}

func TestCloseTwoYearsMatchesOnePass(t *testing.T) {
	e := newEnv(t, "", nowAt(at(2028, 3, 1, 12, 0)))
	pos := core.Tx{ID: "hot:dep", SourceID: "hot", ExternalID: "dep", Time: at(2026, 5, 1, 0, 0), Kind: core.TxPosition, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -4}}}
	txs := []core.Tx{priced(reviewRecv("a", "hot", "SOL", 10, at(2026, 3, 1, 0, 0))), pos,
		priced(reviewRecv("b", "hot", "SOL", 3, at(2027, 2, 1, 0, 0))), sendTx("c", "hot", "SOL", 1, at(2027, 3, 1, 0, 0))}
	txs = append(txs, transferPair("x", "hot", "cold", 2, at(2026, 12, 31, 23, 30), at(2027, 1, 1, 0, 30))...)
	e.upsertTxs(txs...)
	manual := []tax.Lot{{Wallet: "hot", Asset: "SOL", Amount: 2, CostUSD: 20, Acquired: at(2026, 2, 1, 0, 0)}}
	wo := []tax.WriteOff{{Wallet: "hot", Asset: "SOL", Amount: 1, Date: at(2027, 6, 1, 0, 0)}}
	if err := e.taxSet.Update(func(st *tax.Settings) { st.ManualLots, st.WriteOffs = manual, wo }); err != nil {
		t.Fatal(err)
	}
	e.ref.Force(context.Background())
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2027"}, "confirm": {"1"}})
	y26, ok26 := e.taxYears.Get(2026)
	y27, ok27 := e.taxYears.Get(2027)
	if !ok26 || !ok27 {
		t.Fatalf("closed: %v", e.taxYears.Years())
	}
	for _, d := range y27.Disposals {
		if d.Sold.Before(at(2027, 1, 1, 0, 0)) {
			t.Errorf("2027 holds 2026 disposal %+v", d)
		}
	}
	all, _ := e.act.All(context.Background(), nil)
	one := tax.Compute(tax.Input{Loc: time.UTC, Manual: manual, WriteOffs: wo, Txs: all, To: at(2028, 1, 1, 0, 0)})
	if len(one.Lots) == 0 || len(one.Parked) == 0 {
		t.Fatalf("one pass is empty: %+v", one)
	}
	if got, want := lotTotals(y27.ClosingLots), lotTotals(one.Lots); !maps.Equal(got, want) {
		t.Errorf("closing lots %v, one pass %v", got, want)
	}
	if len(y26.ClosingParked) == 0 {
		t.Error("2026 should carry parked balances")
	}
	if len(y27.ClosingParked) != len(one.Parked) || (len(one.Parked) > 0 && y27.ClosingParked[0].Amount != one.Parked[0].Amount) {
		t.Errorf("parked %v, one pass %v", y27.ClosingParked, one.Parked)
	}
}

func TestCloseTwiceIsAlreadyClosed(t *testing.T) {
	e := newEnv(t, "", nowAt(at(2028, 3, 1, 12, 0)))
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 1, at(2026, 3, 1, 0, 0))), priced(reviewRecv("b", "hot", "SOL", 1, at(2027, 3, 1, 0, 0))))
	e.ref.Force(context.Background())
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	first, _ := e.taxYears.Get(2026)
	out := html.UnescapeString(e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}}))
	if !strings.Contains(out, "2026 is already closed") {
		t.Errorf("out: %.300s", out)
	}
	second, _ := e.taxYears.Get(2026)
	if !reflect.DeepEqual(first, second) || len(e.taxYears.Years()) != 1 {
		t.Error("closed year changed")
	}
}

func TestCloseRefusedWhileNotReady(t *testing.T) {
	e := newEnv(t, "", nowAt(at(2027, 2, 1, 12, 0)))
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 1, at(2026, 3, 1, 0, 0))))
	out := e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	if !strings.Contains(out, "Still syncing; try again in a minute.") || len(e.taxYears.Years()) != 0 {
		t.Errorf("out: %.300s", out)
	}
}

func TestCloseYearInNewYorkTimezone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	e := newEnv(t, "", nowAt(at(2027, 2, 1, 12, 0)), func(d *Deps) { d.TaxLoc = ny })
	e.upsertTxs(
		priced(reviewRecv("a", "hot", "SOL", 5, at(2026, 3, 1, 0, 0))),
		sendTx("in26", "hot", "SOL", 1, at(2027, 1, 1, 3, 0)),
		sendTx("in27", "hot", "SOL", 1, at(2027, 1, 1, 6, 0)),
	)
	e.ref.Force(context.Background())
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	y, ok := e.taxYears.Get(2026)
	if !ok || len(y.Disposals) != 1 {
		t.Fatalf("2026 disposals: %+v ok=%v", y.Disposals, ok)
	}
}

func TestUndatedManualLotWarning(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2025, tax.Summary{})
	if err := e.taxSet.Update(func(st *tax.Settings) {
		st.ManualLots = []tax.Lot{{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 5}}
	}); err != nil {
		t.Fatal(err)
	}
	p := e.review()
	if p.UndatedLots != 1 {
		t.Fatalf("undated %d", p.UndatedLots)
	}
	if html := renderReview(t, p); !strings.Contains(html, "1 manual lot(s) without a date are ignored once a year is closed") {
		t.Errorf("warning missing")
	}
}

func TestOutsideYearGuard(t *testing.T) {
	res := tax.Result{Disposals: []tax.Disposal{{Sold: at(2027, 1, 1, 0, 0)}}}
	if !outsideYear(res, 2026, time.UTC) {
		t.Error("2027 row must be outside 2026")
	}
	res = tax.Result{Income: []tax.Income{{Date: at(2026, 6, 1, 0, 0)}}}
	if outsideYear(res, 2026, time.UTC) {
		t.Error("2026 row must be inside")
	}
}

// taxInput is the engine input the server assembles now.
func (e *env) taxInput() tax.Input {
	e.t.Helper()
	d := e.deps
	d.Now, d.Log = time.Now, slog.Default()
	in, _, err := (&Server{Deps: d}).taxInput(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

// Closing a year saves the transfer halves after it whose pair the close processed; the
// next input lists them as PairedBeforeFrom, so the open year skips them silently. A
// Koinly year carries none.
func TestCloseSavesCarriedPeers(t *testing.T) {
	k := newEnv(t, "")
	k.closeYear(2025, tax.Summary{})
	if in := k.taxInput(); in.PairedBeforeFrom != nil {
		t.Errorf("after a Koinly year PairedBeforeFrom = %v, want none", in.PairedBeforeFrom)
	}

	e := newEnv(t, "", nowAt(at(2027, 2, 1, 12, 0)))
	e.ref.Force(context.Background())
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 5, at(2026, 3, 1, 0, 0))))
	e.upsertTxs(transferPair("x", "hot", "cold", 2, at(2026, 12, 31, 23, 30), at(2027, 1, 1, 0, 30))...)
	if in := e.taxInput(); in.PairedBeforeFrom != nil {
		t.Errorf("with no closed year PairedBeforeFrom = %v, want none", in.PairedBeforeFrom)
	}
	e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	y, ok := e.taxYears.Get(2026)
	if !ok || !reflect.DeepEqual(y.CarriedPeers, []string{"cold:xr"}) {
		t.Fatalf("closed 2026: CarriedPeers = %v ok=%v, want [cold:xr]", y.CarriedPeers, ok)
	}
	in := e.taxInput()
	if !reflect.DeepEqual(in.PairedBeforeFrom, map[string]bool{"cold:xr": true}) {
		t.Errorf("PairedBeforeFrom = %v, want cold:xr", in.PairedBeforeFrom)
	}
	res := tax.Compute(in)
	for _, is := range res.Issues {
		if is.Kind == tax.IssueTransferUnpaired {
			t.Errorf("open year issue %+v, want the carried half skipped", is)
		}
	}
	if got := lotTotals(res.Lots)[lotKey{"cold", "SOL"}]; got[0] != 2 {
		t.Errorf("cold holds %v SOL, want the 2 moved by the close", got[0])
	}
}

// Once a year is closed, manual lots dated before the open year are ignored (the closed
// year holds them): the panel says how many, next to the undated ones.
func TestEarlyManualLotWarning(t *testing.T) {
	e := newEnv(t, "")
	if err := e.taxSet.Update(func(st *tax.Settings) {
		st.ManualLots = []tax.Lot{
			{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 5, Acquired: at(2025, 6, 1, 0, 0)},
			{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 5, Acquired: at(2024, 6, 1, 0, 0)},
			{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 5, Acquired: at(2026, 1, 1, 0, 0)}, // the open year's first instant
			{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 5},
		}
	}); err != nil {
		t.Fatal(err)
	}
	if p := e.review(); p.EarlyLots != 0 {
		t.Errorf("no closed year: EarlyLots %d, want 0", p.EarlyLots)
	}
	e.closeYear(2025, tax.Summary{})
	p := e.review()
	if p.EarlyLots != 2 || p.UndatedLots != 1 {
		t.Fatalf("EarlyLots %d UndatedLots %d, want 2 and 1", p.EarlyLots, p.UndatedLots)
	}
	page := renderReview(t, p)
	if !strings.Contains(page, "2 manual lot(s) dated before Jan 1, 2026 are ignored — they belong to a closed year") {
		t.Errorf("early-lot warning missing: %.600s", page)
	}
	if !strings.Contains(page, "1 manual lot(s) without a date are ignored once a year is closed") {
		t.Errorf("undated warning missing")
	}
}
