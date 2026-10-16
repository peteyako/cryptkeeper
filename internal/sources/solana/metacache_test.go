package solana

import (
	"path/filepath"
	"testing"
	"time"

	"crypto-portfolio/internal/store"
)

func sampleMeta(mint string) TokenMeta {
	return TokenMeta{
		Mint: mint, Symbol: "TST", Name: "Test Token", LogoURL: "https://example.com/t.png",
		Decimals: 6, Verified: true, Source: "jupiter",
	}
}

func TestMemoryMetaCache(t *testing.T) {
	c := NewMemoryMetaCache()
	if _, ok := c.Get("m1"); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.Put("m1", sampleMeta("m1"))
	got, ok := c.Get("m1")
	if !ok || got != sampleMeta("m1") {
		t.Errorf("got %+v, %v", got, ok)
	}
	c.Put("m1", TokenMeta{Mint: "m1", Symbol: "NEW"})
	if got, _ := c.Get("m1"); got.Symbol != "NEW" {
		t.Errorf("overwrite failed: %+v", got)
	}
}

func openTempDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLiteMetaCacheRoundTrip(t *testing.T) {
	db := openTempDB(t)
	c := NewSQLiteMetaCache(db, time.Hour)

	if _, ok := c.Get("m1"); ok {
		t.Fatal("empty cache returned a hit")
	}
	want := sampleMeta("m1")
	c.Put("m1", want)
	got, ok := c.Get("m1")
	if !ok || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, ok, want)
	}

	// Survives a new cache instance on the same DB (i.e. a restart).
	c2 := NewSQLiteMetaCache(db, time.Hour)
	if got, ok := c2.Get("m1"); !ok || got != want {
		t.Fatalf("second instance: got %+v, %v", got, ok)
	}

	// Put replaces and refreshes.
	upd := want
	upd.Symbol = "NEW"
	c.Put("m1", upd)
	if got, _ := c.Get("m1"); got.Symbol != "NEW" {
		t.Errorf("upsert failed: %+v", got)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM token_meta`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("rows = %d, err = %v", rows, err)
	}
}

func TestSQLiteMetaCacheTTL(t *testing.T) {
	db := openTempDB(t)
	c := NewSQLiteMetaCache(db, time.Hour)
	c.Put("fresh", sampleMeta("fresh"))
	c.Put("old", sampleMeta("old"))

	// Age "old" past the TTL, and "fresh" to just inside it.
	age := func(mint string, d time.Duration) {
		t.Helper()
		if _, err := db.Exec(`UPDATE token_meta SET fetched_at = ? WHERE mint = ?`,
			time.Now().Add(-d).Unix(), mint); err != nil {
			t.Fatal(err)
		}
	}
	age("old", 2*time.Hour)
	age("fresh", 30*time.Minute)

	if _, ok := c.Get("old"); ok {
		t.Error("entry older than ttl should be a miss")
	}
	if _, ok := c.Get("fresh"); !ok {
		t.Error("entry within ttl should be a hit")
	}

	// Re-putting an expired entry makes it a hit again.
	c.Put("old", sampleMeta("old"))
	if _, ok := c.Get("old"); !ok {
		t.Error("refreshed entry should be a hit")
	}
}

func TestSQLiteMetaCacheIgnoresCorruptRow(t *testing.T) {
	db := openTempDB(t)
	c := NewSQLiteMetaCache(db, time.Hour)
	if _, err := db.Exec(`INSERT INTO token_meta (mint, json, fetched_at) VALUES ('bad', '{not json', ?)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("bad"); ok {
		t.Error("corrupt row should be a miss")
	}
}
