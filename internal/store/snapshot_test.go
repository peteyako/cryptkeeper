package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T, name string) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func kv(t *testing.T, db *DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := db.Query(`SELECT k, v FROM kv`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		out[k] = v
	}
	return out
}

// TestSnapshotRestore: a snapshot restores into another database; merge keeps the rows
// already there and adds the missing ones, replace makes the tables the snapshot's.
func TestSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	from, to := openTest(t, "from.db"), openTest(t, "to.db")
	from.Exec(`INSERT INTO kv (k, v, updated_at) VALUES ('a', '"from-a"', 1), ('b', '"from-b"', 1)`)
	from.Exec(`INSERT INTO price_history (asset, day, usd) VALUES ('BTC', '2026-01-01', 90000)`)
	to.Exec(`INSERT INTO kv (k, v, updated_at) VALUES ('a', '"to-a"', 2), ('c', '"to-c"', 2)`)

	snap, err := from.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := to.Restore(ctx, snap, false)
	if err != nil || n == 0 {
		t.Fatalf("merge: %d tables, %v", n, err)
	}
	if got := kv(t, to); got["a"] != `"to-a"` || got["b"] != `"from-b"` || got["c"] != `"to-c"` {
		t.Errorf("merge kv %v, want to's a and c kept and from's b added", got)
	}
	var usd float64
	if err := to.QueryRow(`SELECT usd FROM price_history WHERE asset = 'BTC'`).Scan(&usd); err != nil || usd != 90000 {
		t.Errorf("price history %v %v", usd, err)
	}

	if _, err := to.Restore(ctx, snap, true); err != nil {
		t.Fatal(err)
	}
	if got := kv(t, to); len(got) != 2 || got["a"] != `"from-a"` || got["b"] != `"from-b"` {
		t.Errorf("replace kv %v, want exactly from's", got)
	}
}

// TestRestoreRefusesOtherFiles: a file that isn't SQLite, or an SQLite database that isn't
// CryptKeep's, is refused without touching anything.
func TestRestoreRefusesOtherFiles(t *testing.T) {
	ctx := context.Background()
	to := openTest(t, "to.db")
	to.Exec(`INSERT INTO kv (k, v, updated_at) VALUES ('keep', '1', 1)`)
	if _, err := to.Restore(ctx, []byte("not a database"), true); err == nil {
		t.Error("garbage was restored")
	}
	other := filepath.Join(t.TempDir(), "other.db")
	odb, err := sql.Open("sqlite", "file:"+other)
	if err != nil {
		t.Fatal(err)
	}
	odb.Exec(`CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, updated_at INTEGER NOT NULL)`)
	odb.Exec(`VACUUM INTO '` + other + `.copy'`)
	odb.Close()
	foreign, _ := readFile(other + ".copy")
	if _, err := to.Restore(ctx, foreign, true); err == nil {
		t.Error("a non-CryptKeep database was restored")
	}
	if got := kv(t, to); got["keep"] != "1" {
		t.Errorf("a refused restore changed the database: %v", got)
	}
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
