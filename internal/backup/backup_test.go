package backup

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
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
	return Stores{Sources: reg, Portfolios: ps, Alerts: rules, CostBasis: cb}, dir
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
