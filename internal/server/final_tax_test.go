package server

import (
	"context"
	"errors"
	"html"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/tax/irs"
	"crypto-portfolio/internal/tax/report"
	"crypto-portfolio/web/templates"
)

// ftTx is a confirmed invented tx with one leg of asset worth usd (nil = unpriced).
func ftTx(id, src string, kind core.TxKind, asset string, amt float64, usdv *float64, at time.Time) core.Tx {
	return core.Tx{ID: src + ":" + id, SourceID: src, ExternalID: id, Time: at, Kind: kind, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: asset, Symbol: "SOL", Amount: amt, USD: usdv}}}
}

// ftUnpaired is a transfer half whose peer never reached the activity store.
func ftUnpaired(id, src string, amt float64, at time.Time) core.Tx {
	tx := ftTx(id, src, core.TxTransfer, "SOL", amt, usd(10), at)
	tx.PeerID = "gone:" + id
	return tx
}

// TestFtHiddenAssetsSplit: only configured hidden mints are spam; other hidden rows no
// wallet shows are quiet, not dropped.
func TestFtHiddenAssetsSplit(t *testing.T) {
	row := func(key string) portfolio.HoldingRow {
		return portfolio.HoldingRow{Holding: core.Holding{AssetKey: key, Symbol: key}}
	}
	snap := portfolio.Snapshot{Wallets: []portfolio.WalletView{
		{ID: "phantom", Rows: []portfolio.HoldingRow{row("SOL")}, HiddenRows: []portfolio.HoldingRow{row("spl:Meme1"), row("USDC"), row("spl:Mint9")}},
	}}
	snap.Assets = []portfolio.AssetRow{{AssetKey: "SOL"}, {AssetKey: "USDC"}}
	spam := spamSet([]string{"Mint9", ""})
	quiet := quietSet(snap, spam)
	if len(spam) != 1 || !spam["spl:Mint9"] {
		t.Errorf("spam = %v, want only the hidden mint", spam)
	}
	if len(quiet) != 1 || !quiet["spl:Meme1"] {
		t.Errorf("quiet = %v, want the auto-hidden token only", quiet)
	}
}

// TestFtUnpairedHalfTaggable: the review queue offers tags for an unpaired transfer half,
// the tag saves, and the tagged half keeps its controls; a paired half refuses tags.
func TestFtUnpairedHalfTaggable(t *testing.T) {
	e := newEnv(t, "")
	half := ftUnpaired("x", "hot", -2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	pair := transferPair("p", "hot", "cold", 1, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 1, 0, 0, 0, time.UTC))
	e.upsertTxs(append(pair, half)...)
	g := reviewGroupOf(e.review(), tax.IssueTransferUnpaired, "SOL")
	if g == nil || len(g.Items) != 1 || !slices.Equal(g.Items[0].TagOptions, tagStrings(tax.SendTags())) {
		t.Fatalf("unpaired group: %+v", g)
	}
	out := html.UnescapeString(e.reviewPost("/reports/tax/tag", url.Values{"tx": {half.ID}, "asset": {"SOL"}, "tag": {"own-wallet"}}))
	if strings.Contains(out, "doesn&#39;t apply") || strings.Contains(out, "doesn't apply") {
		t.Fatalf("tag refused: %.400s", within(out, `role="alert"`, 300))
	}
	if got := e.taxSet.Get().Tags[half.ID].Tag; got != tax.TagOwnWallet {
		t.Fatalf("tag = %q", got)
	}
	p := e.review()
	if reviewGroupOf(p, tax.IssueTransferUnpaired, "SOL") != nil {
		t.Error("the tagged half is still unpaired")
	}
	tagged := reviewGroupOf(p, templates.ReviewTagged, "")
	if tagged == nil || len(tagged.Items) != 1 || len(tagged.Items[0].TagOptions) == 0 {
		t.Errorf("tagged group: %+v", tagged)
	}
	out = html.UnescapeString(e.reviewPost("/reports/tax/tag", url.Values{"tx": {pair[0].ID}, "asset": {"SOL"}, "tag": {"gift"}}))
	if !strings.Contains(out, "tag gift doesn't apply") {
		t.Errorf("tag on a paired half: %.400s", within(out, `role="alert"`, 300))
	}
}

// TestFtCPASummaryMatchesScheduleD: the CPA report's summary is built from the cent-rounded
// rows, so it shows the Schedule D total, never a float sum a cent off.
func TestFtCPASummaryMatchesScheduleD(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.upsertTxs(ftTx("in", "hot", core.TxReceive, "SOL", 3, usd(30), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)))
	for i, id := range []string{"s1", "s2", "s3"} {
		e.upsertTxs(ftTx(id, "hot", core.TxSend, "SOL", -1, usd(-20.004), time.Date(2026, 3, 1+i, 0, 0, 0, 0, time.UTC)))
	}
	_, body := e.do("GET", "/reports/tax/2026/report", nil, nil)
	summary := within(body, "<h2>Summary</h2>", 1200)
	if !strings.Contains(summary, "30.00") || strings.Contains(body, "30.01") {
		t.Errorf("summary must show the Schedule D 30.00:\n%s", summary)
	}
	if !strings.Contains(body, "Gain/loss on fee disposals (included above)") {
		t.Error("fees label")
	}
}

// TestFtZoneChangeAfterClose: a year closed in UTC ends at midnight UTC; with the tax zone
// now New York the open year starts there (no tx counted twice or dropped), the Tax section
// warns, and the next close records its own zone.
func TestFtZoneChangeAfterClose(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	e := newEnv(t, "", nowAt(at(2027, 2, 1, 12, 0)), func(d *Deps) { d.TaxLoc = ny })
	if err := e.taxYears.Save(tax.Year{Year: 2025, Source: "cryptkeeper", Timezone: "UTC", ClosedAt: at(2026, 1, 5, 0, 0),
		ClosingLots: []tax.Lot{solLot("hot", 5, 500)}}); err != nil {
		t.Fatal(err)
	}
	e.upsertTxs(
		sendTx("closed", "hot", "SOL", 1, at(2025, 12, 31, 23, 0)), // in 2025 (UTC), already frozen
		sendTx("boundary", "hot", "SOL", 1, at(2026, 1, 1, 2, 0)),  // Dec 31 21:00 in New York
	)
	e.ref.Force(context.Background())
	s := e.taxServer()
	rep, err := s.yearReport(context.Background(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].TxID != "hot:boundary" {
		t.Fatalf("2026 rows %+v, want the boundary sale only", rep.Rows)
	}
	if p := s.taxPage(context.Background(), 2026, "gains", 1, nil); !strings.Contains(p.ZoneNote, "UTC") || !strings.Contains(p.ZoneNote, "America/New_York") {
		t.Errorf("zone note %q", p.ZoneNote)
	}
	out := html.UnescapeString(e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}}))
	y, ok := e.taxYears.Get(2026)
	if !ok {
		t.Fatalf("2026 not closed: %.400s", within(out, `role="alert"`, 400))
	}
	if y.Timezone != "America/New_York" || len(y.Disposals) != 1 || y.Disposals[0].TxID != "hot:boundary" {
		t.Errorf("2026: zone %q disposals %+v", y.Timezone, y.Disposals)
	}
}

// TestFtCloseRefusedWhilePending: a pending tx in the year blocks the close and is named.
func TestFtCloseRefusedWhilePending(t *testing.T) {
	e := newEnv(t, "", nowAt(at(2027, 2, 1, 12, 0)))
	pending := sendTx("p", "hot", "SOL", 1, at(2026, 6, 1, 0, 0))
	pending.Status = core.TxPending
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 5, at(2026, 3, 1, 0, 0))), pending)
	e.ref.Force(context.Background())
	out := html.UnescapeString(e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}}))
	if _, ok := e.taxYears.Get(2026); ok {
		t.Fatal("closed with a pending tx")
	}
	if !strings.Contains(out, "1 transaction is still pending") || !strings.Contains(out, "hot:p") {
		t.Errorf("refusal: %.500s", within(out, `role="alert"`, 500))
	}
}

// TestFtUndatedLotReviewed: with nothing closed, an undated manual lot is a review item.
func TestFtUndatedLotReviewed(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 1, at(2026, 3, 1, 0, 0))))
	if err := e.taxSet.Update(func(st *tax.Settings) {
		st.ManualLots = []tax.Lot{{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 5}}
	}); err != nil {
		t.Fatal(err)
	}
	g := reviewGroupOf(e.review(), tax.IssueUndatedLot, "SOL")
	if g == nil || g.Title != "Manual lots without a date" || g.Count != 1 {
		t.Errorf("undated group: %+v", g)
	}
}

// TestFtTransactionsCSVUsesTaxActivity: a CSV-imported trade the live API also synced is
// listed once, and hidden-mint and spam-tagged txs are left out, as the engine does.
func TestFtTransactionsCSVUsesTaxActivity(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Cfg.Solana.HideMints = []string{"MintHidden"} })
	if _, err := e.reg.Upsert(sources.Def{ID: "kr", Type: sources.TypeKraken, Label: "Kraken"}); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	live := ftTx("T1", "kr", core.TxTrade, "SOL", 2, usd(300), when)
	csvDup := ftTx("T1", "kraken-csv", core.TxTrade, "SOL", 2, usd(300), when)
	hidden := ftTx("h", "hot", core.TxReceive, "spl:MintHidden", 9, nil, when.Add(time.Hour))
	tagged := ftTx("sp", "hot", core.TxReceive, "SOL", 1, usd(1), when.Add(2*time.Hour))
	e.upsertTxs(live, csvDup, hidden, tagged)
	if err := e.taxSet.Update(func(st *tax.Settings) { st.Tags = map[string]tax.TagEntry{tagged.ID: {Tag: tax.TagSpam}} }); err != nil {
		t.Fatal(err)
	}
	_, body := e.do("GET", "/reports/tax/2026/transactions.csv", nil, nil)
	rows := strings.Split(strings.TrimSpace(body), "\n")[3:]
	if len(rows) != 1 || !strings.Contains(rows[0], "Kraken") {
		t.Errorf("rows %q, want the one Kraken trade", rows)
	}
}

// TestFtCSVImportCountsAsExchangeWallet: with exactly one live Kraken source, kraken-csv
// rows are its history; with two, they stay apart.
func TestFtCSVImportCountsAsExchangeWallet(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.reg.Upsert(sources.Def{ID: "kr", Type: sources.TypeKraken, Label: "Kraken"}); err != nil {
		t.Fatal(err)
	}
	old := ftTx("old", "kraken-csv", core.TxTrade, "SOL", 2, usd(300), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	e.upsertTxs(old)
	got, err := e.taxServer().taxActivity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SourceID != "kr" || got[0].ID != old.ID {
		t.Fatalf("one Kraken source: %+v", got)
	}
	if _, err := e.reg.Upsert(sources.Def{ID: "kr2", Type: sources.TypeKraken, Label: "Kraken 2"}); err != nil {
		t.Fatal(err)
	}
	got, _ = e.taxServer().taxActivity(context.Background())
	if len(got) != 1 || got[0].SourceID != "kraken-csv" {
		t.Errorf("two Kraken sources: %+v", got)
	}
}

// TestFtTaxTablesPrivate: the Tax tables' figures are masked by privacy mode.
func TestFtTaxTablesPrivate(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.saleYear2026()
	s := e.taxServer()
	for _, tab := range []string{"gains", "holdings"} {
		var b strings.Builder
		if err := templates.TaxSection(s.taxPage(context.Background(), 2026, tab, 1, nil)).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		tbody := within(b.String(), "<tbody>", 2000)
		if !strings.Contains(tbody, `<td class="num"><span class="fig">`) {
			t.Errorf("%s figures are not masked:\n%s", tab, tbody)
		}
	}
}

// TestFtKoinlyAcceptSavesYearFirst: when the year can't be saved, no mapping is written; a
// successful accept records the zone and a UTC ClosedAt.
func TestFtKoinlyAcceptSavesYearFirst(t *testing.T) {
	e := newEnv(t, "")
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"), fixture(t, "transactions.csv"))
	e.koinlyPost("/reports/koinly/map", url.Values{"w.test wallet": {"untracked"}, "a.BTC (Bitcoin)": {"BTC"}})
	block := filepath.Join(e.dataDir, "tax") // a file where the year directory goes
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := e.koinlyPost("/reports/koinly/accept", nil)
	if !strings.Contains(out, "Couldn&#39;t close 2025") && !strings.Contains(out, "Couldn't close 2025") {
		t.Fatalf("accept: %.400s", out)
	}
	if st := e.taxSet.Get(); len(st.WalletMap) != 0 || len(st.AssetMap) != 0 {
		t.Errorf("mappings saved although the year was not: %+v", st)
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if out := e.koinlyPost("/reports/koinly/accept", nil); !strings.Contains(out, "window.location") {
		t.Fatalf("accept: %.400s", out)
	}
	y, ok := e.taxYears.Get(2025)
	if !ok || y.Timezone != "UTC" || y.ClosedAt.Location() != time.UTC {
		t.Errorf("year %v: zone %q closedAt %v", ok, y.Timezone, y.ClosedAt)
	}
	if st := e.taxSet.Get(); st.AssetMap["BTC (Bitcoin)"] != "BTC" {
		t.Errorf("mappings: %+v", st.AssetMap)
	}
}

// ftWriteOff saves one write-off and returns it.
func ftWriteOff(e *env) tax.WriteOff {
	wo := tax.WriteOff{Wallet: "hot", Asset: "SOL", Amount: 1.5, Date: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	if err := e.taxSet.Update(func(st *tax.Settings) { st.WriteOffs = []tax.WriteOff{wo} }); err != nil {
		e.t.Fatal(err)
	}
	return wo
}

// TestFtWriteOffDeleteChecksIdentity: the delete names the write-off it means and asks
// first; a list that changed underneath is refused.
func TestFtWriteOffDeleteChecksIdentity(t *testing.T) {
	e := newEnv(t, "")
	wo := ftWriteOff(e)
	page := renderReview(t, e.review())
	if !strings.Contains(page, "confirm(") || !strings.Contains(page, `name="at" value="2026-05-01T12:00:00Z"`) || !strings.Contains(page, `name="amount" value="1.5"`) {
		t.Errorf("delete form:\n%s", within(page, "Write-offs", 1500))
	}
	ident := url.Values{"wallet": {wo.Wallet}, "asset": {wo.Asset}, "at": {"2026-05-01T12:00:00Z"}, "amount": {"2"}}
	out := html.UnescapeString(e.reviewPost("/reports/tax/writeoff/0/delete", ident))
	if !strings.Contains(out, "The write-offs changed; reload and try again.") || len(e.taxSet.Get().WriteOffs) != 1 {
		t.Errorf("mismatch not refused: %.300s", within(out, `role="alert"`, 300))
	}
	ident.Set("amount", "1.5")
	e.reviewPost("/reports/tax/writeoff/0/delete", ident)
	if n := len(e.taxSet.Get().WriteOffs); n != 0 {
		t.Errorf("write-offs left %d", n)
	}
}

// TestFtKoinlyYearHidesTransactionsLink: the Tax section offers no transaction history for a
// Koinly year, and every year shows the TXF caveat.
func TestFtKoinlyYearHidesTransactionsLink(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	s := e.taxServer()
	render := func(year int) string {
		var b strings.Builder
		if err := templates.TaxSection(s.taxPage(context.Background(), year, "gains", 1, nil)).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if k := render(2025); strings.Contains(k, "transactions.csv") || !strings.Contains(k, "TXF has no digital-asset codes") {
		t.Errorf("Koinly year downloads:\n%s", within(k, "tax__downloads", 1500))
	}
	if c := render(2026); !strings.Contains(c, "/reports/tax/2026/transactions.csv") {
		t.Error("CryptKeeper year lost its transactions link")
	}
}

// TestFtTagSelectLabelled: the per-row tag select names its asset and date.
func TestFtTagSelectLabelled(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(reviewRecv("1", "hot", "spl:A", 1, time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)))
	page := renderReview(t, e.review())
	if !strings.Contains(page, `aria-label="Tag for TKN on 2026-03-01 09:30"`) {
		t.Errorf("select label:\n%s", within(page, `name="tag"`, 300))
	}
}

// TestFtTokenNamesMemoized: a mint's token_meta lookup is made once per render.
func TestFtTokenNamesMemoized(t *testing.T) {
	e := newEnv(t, "")
	mint := "MintMemo1111"
	if _, err := e.db.Exec(`INSERT INTO token_meta (mint, json) VALUES (?, ?)`, mint, `{"Symbol":"MEMO","Name":"Memo Token"}`); err != nil {
		t.Skip("token_meta:", err)
	}
	syms := &taxSymbols{s: e.taxServer(), ctx: context.Background()}
	if got := syms.get("spl:" + mint); got.symbol != "MEMO" {
		t.Fatalf("first lookup %+v", got)
	}
	if _, err := e.db.Exec(`DELETE FROM token_meta WHERE mint = ?`, mint); err != nil {
		t.Fatal(err)
	}
	if got := syms.get("spl:" + mint); got.symbol != "MEMO" || got.name != "Memo Token" {
		t.Errorf("second lookup went back to the database: %+v", got)
	}
}

// TestFtZipSurvivesIRSFailure: a failed IRS form fill leaves the PDF out of all.zip and
// explains why in NOTE.txt; every other file is still there.
func TestFtZipSurvivesIRSFailure(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.saleYear2026()
	orig := fillIRSForms
	fillIRSForms = func(context.Context, report.YearReport, io.Writer, irs.Options) error {
		return errors.New("invented form failure")
	}
	t.Cleanup(func() { fillIRSForms = orig })
	resp, body := e.do("GET", "/reports/tax/2026/all.zip", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %.200s", resp.StatusCode, body)
	}
	files, order := zipNames(t, body)
	if _, ok := files["cryptkeeper_2026_irs_reports_form_8949_schedule_d.pdf"]; ok {
		t.Error("the failed PDF is in the zip")
	}
	note := files["cryptkeeper_2026_NOTE.txt"]
	if !strings.Contains(note, "IRS forms") || !strings.Contains(note, "invented form failure") {
		t.Errorf("NOTE.txt = %q", note)
	}
	if len(order) != 9 || files["cryptkeeper_2026_capital_gains_report.csv"] == "" {
		t.Errorf("entries %v", order)
	}
}

// TestFtSpamOnUnpairedHalfCanBeRetagged: spamming an unpaired half keeps it listed as
// unpaired, so its tag controls stay and the tag can be changed.
func TestFtSpamOnUnpairedHalfCanBeRetagged(t *testing.T) {
	e := newEnv(t, "")
	half := ftUnpaired("x", "hot", -2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	e.upsertTxs(half)
	e.reviewPost("/reports/tax/tag", url.Values{"tx": {half.ID}, "asset": {"SOL"}, "tag": {"spam"}})
	tagged := reviewGroupOf(e.review(), templates.ReviewTagged, "")
	if tagged == nil || len(tagged.Items) != 1 || len(tagged.Items[0].TagOptions) == 0 {
		t.Fatalf("spam-tagged half shows no tag controls: %+v", tagged)
	}
	out := html.UnescapeString(e.reviewPost("/reports/tax/tag", url.Values{"tx": {half.ID}, "asset": {"SOL"}, "tag": {"own-wallet"}}))
	if strings.Contains(out, "doesn't apply") {
		t.Fatalf("spam -> own-wallet refused: %.300s", within(out, `role="alert"`, 300))
	}
	if got := e.taxSet.Get().Tags[half.ID].Tag; got != tax.TagOwnWallet {
		t.Errorf("tag = %q", got)
	}
}

// TestFtOpeningFromCSVWalletFollowsTheExchange: lots and parked balances a closed year left
// in kraken-csv move to the one live Kraken source, as the CSV txs do.
func TestFtOpeningFromCSVWalletFollowsTheExchange(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	if _, err := e.reg.Upsert(sources.Def{ID: "kr", Type: sources.TypeKraken, Label: "Kraken"}); err != nil {
		t.Fatal(err)
	}
	y := tax.Year{Year: 2025, Source: "cryptkeeper", ClosedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		ClosingLots:   []tax.Lot{{Wallet: "kraken-csv", Asset: "SOL", Amount: 2, CostUSD: 40, Acquired: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)}},
		ClosingParked: []tax.Parked{{Wallet: "kraken-csv", Asset: "SOL", Amount: 1}}}
	if err := e.taxYears.Save(y); err != nil {
		t.Fatal(err)
	}
	in, _, err := e.taxServer().taxInput(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Opening) != 1 || in.Opening[0].Wallet != "kr" || len(in.OpeningParked) != 1 || in.OpeningParked[0].Wallet != "kr" {
		t.Errorf("opening %+v parked %+v", in.Opening, in.OpeningParked)
	}
	if y2, _ := e.taxYears.Get(2025); y2.ClosingLots[0].Wallet != "kraken-csv" {
		t.Errorf("the stored year was modified: %+v", y2.ClosingLots)
	}
}

// TestFtWithoutSpamKeepsPairedHalfWithLeftoverTag: a spam tag left on a half whose pair is
// intact is ignored by the engine, so the transaction history keeps the pair.
func TestFtWithoutSpamKeepsPairedHalfWithLeftoverTag(t *testing.T) {
	e := newEnv(t, "")
	pair := transferPair("p", "hot", "cold", 1, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 1, 0, 0, 0, time.UTC))
	half := ftUnpaired("x", "hot", -2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	e.taxSet.Update(func(v *tax.Settings) {
		v.Tags = map[string]tax.TagEntry{pair[0].ID: {Tag: tax.TagSpam}, half.ID: {Tag: tax.TagSpam}}
	})
	got := e.taxServer().withoutSpam(append(pair, half))
	if len(got) != 2 {
		t.Errorf("kept %d txs, want the pair only: %+v", len(got), got)
	}
}
