package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"crypto-portfolio/internal/store"
	"crypto-portfolio/internal/tax"
)

func withDB(t *testing.T, st Stores, dir string) Stores {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "cryptkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st.DB = db
	return st
}

// TestDatabaseRoundTrip: an export with the database carries it; restoring it into another
// CryptKeeper brings its rows back, merged or replacing, along with the other sections.
func TestDatabaseRoundTrip(t *testing.T) {
	ctx := context.Background()
	src, dir := newStores(t)
	src = withDB(t, src, dir)
	seed(t, src)
	src.DB.PutJSON(ctx, "history.marker", "from the backup")

	plain, err := Export(src, false)
	if err != nil || strings.Contains(string(plain), "database:") {
		t.Fatalf("a plain export has no database: %v", err)
	}
	raw, err := ExportDatabase(ctx, src, false)
	if err != nil || !strings.Contains(string(raw), "database: ") {
		t.Fatalf("export with database: %v", err)
	}

	dst, dir2 := newStores(t)
	dst = withDB(t, dst, dir2)
	dst.DB.PutJSON(ctx, "local.only", "kept by a merge")
	res, err := Import(dst, raw, false)
	if err != nil || !res.Database || !strings.Contains(res.Summary(), "database restored") {
		t.Fatalf("merge: %+v %v", res, err)
	}
	var got string
	if ok, _ := dst.DB.GetJSON(ctx, "history.marker", &got); !ok || got != "from the backup" {
		t.Errorf("restored marker %q", got)
	}
	if ok, _ := dst.DB.GetJSON(ctx, "local.only", &got); !ok {
		t.Error("a merge dropped a local row")
	}
	if _, ok := dst.Sources.Get("kids"); !ok {
		t.Error("the other sections weren't restored with the database")
	}

	if _, err := Import(dst, raw, true); err != nil {
		t.Fatal(err)
	}
	if ok, _ := dst.DB.GetJSON(ctx, "local.only", &got); ok {
		t.Error("a replace kept a row the backup doesn't have")
	}
}

// TestDatabaseNeedsAStore: a backup with a database can't restore where there is none, and
// nothing else is restored either.
func TestDatabaseNeedsAStore(t *testing.T) {
	ctx := context.Background()
	src, dir := newStores(t)
	src = withDB(t, src, dir)
	seed(t, src)
	raw, err := ExportDatabase(ctx, src, false)
	if err != nil {
		t.Fatal(err)
	}
	dst, _ := newStores(t)
	if _, err := Import(dst, raw, false); err == nil {
		t.Fatal("restored a database without a store")
	}
	if _, ok := dst.Sources.Get("kids"); ok {
		t.Error("sections were restored although the file can't be restored whole")
	}
}

// TestScopeIsBackedUp: the tax scope alone is a tax section, and a merge restores it.
func TestScopeIsBackedUp(t *testing.T) {
	src, _ := newStores(t)
	if err := src.Tax.Update(func(s *tax.Settings) { s.Scope = "taxable" }); err != nil {
		t.Fatal(err)
	}
	raw, err := Export(src, false)
	if err != nil {
		t.Fatal(err)
	}
	dst, _ := newStores(t)
	if _, err := Import(dst, raw, false); err != nil {
		t.Fatal(err)
	}
	if got := dst.Tax.Get().Scope; got != "taxable" {
		t.Errorf("restored scope %q", got)
	}
}
