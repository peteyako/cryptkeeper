package server

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/tax"
)

// reTags matches HTML tags, to read a fragment's text.
var reTags = regexp.MustCompile(`<[^>]*>`)

// csvFile is one uploaded file: its name and content.
type csvFile struct {
	name    string
	content []byte
}

// fixture reads one of the synthetic Koinly exports of internal/tax/koinly/testdata.
func fixture(t *testing.T, name string) csvFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "tax", "koinly", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return csvFile{name, raw}
}

// koinlyUpload posts files to the wizard's upload, each as a "file" field, and returns
// the SSE response.
func (e *env) koinlyUpload(files ...csvFile) string {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, _ := mw.CreateFormFile("file", f.name)
		fw.Write(f.content)
	}
	mw.Close()
	_, out := e.do("POST", "/reports/koinly/upload", &buf, ds(map[string]string{"Content-Type": mw.FormDataContentType()}))
	return out
}

func (e *env) koinlyPost(path string, vals url.Values) string {
	e.t.Helper()
	var body io.Reader
	hdr := ds(nil)
	if vals != nil {
		body, hdr = form(vals)
	}
	_, out := e.do("POST", path, body, hdr)
	return out
}

func (e *env) draftExists() bool {
	e.t.Helper()
	var v map[string]any
	ok, err := e.db.GetJSON(context.Background(), koinlyDraftKey, &v)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func TestKoinlyUploadDetectsFiles(t *testing.T) {
	e := newEnv(t, "")
	out := e.koinlyUpload(fixture(t, "holdings.csv"), fixture(t, "transactions.csv"))
	if !strings.Contains(out, `id="koinly-wizard"`) {
		t.Fatalf("upload must patch #koinly-wizard: %.400s", out)
	}
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	for _, want := range []string{"holdings.csv", "transactions.csv", `name="w.test wallet"`,
		`name="a.BTC (Bitcoin)"`, `name="a.TNFT#42 (TestPass)"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// One wallet mapping row per Koinly wallet name: the history names one wallet.
	if n := strings.Count(page, `name="w.`); n != 1 {
		t.Errorf("%d wallet rows, want 1", n)
	}
	// A later upload adds to the draft: the gains report names two more wallets.
	e.koinlyUpload(fixture(t, "gains.csv"))
	_, page = e.do("GET", "/reports/koinly", nil, nil)
	for _, want := range []string{"gains.csv", `name="w.Cold"`, `name="w.Test Wallet A"`, `name="w.test wallet"`} {
		if !strings.Contains(page, want) {
			t.Errorf("after the gains upload, page lacks %q", want)
		}
	}
	if n := strings.Count(page, `name="w.`); n != 3 {
		t.Errorf("%d wallet rows after the gains upload, want 3", n)
	}
	// A report uploaded again replaces the earlier one of its kind, name included.
	e.koinlyUpload(csvFile{"holdings-v2.csv", fixture(t, "holdings.csv").content})
	_, page = e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(page, ">holdings-v2.csv<") || strings.Contains(page, ">holdings.csv<") || !strings.Contains(page, ">transactions.csv<") {
		t.Errorf("file names after replacing the holdings report:\n%s", within(page, `class="koinly-files"`, 600))
	}
	// Without a draft the page is just the upload step.
	e.koinlyPost("/reports/koinly/discard", nil)
	if e.draftExists() {
		t.Fatal("discard must delete the draft")
	}
	_, page = e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(page, `id="koinly-upload"`) || strings.Contains(page, `name="w.`) {
		t.Error("after discard the page shows only the upload step")
	}
}

func TestKoinlyUploadRejectsMismatchedYears(t *testing.T) {
	e := newEnv(t, "")
	gains2024 := []byte(strings.Replace(string(fixture(t, "gains.csv").content), "report 2025", "report 2024", 1))
	out := e.koinlyUpload(fixture(t, "holdings.csv"), csvFile{"gains-2024.csv", gains2024})
	if !strings.Contains(out, "2024") || !strings.Contains(out, "2025") || e.draftExists() {
		t.Fatalf("mismatched years must be refused with nothing saved: %.600s", out)
	}
}

func TestKoinlyMapPersistsDraft(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeSolana, Label: "Phantom", Address: "SoLFakeAddr1111"})
	e.koinlyUpload(fixture(t, "holdings.csv"), fixture(t, "transactions.csv"))
	out := e.koinlyPost("/reports/koinly/map", url.Values{
		"w.test wallet": {"sol"}, "a.BTC (Bitcoin)": {"BTC"}, "a.TNFT#42 (TestPass)": {""},
	})
	if !strings.Contains(out, `id="koinly-wizard"`) {
		t.Fatalf("map must patch #koinly-wizard: %.400s", out)
	}
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(page, `<option value="sol" selected>Phantom (Solana)</option>`) {
		t.Errorf("sol not selected for the Koinly wallet:\n%s", within(page, `name="w.test wallet"`, 600))
	}
	out = html.UnescapeString(e.koinlyPost("/reports/koinly/map", url.Values{"w.test wallet": {"bogus"}}))
	if !strings.Contains(out, `"bogus"`) || !strings.Contains(out, "Koinly wallet test wallet") {
		t.Errorf("an unknown wallet must be refused naming the Koinly wallet: %s", within(out, "notice--loss", 400))
	}
	if !strings.Contains(page, `<option value="untracked">Untracked (no longer synced)</option>`) {
		t.Error("the wallet options lack untracked")
	}
	if !strings.Contains(within(page, `name="a.BTC (Bitcoin)"`, 600), `value="BTC" selected`) {
		t.Errorf("BTC not selected:\n%s", within(page, `name="a.BTC (Bitcoin)"`, 600))
	}
	if !strings.Contains(within(page, `name="a.TNFT#42 (TestPass)"`, 600), `<option value="" selected>Skip</option>`) {
		t.Errorf("the NFT is not skipped:\n%s", within(page, `name="a.TNFT#42 (TestPass)"`, 600))
	}
}

func TestKoinlyAcceptWritesYear(t *testing.T) {
	e := newEnv(t, "")
	// CryptKeep sees 0.3 BTC; Koinly 0.5 at $20,000: 0.2 BTC goes to untracked.
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 0.3}}})
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"), fixture(t, "transactions.csv"), fixture(t, "gains.csv"), fixture(t, "income.csv"))
	e.koinlyPost("/reports/koinly/map", url.Values{
		"w.test wallet": {"cold"}, "w.Cold": {"cold"}, "w.Test Wallet A": {"untracked"},
		"a.BTC (Bitcoin)": {"BTC"}, "a.TNFT#42 (TestPass)": {""}, "a.$ CLAIM AT EXAMPLE.COM ($ Example.com)": {""},
	})
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	recon := within(page, `id="koinly-recon"`, 6000)
	for _, want := range []string{"BTC (Bitcoin)", "$20,000.00", "BTC: Koinly 0.5 vs CryptKeep 0.3", `id="koinly-accept"`} {
		if !strings.Contains(recon, want) {
			t.Errorf("reconciliation lacks %q", want)
		}
	}
	if strings.Index(recon, "BTC: Koinly 0.5 vs CryptKeep 0.3") > strings.Index(recon, `id="koinly-accept"`) {
		t.Error("problems must show above Accept")
	}

	out := e.koinlyPost("/reports/koinly/accept", nil)
	if !strings.Contains(out, `window.location.href = "/reports"`) {
		t.Fatalf("accept must send the browser to /reports: %.600s", out)
	}
	y, ok := e.taxYears.Get(2025)
	if !ok || y.Source != "koinly" {
		t.Fatalf("2025 not closed by Koinly: %+v %v", y, ok)
	}
	var amount, cost, untracked float64
	for _, l := range y.ClosingLots {
		amount, cost = amount+l.Amount, cost+l.CostUSD
		if l.Wallet == tax.Untracked {
			untracked += l.Amount
		}
	}
	if math.Abs(cost-20000) > 1e-6 || math.Abs(amount-0.5) > 1e-12 || math.Abs(untracked-0.2) > 1e-12 {
		t.Errorf("closing lots Σ amount %v cost %v untracked %v: %+v", amount, cost, untracked, y.ClosingLots)
	}
	if len(y.Disposals) != 3 || len(y.Income) != 1 || math.Abs(y.Summary.LongGain-2000.50) > 1e-6 || math.Abs(y.Summary.Income-300.25) > 1e-6 {
		t.Errorf("gains/income not kept: %d disposals, %d income, %+v", len(y.Disposals), len(y.Income), y.Summary)
	}
	st := e.taxSet.Get()
	if st.WalletMap["test wallet"] != "cold" || st.WalletMap["Test Wallet A"] != tax.Untracked || st.AssetMap["BTC (Bitcoin)"] != "BTC" {
		t.Errorf("tax.yaml maps: %+v %+v", st.WalletMap, st.AssetMap)
	}
	if v, ok := st.AssetMap["TNFT#42 (TestPass)"]; !ok || v != "" {
		t.Errorf("a skipped asset is kept as \"\": %q %v", v, ok)
	}
	if e.draftExists() {
		t.Error("the draft must be deleted after accept")
	}
	// The dashboard now costs Cold's 0.3 BTC from the imported lots: $12,000.
	if _, page := e.do("GET", "/w/cold", nil, nil); !strings.Contains(within(page, `<span class="label">Cost basis</span>`, 300), "$12,000.00") {
		t.Errorf("cold cost basis tile: %q", within(page, `<span class="label">Cost basis</span>`, 300))
	}
}

// TestKoinlyExtraBasis: CryptKeep holds more than Koinly; the user's cost for the extra
// units is saved with the mapping and becomes a real (unflagged) basis in the closed year.
func TestKoinlyExtraBasis(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 0.8}}})
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"))
	e.koinlyPost("/reports/koinly/map", url.Values{"a.BTC (Bitcoin)": {"BTC"}})
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(page, `name="x.BTC"`) || !strings.Contains(page, "BTC: Koinly 0.5 vs CryptKeep 0.8") {
		t.Fatalf("the extra basis input or the difference is missing:\n%s", within(page, `id="koinly-recon"`, 6000))
	}
	if out := e.koinlyPost("/reports/koinly/map", url.Values{"x.BTC": {"lots"}}); !strings.Contains(out, "Your cost for BTC must be a dollar amount") {
		t.Errorf("a bad amount: %.400s", within(out, "notice", 400))
	}
	// An extra basis for an asset no holding maps to is ignored.
	e.koinlyPost("/reports/koinly/map", url.Values{"x.BTC": {"$1,500.00"}, "x.ETH": {"99"}})
	_, page = e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(page, `value="1500"`) {
		t.Errorf("the extra basis is not kept:\n%s", within(page, `name="x.BTC"`, 300))
	}
	if out := e.koinlyPost("/reports/koinly/accept", nil); !strings.Contains(out, "window.location") {
		t.Fatalf("accept: %.600s", out)
	}
	y, _ := e.taxYears.Get(2025)
	var cost, extra float64
	for _, l := range y.ClosingLots {
		cost += l.CostUSD
		if l.Flag == tax.FlagMissingBasis {
			t.Errorf("lot flagged missing-basis despite the user's basis: %+v", l)
		}
		if l.Acquired.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
			extra += l.Amount
		}
	}
	if math.Abs(cost-21500) > 1e-6 || math.Abs(extra-0.3) > 1e-12 {
		t.Errorf("closing lots cost %v (want 21,500), extra units %v (want 0.3): %+v", cost, extra, y.ClosingLots)
	}
}

// problems returns the reconciliation's problem lines of the wizard page.
func problems(t *testing.T, page string) string {
	t.Helper()
	return html.UnescapeString(within(page, "Check these before you accept:", 4000))
}

// TestKoinlySkippedAssetWithCost: skipping a holding that has a cost is listed, and with
// nothing left to carry over Accept is refused.
func TestKoinlySkippedAssetWithCost(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 0.5}}})
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"))
	e.koinlyPost("/reports/koinly/map", url.Values{"a.BTC (Bitcoin)": {""}})
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	pr := problems(t, page)
	if !strings.Contains(pr, "BTC (Bitcoin): skipped; Koinly's $20,000.00 cost won't carry over") {
		t.Errorf("problems lack the skipped BTC:\n%s", pr)
	}
	if strings.Contains(pr, "TNFT#42") || strings.Contains(pr, "EXAMPLE.COM") {
		t.Errorf("worthless skipped holdings are not problems:\n%s", pr)
	}
	out := html.UnescapeString(e.koinlyPost("/reports/koinly/accept", nil))
	if !strings.Contains(out, "nothing would carry over — map at least one asset") || strings.Contains(out, "window.location") {
		t.Fatalf("accept with every costed asset skipped: %s", reTags.ReplaceAllString(within(out, `id="koinly-accept"`, 2000), " "))
	}
	if _, ok := e.taxYears.Get(2025); ok {
		t.Error("2025 was written")
	}
}

// ambiguousHoldings is an invented holdings report whose JUP matches two known tokens.
var ambiguousHoldings = csvFile{"holdings.csv", []byte("Balances as at 12/31/2025 23:59\n\n" +
	"Asset,Quantity,Cost (USD),Value (USD),Description\n" +
	"JUP (Jupiter),900,610.00,500.00,-\n" +
	"BTC (Bitcoin),0.5,\"20,000.00\",\"45,000.00\",-\n" +
	"Total,-,\"20,610.00\",\"45,500.00\",-\n")}

// TestKoinlyAmbiguousSkipBlocksAccept: a costed holding left on Skip because its symbol is
// ambiguous blocks Accept until the user picks a key or ticks "skip anyway".
func TestKoinlyAmbiguousSkipBlocksAccept(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 0.5}}})
	leg := func(id, key string) core.Tx {
		return core.Tx{ID: core.TxID("hot", id), SourceID: "hot", ExternalID: id, Time: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC),
			Kind: core.TxReceive, Status: core.TxConfirmed, Legs: []core.Leg{{AssetKey: key, Symbol: "JUP", Amount: 1}}}
	}
	if _, err := e.act.Upsert(context.Background(), []core.Tx{leg("j1", "spl:JupFakeMintA"), leg("j2", "spl:JupFakeMintB")}); err != nil {
		t.Fatal(err)
	}
	e.ref.Force(context.Background())
	e.koinlyUpload(ambiguousHoldings)
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(within(page, `name="a.JUP (Jupiter)"`, 2500), `<option value="" selected>Skip</option>`) {
		t.Fatalf("the ambiguous JUP is not left on Skip")
	}
	if !strings.Contains(page, `name="s.JUP (Jupiter)"`) {
		t.Error(`no "skip anyway" checkbox for the ambiguous JUP`)
	}
	if !strings.Contains(problems(t, page), "JUP (Jupiter): skipped; Koinly's $610.00 cost won't carry over") {
		t.Errorf("problems:\n%s", problems(t, page))
	}
	refused := func() bool {
		out := html.UnescapeString(e.koinlyPost("/reports/koinly/accept", nil))
		return strings.Contains(out, "JUP (Jupiter) matches several assets") && !strings.Contains(out, "window.location")
	}
	if !refused() {
		t.Fatal("accept must wait for the ambiguous JUP")
	}
	// The full form posts the select without the box: still blocked.
	e.koinlyPost("/reports/koinly/map", url.Values{"a.JUP (Jupiter)": {""}, "a.BTC (Bitcoin)": {"BTC"}})
	if !refused() {
		t.Fatal("still blocked without the box")
	}
	e.koinlyPost("/reports/koinly/map", url.Values{"a.JUP (Jupiter)": {""}, "s.JUP (Jupiter)": {"1"}, "a.BTC (Bitcoin)": {"BTC"}})
	_, page = e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(within(page, `name="s.JUP (Jupiter)"`, 200), "checked") {
		t.Errorf("the box is not kept ticked:\n%s", within(page, `name="s.JUP (Jupiter)"`, 200))
	}
	if out := e.koinlyPost("/reports/koinly/accept", nil); !strings.Contains(out, "window.location") {
		t.Fatalf("accept after skip anyway: %s", reTags.ReplaceAllString(within(html.UnescapeString(out), `id="koinly-accept"`, 2000), " "))
	}
	y, ok := e.taxYears.Get(2025)
	if !ok {
		t.Fatal("2025 not closed")
	}
	for _, l := range y.ClosingLots {
		if l.Asset != "BTC" {
			t.Errorf("a skipped asset has a lot: %+v", l)
		}
	}
}

// TestKoinlyReimportReplacesClosedYear: re-importing the latest closed year says it
// replaces it, and does.
func TestKoinlyReimportReplacesClosedYear(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2025, tax.Summary{ShortGain: 1}, solLot("sol", 1, 10))
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 0.5}}})
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"))
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	if !strings.Contains(problems(t, page), "Accept replaces the closed 2025 year") {
		t.Errorf("problems:\n%s", problems(t, page))
	}
	if out := e.koinlyPost("/reports/koinly/accept", nil); !strings.Contains(out, "window.location") {
		t.Fatalf("accept: %.600s", out)
	}
	if y, _ := e.taxYears.Get(2025); y.Summary.ShortGain != 0 || len(y.ClosingLots) == 0 || y.ClosingLots[0].Asset != "BTC" {
		t.Errorf("2025 not replaced: %+v", y)
	}
}

// TestKoinlyDraftWritesSerialised: concurrent mapping posts, each setting a different
// asset, all land (no lost update between load and save).
func TestKoinlyDraftWritesSerialised(t *testing.T) {
	e := newEnv(t, "")
	e.koinlyUpload(fixture(t, "holdings.csv"))
	assets := []string{"BTC (Bitcoin)", "TNFT#42 (TestPass)", "$ CLAIM AT EXAMPLE.COM ($ Example.com)"}
	for round := range 10 {
		var wg sync.WaitGroup
		for i, a := range assets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, hdr := form(url.Values{"a." + a: {fmt.Sprintf("K%d-%d", round, i)}})
				req, _ := http.NewRequest("POST", e.srv.URL+"/reports/koinly/map", body)
				for k, v := range hdr {
					req.Header.Set(k, v)
				}
				if resp, err := http.DefaultClient.Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
		}
		wg.Wait()
		var d koinlyDraft
		if _, err := e.db.GetJSON(context.Background(), koinlyDraftKey, &d); err != nil {
			t.Fatal(err)
		}
		for i, a := range assets {
			if want := fmt.Sprintf("K%d-%d", round, i); d.AssetMap[a] != want {
				t.Fatalf("round %d: %s maps to %q, want %q (a lost update)", round, a, d.AssetMap[a], want)
			}
		}
	}
}

func TestKoinlyAcceptRefusedWhenLaterYearClosed(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2026, tax.Summary{})
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"), fixture(t, "transactions.csv"))
	e.koinlyPost("/reports/koinly/map", url.Values{"w.test wallet": {"untracked"}, "a.BTC (Bitcoin)": {"BTC"}})
	out := e.koinlyPost("/reports/koinly/accept", nil)
	if !strings.Contains(out, "2026 is already closed; reopen it first") || strings.Contains(out, "window.location") {
		t.Fatalf("accept: %.600s", out)
	}
	if _, ok := e.taxYears.Get(2025); ok {
		t.Error("2025 was written")
	}
	if st := e.taxSet.Get(); len(st.WalletMap) != 0 || len(st.AssetMap) != 0 {
		t.Errorf("tax.yaml was written: %+v", st)
	}
	if !e.draftExists() {
		t.Error("the draft must be kept")
	}
}

func TestKoinlyAcceptNeedsHoldings(t *testing.T) {
	e := newEnv(t, "")
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "transactions.csv"))
	if out := e.koinlyPost("/reports/koinly/accept", nil); !strings.Contains(out, "holdings report") {
		t.Fatalf("accept without holdings: %.600s", out)
	}
	if _, ok := e.taxYears.Get(2025); ok {
		t.Error("2025 was written")
	}
}

// TestKoinlyAcceptRefusesUnfinishedYear: a year that hasn't ended can't be closed.
func TestKoinlyAcceptRefusesUnfinishedYear(t *testing.T) {
	e := newEnv(t, "", func(d *Deps) { d.Now = func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) } })
	e.ref.Force(context.Background())
	holdings := csvFile{"holdings-2026.csv", []byte("Balances as at 12/31/2026 23:59\n\n" +
		"Asset,Quantity,Cost (USD),Value (USD),Description\nETH (Ethereum),2,\"4,000.00\",\"6,000.00\",-\nTotal,-,\"4,000.00\",\"6,000.00\",-\n")}
	if out := e.koinlyUpload(holdings); !strings.Contains(out, "Loaded holdings-2026.csv") {
		t.Fatalf("upload: %.600s", out)
	}
	out := html.UnescapeString(e.koinlyPost("/reports/koinly/accept", nil))
	if !strings.Contains(out, "2026 hasn't ended yet") || strings.Contains(out, "window.location") {
		t.Fatalf("accept: %s", reTags.ReplaceAllString(within(out, `id="koinly-accept"`, 3000), " "))
	}
	if n := strings.Count(out, "2026 hasn't ended yet"); n != 1 {
		t.Errorf("the reason shows %d times, want once", n)
	}
	if _, ok := e.taxYears.Get(2026); ok {
		t.Error("2026 was closed")
	}
}

func TestKoinlyUploadRejectsOtherCSV(t *testing.T) {
	e := newEnv(t, "")
	coinbase := csvFile{"coinbase.csv", []byte("ID,Timestamp,Transaction Type,Asset,Quantity Transacted,Price Currency,Price at Transaction,Subtotal,Total (inclusive of fees and/or spread),Fees and/or Spread,Notes\n" +
		"abc123,2025-01-02T10:00:00Z,Buy,ETH,1,USD,$3000.00,$3000.00,$3010.00,$10.00,Bought 1 ETH\n")}
	out := e.koinlyUpload(coinbase)
	if !strings.Contains(out, "not a Koinly report") || !strings.Contains(out, "coinbase.csv") {
		t.Fatalf("upload: %.600s", out)
	}
	if e.draftExists() {
		t.Error("nothing may be saved")
	}
}

// syncingSource is a source with activity support.
type syncingSource struct{ fakeExchange }

func (syncingSource) Activity(context.Context, core.ActivityRequest) (core.ActivityResult, error) {
	return core.ActivityResult{State: []byte("done"), BackfillDone: true}, nil
}

// TestKoinlyWarnsAboutShortHistory: a wallet whose activity history doesn't reach the
// cutover gets a warning on the reconciliation.
func TestKoinlyWarnsAboutShortHistory(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "hot", Type: sources.TypeSolana, Label: "Hot", Address: "HotFakeAddr1111"})
	e.ref.Force(context.Background())
	e.koinlyUpload(fixture(t, "holdings.csv"))
	_, page := e.do("GET", "/reports/koinly", nil, nil)
	// The test harness's sources have no activity: only activity sources are checked.
	if strings.Contains(page, "activity history for") {
		t.Error("a source without activity support needs no warning")
	}
	hot := []sources.Entry{{Def: sources.Def{ID: "hot", Label: "Hot"}, Source: syncingSource{fakeExchange{"hot"}}}}
	cutover := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := koinlyHistoryWarnings(hot, map[string]bool{"hot": false}, nil, cutover)
	if len(w) != 1 || w[0] != "activity history for Hot doesn't reach 2026-01-01; its quantity may be off" {
		t.Errorf("warnings %q", w)
	}
	late := []core.Tx{{SourceID: "hot", Time: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}}
	if w := koinlyHistoryWarnings(hot, map[string]bool{"hot": true}, late, cutover); len(w) != 1 {
		t.Errorf("earliest tx after the cutover: %q", w)
	}
	early := append(late, core.Tx{SourceID: "hot", Time: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)})
	if w := koinlyHistoryWarnings(hot, map[string]bool{"hot": true}, early, cutover); len(w) != 0 {
		t.Errorf("history reaching the cutover: %q", w)
	}
}

// ---- Task 6 carry-overs ------------------------------------------------------------------

// TestRealizedCardFlagsMissingBasisSales: open-year sales without a cost basis point the
// user at the Koinly import.
func TestRealizedCardFlagsMissingBasisSales(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	sell := core.Tx{ID: core.TxID("sol", "s1"), SourceID: "sol", ExternalID: "s1", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Kind: core.TxTrade, Status: core.TxConfirmed, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: -2, USD: usd(-300)}, {AssetKey: "USDC", Symbol: "USDC", Amount: 300, USD: usd(300)}}}
	if _, err := e.act.Upsert(context.Background(), []core.Tx{sell}); err != nil {
		t.Fatal(err)
	}
	e.ref.Force(context.Background())
	_, page := e.do("GET", "/reports", nil, nil)
	card := within(page, `id="realized-h"`, 2000)
	if text := reTags.ReplaceAllString(card, ""); !strings.Contains(text, "1 sale has no cost basis — import your opening basis from Koinly") ||
		!strings.Contains(card, `href="/reports/koinly"`) {
		t.Errorf("realized card:\n%s", card)
	}
	// The Reports page links to the import either way.
	if !strings.Contains(page, `href="/reports/koinly"`) {
		t.Error("Reports lacks the Koinly import link")
	}
}

func TestRealizedCardQuietWithBasis(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol", Holdings: []manual.Holding{{Asset: "SOL", Amount: 8}}})
	sell := core.Tx{ID: core.TxID("sol", "s1"), SourceID: "sol", ExternalID: "s1", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Kind: core.TxTrade, Status: core.TxConfirmed, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: -2, USD: usd(-300)}, {AssetKey: "USDC", Symbol: "USDC", Amount: 300, USD: usd(300)}}}
	if _, err := e.act.Upsert(context.Background(), []core.Tx{sell}); err != nil {
		t.Fatal(err)
	}
	e.ref.Force(context.Background())
	if _, page := e.do("GET", "/reports", nil, nil); strings.Contains(page, "no cost basis") {
		t.Error("covered sales must not be flagged")
	}
}

// TestTaxResultNotCachedBeforeSnapshot: before the first refresh the spam set is
// incomplete, so the result is recomputed on every read rather than cached.
func TestTaxResultNotCachedBeforeSnapshot(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	if _, page := e.do("GET", "/reports", nil, nil); strings.Contains(page, ">2026<") {
		t.Fatal("no 2026 sale yet")
	}
	sell := core.Tx{ID: core.TxID("sol", "s1"), SourceID: "sol", ExternalID: "s1", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Kind: core.TxTrade, Status: core.TxConfirmed, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: -2, USD: usd(-300)}, {AssetKey: "USDC", Symbol: "USDC", Amount: 300, USD: usd(300)}}}
	if _, err := e.act.Upsert(context.Background(), []core.Tx{sell}); err != nil {
		t.Fatal(err)
	}
	// Not invalidated: only a result computed without a snapshot may be recomputed.
	if _, page := e.do("GET", "/reports", nil, nil); !strings.Contains(within(page, `id="realized-h"`, 1500), ">2026<") {
		t.Error("a result computed before the first snapshot was cached")
	}
	// Once the snapshot is in, the result is cached again.
	e.ref.Force(context.Background())
	e.do("GET", "/reports", nil, nil)
	later := sell
	later.ID, later.ExternalID, later.Time = core.TxID("sol", "s2"), "s2", time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := e.act.Upsert(context.Background(), []core.Tx{later}); err != nil {
		t.Fatal(err)
	}
	if _, page := e.do("GET", "/reports", nil, nil); strings.Contains(within(page, `id="realized-h"`, 1500), ">2027<") {
		t.Error("after the first snapshot the result is cached")
	}
}

// TestUnknownUnitsThresholdIsRelative: float residue on a huge-supply token is not
// "unknown cost".
func TestUnknownUnitsThresholdIsRelative(t *testing.T) {
	lot := func(asset string, amount float64) tax.Lot {
		return tax.Lot{Wallet: "w", Asset: asset, Amount: amount, CostUSD: 100}
	}
	res := tax.Result{Lots: []tax.Lot{lot("BONK", 5e11-0.001), lot("SOL", 1-1e-10), lot("JUP", 0.9), lot("WIF", 4e11)}}
	rows := pnlRows(res, []portfolio.AssetRow{
		{AssetKey: "BONK", Symbol: "BONK", Amount: 5e11}, {AssetKey: "SOL", Symbol: "SOL", Amount: 1},
		{AssetKey: "JUP", Symbol: "JUP", Amount: 1}, {AssetKey: "WIF", Symbol: "WIF", Amount: 5e11},
	}, nil)
	want := map[string]float64{"BONK": 0, "SOL": 0, "JUP": 0.1, "WIF": 1e11}
	for _, r := range rows {
		if math.Abs(r.UnknownUnits-want[r.AssetKey]) > 1e-6*math.Max(1, want[r.AssetKey]) {
			t.Errorf("%s: unknown %v, want %v", r.AssetKey, r.UnknownUnits, want[r.AssetKey])
		}
	}
}

// TestWalletTileImportLinkAndAvg: with no basis the tile links to the Koinly import; the
// avg divides the basis by the units it covers.
func TestWalletTileImportLinkAndAvg(t *testing.T) {
	e := newEnv(t, "")
	e.price["SOL"] = 150
	missing := solLot("sol", 5, 0)
	missing.Flag = tax.FlagMissingBasis
	e.closeYear(2025, tax.Summary{}, solLot("sol", 5, 500), missing)
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol", Holdings: []manual.Holding{{Asset: "SOL", Amount: 10}}})
	e.reg.Upsert(sources.Def{ID: "bare", Type: sources.TypeManual, Label: "Bare", Holdings: []manual.Holding{{Asset: "ETH", Amount: 1}}})
	e.ref.Force(context.Background())

	_, page := e.do("GET", "/w/sol", nil, nil)
	tile := within(page, `<span class="label">Cost basis</span>`, 700)
	if !strings.Contains(tile, "$500.00") || !strings.Contains(tile, "$100.00") {
		t.Errorf("sol tile: basis $500 over 5 covered SOL is avg $100:\n%s", tile)
	}
	_, page = e.do("GET", "/w/bare", nil, nil)
	tile = within(page, `<span class="label">Cost basis</span>`, 1000)
	if !strings.Contains(tile, "Import cost basis") || !strings.Contains(tile, `href="/reports/koinly"`) || strings.Contains(page, "Set cost basis") {
		t.Errorf("bare tile:\n%s", tile)
	}
}
