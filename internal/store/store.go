// Package store is the SQLite database (data/vault.db) for machine-written state:
// last-good caches, history, token metadata, activity, price history and alert state.
// Human-editable state (sources, portfolios, alert rules) stays in YAML files.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with typed helpers.
type DB struct{ *sql.DB }

// migrations are applied in order; append only, never edit a released entry.
var migrations = []string{
	`CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, updated_at INTEGER NOT NULL);
	 CREATE TABLE lastgood_price (asset TEXT PRIMARY KEY, usd REAL NOT NULL, change24h REAL, as_of INTEGER NOT NULL);
	 CREATE TABLE lastgood_wallet (wallet_id TEXT PRIMARY KEY, json TEXT NOT NULL, as_of INTEGER NOT NULL);
	 CREATE TABLE history (ts INTEGER PRIMARY KEY, prices TEXT NOT NULL, amounts TEXT NOT NULL);
	 CREATE TABLE token_meta (mint TEXT PRIMARY KEY, json TEXT NOT NULL, fetched_at INTEGER NOT NULL);
	 CREATE TABLE price_history (asset TEXT NOT NULL, day TEXT NOT NULL, usd REAL NOT NULL, PRIMARY KEY (asset, day));`,
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps WAL simple and avoids SQLITE_BUSY
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db}, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := db.QueryRow(`SELECT v FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := db.Exec(`INSERT INTO schema_version (v) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET v = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// AddMigration lets later packages register schema changes at init time. Order of
// registration must be stable (package init order is deterministic within a binary).
func AddMigration(sql string) { migrations = append(migrations, sql) }

// GetJSON loads a JSON value from the kv table into out. Returns false if absent.
func (d *DB) GetJSON(ctx context.Context, key string, out any) (bool, error) {
	var raw string
	err := d.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), out)
}

// PutJSON stores a JSON value in the kv table.
func (d *DB) PutJSON(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `INSERT INTO kv (k, v, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at`, key, string(raw), time.Now().Unix())
	return err
}
