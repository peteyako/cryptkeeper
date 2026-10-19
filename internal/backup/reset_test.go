package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestResetErasesEverything: after a reset every store is empty, the database too, and a
// backup taken before restores all of it.
func TestResetErasesEverything(t *testing.T) {
	ctx := context.Background()
	st, dir := newStores(t)
	st = withDB(t, st, dir)
	seed(t, st)
	seedTax(t, st)
	st.DB.PutJSON(ctx, "history.marker", "before the reset")
	raw, err := ExportDatabase(ctx, st, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := Reset(ctx, st); err != nil {
		t.Fatal(err)
	}
	switch {
	case len(st.Sources.Defs()) != 0:
		t.Errorf("sources left: %v", st.Sources.Defs())
	case len(st.Portfolios.List()) != 0:
		t.Errorf("portfolios left: %v", st.Portfolios.List())
	case len(st.Alerts.List()) != 0:
		t.Errorf("alert rules left: %v", st.Alerts.List())
	case len(st.CostBasis.Get().Assets) != 0:
		t.Errorf("cost basis left: %+v", st.CostBasis.Get())
	case !isEmptySettings(st.Tax.Get()):
		t.Errorf("tax settings left: %+v", st.Tax.Get())
	case len(st.TaxYears.Years()) != 0:
		t.Errorf("closed years left: %v", st.TaxYears.Years())
	}
	if _, err := os.Stat(filepath.Join(dir, "tax", "2025.yaml")); !os.IsNotExist(err) {
		t.Errorf("the closed year's file is still there: %v", err)
	}
	var got string
	if ok, _ := st.DB.GetJSON(ctx, "history.marker", &got); ok {
		t.Error("the database kept its rows")
	}
	if f, _ := Parse(mustExport(t, st)); len(f.Sources)+len(f.Portfolios)+len(f.Alerts)+len(f.TaxYears) != 0 || f.CostBasis != nil || f.Tax != nil {
		t.Errorf("an export after the reset isn't empty: %+v", f)
	}

	if _, err := Import(st, raw, false); err != nil {
		t.Fatalf("restore after reset: %v", err)
	}
	if _, ok := st.Sources.Get("kraken"); !ok || len(st.TaxYears.Years()) != 1 {
		t.Error("the backup didn't bring the sources and tax years back")
	}
	if ok, _ := st.DB.GetJSON(ctx, "history.marker", &got); !ok || got != "before the reset" {
		t.Errorf("the backup didn't bring the database back: %q", got)
	}
}

// TestResetEmptyIsANoop: resetting a fresh install works and writes no files it didn't have.
func TestResetEmptyIsANoop(t *testing.T) {
	st, dir := newStores(t)
	if err := Reset(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"costbasis.yaml", "tax.yaml", "backups"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written by resetting nothing (%v)", name, err)
		}
	}
}

func mustExport(t *testing.T, st Stores) []byte {
	t.Helper()
	raw, err := Export(st, false)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
