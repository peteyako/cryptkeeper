package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// sqliteMagic starts every SQLite database file.
var sqliteMagic = []byte("SQLite format 3\x00")

// notRestored are the tables a restore leaves alone: the migrations this database has
// applied, and SQLite's own bookkeeping.
var notRestored = []string{"schema_migrations", "sqlite_sequence"}

// Snapshot is a consistent copy of the whole database, as the bytes of an SQLite file
// (VACUUM INTO a temporary file), taken while the app keeps running.
func (d *DB) Snapshot(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "cryptkeeper-snapshot-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cryptkeeper.db")
	if _, err := d.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return nil, fmt.Errorf("copy the database: %w", err)
	}
	return os.ReadFile(path)
}

// Restore copies the tables of a database snapshot (the bytes Snapshot returns) into this
// one, in one transaction: with replace each table first loses its rows, so the database
// becomes the snapshot's; otherwise the rows already here stay and only the missing ones
// (by primary key) are added. The snapshot is migrated to this build's schema first, so an
// older one restores too (its missing tables count as empty). It returns how many tables
// were restored.
func (d *DB) Restore(ctx context.Context, snapshot []byte, replace bool) (int, error) {
	if !bytes.HasPrefix(snapshot, sqliteMagic) {
		return 0, errors.New("the database in the backup isn't an SQLite database")
	}
	dir, err := os.MkdirTemp("", "cryptkeeper-restore-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "restore.db")
	if err := os.WriteFile(path, snapshot, 0o600); err != nil {
		return 0, err
	}
	if !isCryptKeeper(ctx, path) {
		return 0, errors.New("the database in the backup isn't a CryptKeeper database")
	}
	src, err := Open(path) // migrates it to this build's schema
	if err != nil {
		return 0, fmt.Errorf("the database in the backup can't be read: %w", err)
	}
	src.Close()

	// ATTACH belongs to a connection, so the whole restore runs on one.
	c, err := d.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `ATTACH DATABASE ? AS src`, path); err != nil {
		return 0, err
	}
	defer c.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE src`)

	tables, err := restorable(ctx, c)
	if err != nil {
		return 0, err
	}
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, t := range tables {
		cols, err := sharedColumns(ctx, tx, t)
		if err != nil {
			return 0, err
		}
		if replace {
			if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+quote(t)); err != nil {
				return 0, fmt.Errorf("%s: %w", t, err)
			}
		}
		list := strings.Join(cols, ", ")
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.`+quote(t)+` (`+list+`) SELECT `+list+` FROM src.`+quote(t)); err != nil {
			return 0, fmt.Errorf("%s: %w", t, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(tables), nil
}

// Reset empties every table but schema_migrations in one transaction, so the database is
// as a fresh install leaves it, then compacts the file and its WAL so the erased rows don't
// linger in free pages. It returns how many tables were emptied.
func (d *DB) Reset(ctx context.Context) (int, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	tables, err := strings1(ctx, tx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name != 'schema_migrations'`)
	if err != nil {
		return 0, err
	}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+quote(t)); err != nil {
			return 0, fmt.Errorf("%s: %w", t, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if _, err := d.ExecContext(ctx, `VACUUM`); err != nil {
		return len(tables), fmt.Errorf("compact the database: %w", err)
	}
	if _, err := d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return len(tables), fmt.Errorf("compact the database: %w", err)
	}
	return len(tables), nil
}

// isCryptKeeper reports whether the SQLite file at path is a CryptKeeper database, as uploaded
// (before Open would migrate any database into one): it has applied the base migration.
func isCryptKeeper(ctx context.Context, path string) bool {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE name = '001-base'`).Scan(&n)
	return err == nil && n > 0
}

// queryer is a connection or a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// restorable lists the tables both databases have, but notRestored, by name.
func restorable(ctx context.Context, q queryer) ([]string, error) {
	main, err := strings1(ctx, q, `SELECT name FROM main.sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	src, err := strings1(ctx, q, `SELECT name FROM src.sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range src {
		if slices.Contains(main, t) && !slices.Contains(notRestored, t) && !strings.HasPrefix(t, "sqlite_") {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out, nil
}

// sharedColumns are the quoted columns table t has in both databases.
func sharedColumns(ctx context.Context, q queryer, t string) ([]string, error) {
	main, err := strings1(ctx, q, `SELECT name FROM pragma_table_info(?, 'main')`, t)
	if err != nil {
		return nil, err
	}
	src, err := strings1(ctx, q, `SELECT name FROM pragma_table_info(?, 'src')`, t)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range main {
		if slices.Contains(src, c) {
			out = append(out, quote(c))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no columns in common", t)
	}
	return out, nil
}

// strings1 is the first column of every row of a query.
func strings1(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// quote is an SQL identifier in double quotes.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
