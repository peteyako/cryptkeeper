package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/yamlfile"
)

func newStores(t *testing.T) (Stores, string) {
	t.Helper()
	dir := t.TempDir()
	bk := filepath.Join(dir, "backups")
	box, _ := secrets.NewBox(bytes.Repeat([]byte{5}, 32))
	reg, err := sources.NewRegistry(filepath.Join(dir, "sources.yaml"), bk, box, func(d sources.Def) (core.Source, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	ps, err := portfolios.NewStore(filepath.Join(dir, "portfolios.yaml"), bk)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := alerts.NewRuleStore(filepath.Join(dir, "alerts.yaml"), bk)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := costbasis.NewManualStore(filepath.Join(dir, "costbasis.yaml"), bk)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := tax.NewSettingsStore(filepath.Join(dir, "tax.yaml"), bk)
	if err != nil {
		t.Fatal(err)
	}
	ty, err := tax.NewYearStore(filepath.Join(dir, "tax"), bk)
	if err != nil {
		t.Fatal(err)
	}
	return Stores{Sources: reg, Portfolios: ps, Alerts: rules, CostBasis: cb, Tax: ts, TaxYears: ty}, dir
}

func seed(t *testing.T, st Stores) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := st.Sources.Upsert(sources.Def{ID: "kraken", Type: sources.TypeKraken, Label: "Kraken", APIKey: "pub", APISecret: "very-secret"})
	must(err)
	_, err = st.Sources.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	must(err)
	_, err = st.Portfolios.Upsert(portfolios.Portfolio{ID: "test", Name: "Test", Wallets: portfolios.Selector{IDs: []string{"kids"}}, Assets: portfolios.AllSelector()})
	must(err)
	_, err = st.Alerts.Upsert(alerts.Rule{ID: "btc-high", Type: alerts.TypePrice, Asset: "BTC", Above: 100000})
	must(err)
	must(st.CostBasis.SetAsset("BTC", 0.5, 15000, "old buys"))
}

func TestExportHasEverySectionAndNoPlaintextSecrets(t *testing.T) {
	st, _ := newStores(t)
	seed(t, st)
	raw, err := Export(st, false)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{"sources:", "kids", "portfolios:", "id: test", "alerts:", "btc-high", "costBasis:", "old buys"} {
		if !strings.Contains(out, want) {
			t.Errorf("export missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "very-secret") || strings.Contains(out, "enc:v1:") {
		t.Fatalf("export without secrets leaked them:\n%s", out)
	}
	sealed, _ := Export(st, true)
	if strings.Contains(string(sealed), "very-secret") || !strings.Contains(string(sealed), "enc:v1:") {
		t.Fatal("export with secrets must carry them sealed only")
	}
}

func TestRestoreEverythingAfterDeleting(t *testing.T) {
	st, _ := newStores(t)
	seed(t, st)
	raw, err := Export(st, true)
	if err != nil {
		t.Fatal(err)
	}
	// The scenario from the bug report: delete the portfolio and the source, then restore.
	st.Portfolios.Delete("test")
	st.Sources.Delete("kids")
	st.Alerts.Delete("btc-high")
	st.CostBasis.DeleteAsset("BTC")

	res, err := Import(st, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := st.Portfolios.Get("test"); !ok || !p.Wallets.Has("kids") {
		t.Fatalf("portfolio not restored with its wallet: %+v %v", p, ok)
	}
	if _, ok := st.Sources.Get("kids"); !ok {
		t.Fatal("source not restored")
	}
	if _, ok := st.Alerts.Get("btc-high"); !ok {
		t.Fatal("alert rule not restored")
	}
	if m := st.CostBasis.Get(); len(m.Assets) != 1 || m.Assets[0].CostUSD != 15000 {
		t.Fatalf("cost basis not restored: %+v", m)
	}
	d, _ := st.Sources.Get("kraken")
	if opened, err := st.Sources.Open(d); err != nil || opened.APISecret != "very-secret" {
		t.Fatalf("sealed secret must survive the round trip: %v", err)
	}
	if s := res.Summary(); !strings.Contains(s, "portfolios 1 added") || !strings.Contains(s, "sources 1 added") {
		t.Fatalf("summary %q", s)
	}
}

func TestReplaceOnlyTouchesSectionsInTheFile(t *testing.T) {
	st, _ := newStores(t)
	seed(t, st)
	var removed []string
	st.OnSourceRemoved = func(id string) { removed = append(removed, id) }
	// A sources-only file in replace mode: kraken is removed, portfolios/alerts untouched.
	raw := []byte("version: 1\nsources:\n  - {id: kids, type: manual, label: Kids, kind: manual, holdings: [{asset: SOL, amount: 3}]}\n")
	if _, err := Import(st, raw, true); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "kraken" {
		t.Fatalf("removed %v", removed)
	}
	if _, ok := st.Portfolios.Get("test"); !ok {
		t.Fatal("a section absent from the file must be left alone")
	}
	if len(st.Alerts.List()) != 1 {
		t.Fatal("alerts must be untouched")
	}
}

func TestOlderExportsImport(t *testing.T) {
	st, _ := newStores(t)
	// A portfolios-page export (portfolios + alerts) restores through the same path.
	raw := []byte("version: 1\nportfolios:\n  - {id: lt, name: Long-term}\nalerts:\n  - {id: sol-low, type: price, asset: sol, below: 80}\n")
	res, err := Import(st, raw, false)
	if err != nil || res.Portfolios == nil || res.Alerts == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if r, _ := st.Alerts.Get("sol-low"); r.Asset != "SOL" {
		t.Fatalf("alert normalized: %+v", r)
	}
}

func TestInvalidSectionWritesNothing(t *testing.T) {
	st, _ := newStores(t)
	raw := []byte("version: 1\nsources:\n  - {id: ok, type: manual, label: Ok, kind: manual, holdings: [{asset: BTC, amount: 1}]}\nalerts:\n  - {id: broken, type: price}\n")
	if _, err := Import(st, raw, false); err == nil || !strings.Contains(err.Error(), "alerts") {
		t.Fatalf("want alerts validation error, got %v", err)
	}
	if len(st.Sources.Defs()) != 0 {
		t.Fatal("a later invalid section must stop the import before any write")
	}
	if _, err := Parse([]byte("version: 1\nportfolio: []\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("typo must be a line-numbered error: %v", err)
	}
	if _, err := Parse([]byte("version: 1\n")); err == nil {
		t.Fatal("an empty backup must be rejected")
	}
	seedPhrase := []byte("version: 1\nsources:\n  - {id: x, type: bitcoin, label: X, key: \"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about\"}\n")
	if _, err := Import(st, seedPhrase, false); err == nil || !strings.Contains(err.Error(), "seed phrase") {
		t.Fatalf("seed phrase must be refused: %v", err)
	}
}

func taxYear(year int, source string) tax.Year {
	return tax.Year{
		Year: year, Source: source, ClosedAt: time.Date(year+1, 1, 5, 0, 0, 0, 0, time.UTC),
		ClosingLots: []tax.Lot{{Wallet: "w1", Asset: "BTC", Amount: 1, CostUSD: 100, Acquired: time.Date(year, 2, 1, 0, 0, 0, 0, time.UTC)}},
	}
}

func seedTax(t *testing.T, st Stores) {
	t.Helper()
	if err := st.Tax.Set(tax.Settings{
		WalletMap:  map[string]string{"Old Wallet": "kraken"},
		ManualLots: []tax.Lot{{Wallet: tax.Untracked, Asset: "ETH", Amount: 2, CostUSD: 50, Acquired: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}},
		Prices:     map[string]float64{"k": 1.5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.TaxYears.Save(taxYear(2025, "koinly")); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRoundTripTax(t *testing.T) {
	src, _ := newStores(t)
	seedTax(t, src)
	raw, err := Export(src, false)
	if err != nil {
		t.Fatal(err)
	}
	dst, _ := newStores(t)
	res, err := Import(dst, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Tax || res.TaxYears != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got, want := dst.Tax.Get(), src.Tax.Get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("tax settings\n got %+v\nwant %+v", got, want)
	}
	got, ok := dst.TaxYears.Get(2025)
	want, _ := src.TaxYears.Get(2025)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("year 2025\n got %+v\nwant %+v", got, want)
	}
	if !strings.Contains(res.Summary(), "tax settings") || !strings.Contains(res.Summary(), "1 closed tax year") {
		t.Fatalf("summary = %q", res.Summary())
	}
}

func TestBackupMergeKeepsExistingYear(t *testing.T) {
	src, _ := newStores(t)
	if err := src.TaxYears.Save(taxYear(2025, "koinly")); err != nil {
		t.Fatal(err)
	}
	if err := src.TaxYears.Save(taxYear(2026, "cryptkeeper")); err != nil {
		t.Fatal(err)
	}
	raw, err := Export(src, false)
	if err != nil {
		t.Fatal(err)
	}
	dst, _ := newStores(t)
	if err := dst.TaxYears.Save(taxYear(2025, "cryptkeeper")); err != nil {
		t.Fatal(err)
	}
	res, err := Import(dst, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if y, _ := dst.TaxYears.Get(2025); y.Source != "cryptkeeper" {
		t.Fatalf("local 2025 replaced: source %q", y.Source)
	}
	if !slices.Equal(dst.TaxYears.Years(), []int{2025, 2026}) {
		t.Fatalf("years = %v", dst.TaxYears.Years())
	}
	if res.TaxYears != 1 || !slices.Equal(res.TaxYearsSkipped, []int{2025}) {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Summary(), "skipped") {
		t.Fatalf("summary = %q", res.Summary())
	}

	// Only the incoming 2025: nothing imported, reported as skipped.
	only, _ := newStores(t)
	if err := only.TaxYears.Save(taxYear(2025, "koinly")); err != nil {
		t.Fatal(err)
	}
	raw, _ = Export(only, false)
	res, err = Import(dst, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.TaxYears != 0 || len(res.TaxYearsSkipped) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestBackupMergeSkipsYearBeforeLocalLater(t *testing.T) {
	src, _ := newStores(t)
	if err := src.TaxYears.Save(taxYear(2024, "koinly")); err != nil {
		t.Fatal(err)
	}
	raw, _ := Export(src, false)
	dst, _ := newStores(t)
	if err := dst.TaxYears.Save(taxYear(2025, "koinly")); err != nil {
		t.Fatal(err)
	}
	res, err := Import(dst, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.TaxYears != 0 || !slices.Equal(res.TaxYearsSkipped, []int{2024}) || !slices.Equal(dst.TaxYears.Years(), []int{2025}) {
		t.Fatalf("result = %+v years %v", res, dst.TaxYears.Years())
	}
}

func TestBackupMergeTaxSettings(t *testing.T) {
	dst, _ := newStores(t)
	lot := tax.Lot{Wallet: tax.Untracked, Asset: "ETH", Amount: 2, CostUSD: 50, Acquired: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := dst.Tax.Set(tax.Settings{WalletMap: map[string]string{"A": "x", "B": "y"}, ManualLots: []tax.Lot{lot}}); err != nil {
		t.Fatal(err)
	}
	src, _ := newStores(t)
	other := lot
	other.Amount = 3
	if err := src.Tax.Set(tax.Settings{WalletMap: map[string]string{"B": "z", "C": "w"}, ManualLots: []tax.Lot{lot, other}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := Export(src, false)
	if _, err := Import(dst, raw, false); err != nil {
		t.Fatal(err)
	}
	got := dst.Tax.Get()
	if !reflect.DeepEqual(got.WalletMap, map[string]string{"A": "x", "B": "z", "C": "w"}) || len(got.ManualLots) != 2 {
		t.Fatalf("merged = %+v", got)
	}
}

func TestBackupReplaceTaxYears(t *testing.T) {
	src, _ := newStores(t)
	seedTax(t, src)
	raw, _ := Export(src, false)
	dst, dir := newStores(t)
	for _, y := range []int{2023, 2024} {
		if err := dst.TaxYears.Save(taxYear(y, "cryptkeeper")); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Import(dst, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.TaxYears != 1 || !slices.Equal(dst.TaxYears.Years(), []int{2025}) {
		t.Fatalf("result = %+v years %v", res, dst.TaxYears.Years())
	}
	for _, y := range []string{"2023", "2024"} {
		if _, err := os.Stat(filepath.Join(dir, "tax", y+".yaml")); !os.IsNotExist(err) {
			t.Fatalf("%s.yaml still on disk: %v", y, err)
		}
	}
	// Reload from disk to be sure.
	re, err := tax.NewYearStore(filepath.Join(dir, "tax"), "")
	if err != nil || !slices.Equal(re.Years(), []int{2025}) {
		t.Fatalf("reloaded %v err %v", re.Years(), err)
	}
}

func TestBackupTaxWithoutStoresRejected(t *testing.T) {
	src, _ := newStores(t)
	seedTax(t, src)
	raw, _ := Export(src, false)
	dst, _ := newStores(t)
	dst.Tax, dst.TaxYears = nil, nil
	if _, err := Import(dst, raw, false); err == nil {
		t.Fatal("want error")
	}
}

func TestBackupInvalidTaxYearWritesNothing(t *testing.T) {
	bad := taxYear(2025, "koinly")
	bad.ClosingLots[0].Amount = 0
	raw, err := yamlfile.Encode(File{
		Version:  Version,
		Sources:  []sources.Def{{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}}},
		TaxYears: []tax.Year{bad},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err == nil {
		t.Fatal("Parse accepted an invalid tax year")
	}
	for _, replace := range []bool{false, true} {
		dst, dir := newStores(t)
		if _, err := Import(dst, raw, replace); err == nil {
			t.Fatal("Import accepted an invalid tax year")
		}
		if n := len(dst.Sources.Defs()); n != 0 {
			t.Fatalf("%d sources written", n)
		}
		if _, err := os.Stat(filepath.Join(dir, "tax", "2025.yaml")); !os.IsNotExist(err) {
			t.Fatalf("year file written: %v", err)
		}
	}
}

func TestBackupNoopTaxImportNotReported(t *testing.T) {
	src, _ := newStores(t)
	seedTax(t, src)
	raw, _ := Export(src, false)
	res, err := Import(src, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tax || strings.Contains(res.Summary(), "tax settings") {
		t.Fatalf("no-op import reported: %+v", res)
	}
}
