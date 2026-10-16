package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigrateAndKV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.PutJSON(ctx, "a", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(path) // reopen: migrations must be idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got map[string]int
	ok, err := db.GetJSON(ctx, "a", &got)
	if err != nil || !ok || got["x"] != 1 {
		t.Fatalf("%v %v %v", ok, err, got)
	}
	ok, _ = db.GetJSON(ctx, "missing", &got)
	if ok {
		t.Fatal("expected missing")
	}
}
