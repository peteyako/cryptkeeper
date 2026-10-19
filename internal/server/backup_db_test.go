package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
)

// TestBackupWithDatabase: Settings offers the database in the export; an export with it
// carries the activity, and restoring it with Replace brings deleted activity back.
func TestBackupWithDatabase(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	tx := core.Tx{ID: "kids:t1", SourceID: "kids", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Kind: core.TxReceive, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: 1}}}
	if _, err := e.act.Upsert(context.Background(), []core.Tx{tx}); err != nil {
		t.Fatal(err)
	}

	if _, page := e.do("GET", "/settings", nil, nil); !strings.Contains(page, `name="db" value="1"`) {
		t.Fatal("Settings doesn't offer the database")
	}
	resp, plain := e.do("GET", "/backup/export", nil, nil)
	if resp.StatusCode != 200 || strings.Contains(plain, "database:") {
		t.Fatalf("a plain export carries the database: %d", resp.StatusCode)
	}
	resp, file := e.do("GET", "/backup/export?db=1", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(file, "database: ") {
		t.Fatalf("export with database: %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="cryptkeep-backup-with-database-`) {
		t.Errorf("Content-Disposition = %q", cd)
	}

	if err := e.act.DeleteSource(context.Background(), "kids"); err != nil {
		t.Fatal(err)
	}
	body, hdr := multipartFile(t, "file", "cryptkeep-backup-with-database.yaml", []byte(file), map[string]string{"mode": "replace"})
	if _, out := e.do("POST", "/backup/import", body, hdr); !strings.Contains(out, "database restored") {
		t.Fatalf("restore: %.600s", out)
	}
	all, err := e.act.All(context.Background(), nil)
	if err != nil || len(all) != 1 || all[0].ID != "kids:t1" {
		t.Fatalf("activity after restore: %v %v", all, err)
	}
}
