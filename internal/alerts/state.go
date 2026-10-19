package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"cryptkeeper/internal/store"
)

func init() {
	// alert_state holds per-rule engine state (baselines, armed flags, cooldown clocks,
	// outage clocks) as JSON, keyed by (rule_id, key). alert_log is the history of fired
	// alerts, including delivery errors.
	store.AddMigration("alerts-v1", `
CREATE TABLE alert_state (
	rule_id TEXT NOT NULL,
	key     TEXT NOT NULL,
	json    TEXT NOT NULL,
	PRIMARY KEY (rule_id, key)
);
CREATE TABLE alert_log (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      INTEGER NOT NULL,
	rule_id TEXT NOT NULL,
	title   TEXT NOT NULL,
	body    TEXT NOT NULL,
	err     TEXT NOT NULL DEFAULT ''
);`)
}

// ruleState is one rule's rows of alert_state, loaded into memory, modified, and written
// back in one transaction by flush. Values are JSON.
type ruleState struct {
	ruleID string
	vals   map[string][]byte
	dirty  map[string]bool
}

// loadState reads every state row of one rule. The rows are fully consumed and closed
// before returning: the database has a single connection, so a lingering cursor would
// deadlock the next statement.
func (e *Engine) loadState(ctx context.Context, ruleID string) (*ruleState, error) {
	rows, err := e.DB.QueryContext(ctx, `SELECT key, json FROM alert_state WHERE rule_id = ?`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st := &ruleState{ruleID: ruleID, vals: map[string][]byte{}, dirty: map[string]bool{}}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		st.vals[k] = []byte(v)
	}
	return st, rows.Err()
}

// get decodes the value at key into out; false if absent or unreadable.
func (s *ruleState) get(key string, out any) bool {
	raw, ok := s.vals[key]
	return ok && json.Unmarshal(raw, out) == nil
}

// put stores v at key; unchanged values are not written.
func (s *ruleState) put(key string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if old, ok := s.vals[key]; ok && bytes.Equal(old, raw) {
		return
	}
	s.vals[key] = raw
	s.dirty[key] = true
}

func (s *ruleState) del(key string) {
	if _, ok := s.vals[key]; !ok {
		return
	}
	delete(s.vals, key)
	s.dirty[key] = true
}

// keys lists the keys with the given prefix, sorted.
func (s *ruleState) keys(prefix string) []string {
	var out []string
	for k := range s.vals {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// flushState writes the changes of st. It ignores cancellation of ctx on purpose: an alert
// may already have been sent, and losing its state would send it again.
func (e *Engine) flushState(ctx context.Context, st *ruleState) {
	if len(st.dirty) == 0 {
		return
	}
	if err := e.writeState(context.WithoutCancel(ctx), st); err != nil {
		e.log().Warn("alerts: saving state failed", "rule", st.ruleID, "err", err)
		return
	}
	st.dirty = map[string]bool{}
}

func (e *Engine) writeState(ctx context.Context, st *ruleState) error {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for key := range st.dirty {
		if v, ok := st.vals[key]; ok {
			_, err = tx.ExecContext(ctx, `INSERT INTO alert_state (rule_id, key, json) VALUES (?, ?, ?)
				ON CONFLICT(rule_id, key) DO UPDATE SET json = excluded.json`, st.ruleID, key, string(v))
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM alert_state WHERE rule_id = ? AND key = ?`, st.ruleID, key)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// gcState deletes the state of rules that no longer exist or are disabled, so a rule that
// is switched back on starts from a clean slate instead of comparing with an old baseline.
func (e *Engine) gcState(ctx context.Context, rules []Rule) {
	active := map[string]bool{}
	for _, r := range rules {
		if r.IsEnabled() {
			active[r.ID] = true
		}
	}
	ids, err := e.stateRuleIDs(ctx)
	if err != nil {
		e.log().Warn("alerts: listing state failed", "err", err)
		return
	}
	for _, id := range ids {
		if active[id] {
			continue
		}
		if _, err := e.DB.ExecContext(context.WithoutCancel(ctx), `DELETE FROM alert_state WHERE rule_id = ?`, id); err != nil {
			e.log().Warn("alerts: clearing state failed", "rule", id, "err", err)
		}
	}
}

func (e *Engine) stateRuleIDs(ctx context.Context) ([]string, error) {
	rows, err := e.DB.QueryContext(ctx, `SELECT DISTINCT rule_id FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// logKeep is how many rows of alert_log are retained.
const logKeep = 500

// record appends a fired alert (and its delivery error, if any) to alert_log.
func (e *Engine) record(ctx context.Context, n Notification, sendErr error) {
	ctx = context.WithoutCancel(ctx)
	errText := ""
	if sendErr != nil {
		errText = sendErr.Error()
	}
	if _, err := e.DB.ExecContext(ctx, `INSERT INTO alert_log (ts, rule_id, title, body, err) VALUES (?, ?, ?, ?, ?)`,
		n.Time.UnixMilli(), n.RuleID, n.Title, n.Body, errText); err != nil {
		e.log().Warn("alerts: logging failed", "rule", n.RuleID, "err", err)
		return
	}
	if _, err := e.DB.ExecContext(ctx, `DELETE FROM alert_log WHERE id NOT IN (SELECT id FROM alert_log ORDER BY id DESC LIMIT ?)`, logKeep); err != nil {
		e.log().Warn("alerts: pruning log failed", "err", err)
	}
}

// LogEntry is one fired alert as shown on the alerts page. Err is the delivery error, empty
// when the notification went out.
type LogEntry struct {
	Time   time.Time
	RuleID string
	Title  string
	Body   string
	Err    string
}

// Recent returns the last n fired alerts, newest first (including ones whose delivery failed).
func (e *Engine) Recent(ctx context.Context, n int) ([]LogEntry, error) {
	if e.DB == nil {
		return nil, fmt.Errorf("alerts: no database")
	}
	if n <= 0 {
		return nil, nil
	}
	rows, err := e.DB.QueryContext(ctx, `SELECT ts, rule_id, title, body, err FROM alert_log ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var (
			ts int64
			le LogEntry
		)
		if err := rows.Scan(&ts, &le.RuleID, &le.Title, &le.Body, &le.Err); err != nil {
			return nil, err
		}
		le.Time = time.UnixMilli(ts).UTC()
		out = append(out, le)
	}
	return out, rows.Err()
}
