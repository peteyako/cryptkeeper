// Package activity stores and syncs per-source transaction history ("activity"): a SQLite
// backed Store, a Syncer that pulls from every core.ActivitySource on a schedule, and a
// matcher that links the two halves of a transfer between the user's own sources.
package activity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/store"
)

func init() {
	// Times are stored as Unix milliseconds. The full entry lives in json; the other
	// columns exist for filtering and ordering.
	store.AddMigration("activity-v1", `
CREATE TABLE activity (
	id        TEXT PRIMARY KEY,
	source_id TEXT NOT NULL,
	time      INTEGER NOT NULL,
	kind      TEXT NOT NULL,
	status    TEXT NOT NULL,
	assets    TEXT NOT NULL,
	json      TEXT NOT NULL
);
CREATE INDEX activity_time_idx ON activity (time DESC);
CREATE INDEX activity_source_time_idx ON activity (source_id, time DESC);
CREATE TABLE activity_state (
	source_id     TEXT PRIMARY KEY,
	state         BLOB,
	backfill_done INTEGER NOT NULL DEFAULT 0,
	last_sync     INTEGER,
	last_error    TEXT,
	synced_count  INTEGER NOT NULL DEFAULT 0
);`)
}

const (
	defaultLimit = 50
	maxLimit     = 500
)

// Store persists activity entries and per-source sync state in the CryptKeeper database.
type Store struct{ DB *store.DB }

// Filter selects entries for List. Zero values mean "no restriction".
type Filter struct {
	SourceIDs []string      // empty = all
	AssetKeys []string      // empty = all; matches any leg or fee asset
	Kinds     []core.TxKind // empty = all
	Before    time.Time     // zero = unbounded above (i.e. now); otherwise strictly older than
	Since     time.Time     // zero = no lower bound; otherwise at or after
	Limit     int           // default 50, max 500
}

// SyncStatus is the persisted sync state of one source.
type SyncStatus struct {
	SourceID     string
	BackfillDone bool
	LastSync     time.Time // zero when the source never synced successfully
	LastError    string    // from the most recent failed attempt; cleared by a success
	Count        int       // stored entries for the source as of the last sync
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// Upsert inserts or replaces entries by ID in one SQL transaction and returns how many
// IDs were new. A stored row's PeerID/PeerSource survive an update that does not set
// them (and, if the stored row was a matched transfer, so does Kind = transfer), so a
// source re-reporting a pending entry as confirmed does not undo transfer matching.
func (s *Store) Upsert(ctx context.Context, txs []core.Tx) (inserted int, err error) {
	fresh, err := s.upsert(ctx, txs)
	return len(fresh), err
}

// upsert is Upsert returning the entries (as stored) whose IDs were not present before.
func (s *Store) upsert(ctx context.Context, txs []core.Tx) ([]core.Tx, error) {
	if len(txs) == 0 {
		return nil, nil
	}
	sqlTx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer sqlTx.Rollback()

	sel, err := sqlTx.PrepareContext(ctx, `SELECT json FROM activity WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	defer sel.Close()
	put, err := sqlTx.PrepareContext(ctx, `
INSERT INTO activity (id, source_id, time, kind, status, assets, json) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET source_id = excluded.source_id, time = excluded.time,
	kind = excluded.kind, status = excluded.status, assets = excluded.assets, json = excluded.json`)
	if err != nil {
		return nil, err
	}
	defer put.Close()

	var fresh []core.Tx
	for _, t := range txs {
		if t.ID == "" {
			return nil, errors.New("activity: entry with empty ID")
		}
		var raw string
		switch err := sel.QueryRowContext(ctx, t.ID).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
			fresh = append(fresh, t)
		case err != nil:
			return nil, err
		default:
			var old core.Tx
			if json.Unmarshal([]byte(raw), &old) == nil && old.PeerID != "" {
				if t.PeerID == "" {
					t.PeerID = old.PeerID
					if old.Kind == core.TxTransfer {
						t.Kind = old.Kind
					}
				}
				if t.PeerSource == "" {
					t.PeerSource = old.PeerSource
				}
			}
		}
		blob, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		if _, err := put.ExecContext(ctx, t.ID, t.SourceID, ms(t.Time), string(t.Kind), string(t.Status), assetsColumn(t), string(blob)); err != nil {
			return nil, err
		}
	}
	if err := sqlTx.Commit(); err != nil {
		return nil, err
	}
	// fresh holds pre-merge copies, which is right: a new ID has nothing to merge with.
	return fresh, nil
}

// assetToken makes an asset key safe to embed in the space-separated assets column.
func assetToken(key string) string {
	return strings.Join(strings.Fields(key), "_")
}

// assetsColumn lists the distinct asset keys of all legs and the fee, space-separated.
func assetsColumn(t core.Tx) string {
	seen := map[string]bool{}
	var out []string
	add := func(l core.Leg) {
		k := assetToken(l.AssetKey)
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, l := range t.Legs {
		add(l)
	}
	if t.Fee != nil {
		add(*t.Fee)
	}
	return strings.Join(out, " ")
}

// where builds the WHERE clause (including the keyword, or empty) for a filter.
func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	if len(f.SourceIDs) > 0 {
		conds = append(conds, "source_id IN ("+placeholders(len(f.SourceIDs))+")")
		for _, id := range f.SourceIDs {
			args = append(args, id)
		}
	}
	if len(f.Kinds) > 0 {
		conds = append(conds, "kind IN ("+placeholders(len(f.Kinds))+")")
		for _, k := range f.Kinds {
			args = append(args, string(k))
		}
	}
	if len(f.AssetKeys) > 0 {
		parts := make([]string, len(f.AssetKeys))
		for i, k := range f.AssetKeys {
			parts[i] = "instr(' ' || assets || ' ', ?) > 0"
			args = append(args, " "+assetToken(k)+" ")
		}
		conds = append(conds, "("+strings.Join(parts, " OR ")+")")
	}
	if !f.Before.IsZero() {
		conds = append(conds, "time < ?")
		args = append(args, ms(f.Before))
	}
	if !f.Since.IsZero() {
		conds = append(conds, "time >= ?")
		args = append(args, ms(f.Since))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// List returns entries matching f, newest first (ties broken by ID, descending).
// An unset Before means "now": nothing is excluded for being recent.
func (s *Store) List(ctx context.Context, f Filter) ([]core.Tx, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = defaultLimit
	case limit > maxLimit:
		limit = maxLimit
	}
	where, args := f.where()
	return s.query(ctx, `SELECT json FROM activity`+where+` ORDER BY time DESC, id DESC LIMIT ?`, append(args, limit)...)
}

// All returns every stored entry for the given sources (all sources when empty), oldest
// first, which is the order cost-basis calculations want.
func (s *Store) All(ctx context.Context, sourceIDs []string) ([]core.Tx, error) {
	where, args := Filter{SourceIDs: sourceIDs}.where()
	return s.query(ctx, `SELECT json FROM activity`+where+` ORDER BY time ASC, id ASC`, args...)
}

// since returns entries at or after t from all sources, oldest first.
func (s *Store) since(ctx context.Context, t time.Time) ([]core.Tx, error) {
	where, args := Filter{Since: t}.where()
	return s.query(ctx, `SELECT json FROM activity`+where+` ORDER BY time ASC, id ASC`, args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]core.Tx, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Tx{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t core.Tx
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, fmt.Errorf("activity: corrupt row: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Get returns one entry by ID.
func (s *Store) Get(ctx context.Context, id string) (core.Tx, bool, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT json FROM activity WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Tx{}, false, nil
	}
	if err != nil {
		return core.Tx{}, false, err
	}
	var t core.Tx
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return core.Tx{}, false, fmt.Errorf("activity: corrupt row: %w", err)
	}
	return t, true, nil
}

// DeleteSource removes a source's entries and its sync state (for a removed source).
func (s *Store) DeleteSource(ctx context.Context, sourceID string) error {
	sqlTx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer sqlTx.Rollback()
	if _, err := sqlTx.ExecContext(ctx, `DELETE FROM activity WHERE source_id = ?`, sourceID); err != nil {
		return err
	}
	if _, err := sqlTx.ExecContext(ctx, `DELETE FROM activity_state WHERE source_id = ?`, sourceID); err != nil {
		return err
	}
	return sqlTx.Commit()
}

// Statuses returns the sync state of every source that has synced or failed at least once.
func (s *Store) Statuses(ctx context.Context) (map[string]SyncStatus, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT source_id, backfill_done, last_sync, last_error, synced_count FROM activity_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SyncStatus{}
	for rows.Next() {
		var (
			st       SyncStatus
			done     int
			lastSync sql.NullInt64
			lastErr  sql.NullString
		)
		if err := rows.Scan(&st.SourceID, &done, &lastSync, &lastErr, &st.Count); err != nil {
			return nil, err
		}
		st.BackfillDone = done != 0
		if lastSync.Valid {
			st.LastSync = time.UnixMilli(lastSync.Int64)
		}
		st.LastError = lastErr.String
		out[st.SourceID] = st
	}
	return out, rows.Err()
}

// loadState returns the opaque source state and backfill flag saved by the last success.
func (s *Store) loadState(ctx context.Context, sourceID string) (state []byte, backfillDone bool, err error) {
	var done int
	err = s.DB.QueryRowContext(ctx, `SELECT state, backfill_done FROM activity_state WHERE source_id = ?`, sourceID).Scan(&state, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return state, done != 0, err
}

// saveProgress records a successful fetch: new state, flag, time, and clears last_error.
func (s *Store) saveProgress(ctx context.Context, sourceID string, state []byte, backfillDone bool, at time.Time) error {
	done := 0
	if backfillDone {
		done = 1
	}
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO activity_state (source_id, state, backfill_done, last_sync, last_error, synced_count)
VALUES (?, ?, ?, ?, NULL, (SELECT COUNT(*) FROM activity WHERE source_id = ?))
ON CONFLICT(source_id) DO UPDATE SET state = excluded.state, backfill_done = excluded.backfill_done,
	last_sync = excluded.last_sync, last_error = NULL, synced_count = excluded.synced_count`,
		sourceID, state, done, ms(at), sourceID)
	return err
}

// saveError records a failed attempt without touching state, backfill flag or last_sync.
func (s *Store) saveError(ctx context.Context, sourceID, msg string) error {
	const maxLen = 1000
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO activity_state (source_id, last_error) VALUES (?, ?)
ON CONFLICT(source_id) DO UPDATE SET last_error = excluded.last_error`, sourceID, msg)
	return err
}
