package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigrateAndKV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cryptkeep.db")
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

func TestMigrationsRunOnceByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cryptkeep.db")
	AddMigration("test-extra", `CREATE TABLE extra (x INTEGER)`)
	defer func() { migrations = migrations[:len(migrations)-1] }()
	for range 2 { // reopening must not re-run anything
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
		if n != len(migrations) {
			t.Fatalf("recorded %d migrations, registered %d", n, len(migrations))
		}
		db.Close()
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate name must panic")
		}
	}()
	AddMigration("test-extra", `SELECT 1`)
}

func TestDeleteKV(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PutJSON(ctx, "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteKV(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	var n int
	if ok, _ := db.GetJSON(ctx, "a", &n); ok {
		t.Fatal("key should be gone")
	}
	if err := db.DeleteKV(ctx, "missing"); err != nil {
		t.Fatal("deleting an absent key is not an error")
	}
}
