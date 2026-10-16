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

// migration is one named schema change. Names are recorded in schema_migrations once
// applied, so the order in which packages register them (Go init order) never matters for
// correctness: each runs exactly once per database.
type migration struct{ name, sql string }

var migrations = []migration{
	{"001-base", `CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, updated_at INTEGER NOT NULL);
	 CREATE TABLE lastgood_price (asset TEXT PRIMARY KEY, usd REAL NOT NULL, change24h REAL, as_of INTEGER NOT NULL);
	 CREATE TABLE lastgood_wallet (wallet_id TEXT PRIMARY KEY, json TEXT NOT NULL, as_of INTEGER NOT NULL);
	 CREATE TABLE history (ts INTEGER PRIMARY KEY, prices TEXT NOT NULL, amounts TEXT NOT NULL);
	 CREATE TABLE token_meta (mint TEXT PRIMARY KEY, json TEXT NOT NULL, fetched_at INTEGER NOT NULL);
	 CREATE TABLE price_history (asset TEXT NOT NULL, day TEXT NOT NULL, usd REAL NOT NULL, PRIMARY KEY (asset, day));`},
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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Databases created by the first release tracked a schema_version counter; its first
	// migration is 001-base.
	var legacy int
	if err := db.QueryRow(`SELECT v FROM schema_version`).Scan(&legacy); err == nil && legacy >= 1 && !applied["001-base"] {
		if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('001-base', ?)`, time.Now().Unix()); err != nil {
			return err
		}
		applied["001-base"] = true
	}
	for _, m := range migrations {
		if applied[m.name] {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, m.name, time.Now().Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// AddMigration registers a named schema change from a package init(). Names must be
// unique and never reused; never edit a released migration — add a new one.
func AddMigration(name, sql string) {
	for _, m := range migrations {
		if m.name == name {
			panic("store: duplicate migration " + name)
		}
	}
	migrations = append(migrations, migration{name, sql})
}

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
