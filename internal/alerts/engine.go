package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/store"
)

// PortfolioValuer returns the current value of a portfolio ("all" or a portfolio id) in
// snap, with that portfolio's filters applied. ok=false means "not comparable right now"
// (unknown portfolio, stale data), and the rule is skipped.
type PortfolioValuer func(portfolioID string, snap portfolio.Snapshot) (value float64, ok bool)

// HistoryValuer returns a portfolio's value at a past time from recorded history.
// ok=false means history does not reach that far back.
type HistoryValuer func(ctx context.Context, portfolioID string, at time.Time) (value float64, ok bool)

// Engine evaluates the alert rules. Call OnSnapshot after every refresh and OnActivity with
// the newly inserted transactions. Both are safe for concurrent use and never panic or
// return errors: failures are logged and, for delivery, recorded in the alert log.
//
// Delivery is at-most-once. A fired event is consumed (cooldown started, drop baseline
// updated, price disarmed) whether or not the notifier succeeds, so an ntfy outage cannot
// turn into a retry storm; the failure shows up in Recent with its error.
type Engine struct {
	Rules    *RuleStore
	DB       *store.DB // alert_state and alert_log live here
	Notifier Notifier
	Redact   bool   // keep amounts and values out of notification text
	BaseURL  string // for Click links, e.g. "http://vault.tailnet:8787"; "" = no click

	Portfolio PortfolioValuer
	History   HistoryValuer

	// Labels maps a wallet id to its display name; "" falls back to the snapshot's label.
	Labels func(walletID string) string
	// PortfolioLabel maps a portfolio id to its display name; "" falls back to
	// "All holdings" for "all" and to the id itself.
	PortfolioLabel func(portfolioID string) string
	// MaxTxAge ignores transactions older than this in OnActivity, so a first sync that
	// backfills years of history does not look like a burst of new sends. 0 means 72h,
	// negative means no limit.
	MaxTxAge time.Duration

	Now func() time.Time // default time.Now
	Log *slog.Logger     // default slog.Default()

	mu sync.Mutex // serializes evaluation: state is read-modify-write
}

const (
	sendTimeout     = 20 * time.Second
	defaultMaxTxAge = 72 * time.Hour
	// hysteresis is how far a price must retreat past its threshold before the rule re-arms.
	hysteresis = 0.02
	// dropEpsilon is the smallest balance decrease that counts as a drop.
	dropEpsilon = 1e-9
	// seenKeep bounds the per-rule memory of already announced transaction ids.
	seenKeep = 200
	// maxActivityLines caps the lines of one large_activity notification.
	maxActivityLines = 5
)

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// ---- entry points ----

// OnSnapshot evaluates the snapshot rules (balance_drop, price, portfolio_move, source_down).
func (e *Engine) OnSnapshot(ctx context.Context, snap portfolio.Snapshot) {
	if e.Rules == nil || e.DB == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	rules := e.Rules.List()
	e.gcState(ctx, rules)
	now := e.now()
	for _, r := range rules {
		if !r.IsEnabled() {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		e.safely(r.ID, func() {
			switch r.Type {
			case TypeBalanceDrop:
				e.evalBalanceDrop(ctx, r, snap)
			case TypePrice:
				e.evalPrice(ctx, r, snap, now)
			case TypePortfolioMove:
				e.evalPortfolioMove(ctx, r, snap, now)
			case TypeSourceDown:
				e.evalSourceDown(ctx, r, snap, now)
			}
		})
	}
}

// OnActivity evaluates large_activity rules on newly inserted transactions. kinds maps a
// wallet id to its custody kind; wallets missing from it are ignored.
func (e *Engine) OnActivity(ctx context.Context, txs []core.Tx, kinds map[string]core.Kind) {
	if len(txs) == 0 || e.Rules == nil || e.DB == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	for _, r := range e.Rules.List() {
		if !r.IsEnabled() || r.Type != TypeLargeActivity {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		e.safely(r.ID, func() { e.evalLargeActivity(ctx, r, txs, kinds, now) })
	}
}

// Test sends a test notification and logs it as rule "test". It returns the delivery error.
func (e *Engine) Test(ctx context.Context) error {
	if e.DB == nil {
		return errors.New("alerts: no database")
	}
	n := Notification{
		RuleID:   "test",
		Title:    "Vault test",
		Body:     "Test notification from Vault. Alerts are working.",
		Priority: 3,
		Tags:     []string{"white_check_mark"},
		Click:    e.click("/alerts"),
		Time:     e.now(),
	}
	err := e.send(ctx, n)
	e.record(ctx, n, err)
	return err
}

// safely keeps a panic in one rule (say, in a valuer callback) from taking down the
// refresh loop or the other rules.
func (e *Engine) safely(ruleID string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			e.log().Error("alerts: rule evaluation panicked", "rule", ruleID, "panic", r)
		}
	}()
	f()
}

// ---- delivery ----

// fire delivers n and records it in the log.
func (e *Engine) fire(ctx context.Context, n Notification) {
	n.Time = e.now()
	err := e.send(ctx, n)
	if err != nil {
		e.log().Warn("alerts: delivery failed", "rule", n.RuleID, "err", err)
	}
	e.record(ctx, n, err)
}

func (e *Engine) send(ctx context.Context, n Notification) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("notifier panic: %v", r)
		}
	}()
	if e.Notifier == nil {
		return errors.New("no notifier configured")
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return e.Notifier.Send(ctx, n)
}

// click builds a link into the web UI, or "" without a BaseURL.
func (e *Engine) click(path string) string {
	if e.BaseURL == "" {
		return ""
	}
	return strings.TrimRight(e.BaseURL, "/") + path
}

func (e *Engine) walletLabel(id, fallback string) string {
	if e.Labels != nil {
		if s := e.Labels(id); s != "" {
			return s
		}
	}
	if fallback != "" {
		return fallback
	}
	return id
}

func (e *Engine) portfolioLabel(id string) string {
	if e.PortfolioLabel != nil {
		if s := e.PortfolioLabel(id); s != "" {
			return s
		}
	}
	if id == AllPortfolios {
		return "All holdings"
	}
	return id
}

func titleOr(r Rule, def string) string {
	if r.Name != "" {
		return r.Name
	}
	return def
}

// ---- cooldown ----

// cooledDown reports whether r may fire now given its last firing.
func cooledDown(st *ruleState, r Rule, now time.Time) bool {
	cd := r.CooldownDuration()
	if cd <= 0 {
		return true
	}
	var last time.Time
	if !st.get("last_fired", &last) {
		return true
	}
	return now.Sub(last) >= cd
}

func markFired(st *ruleState, now time.Time) { st.put("last_fired", now) }

// ---- balance_drop ----

type baseAsset struct {
	Sym string  `json:"s"`
	Amt float64 `json:"a"`
}

// baseline is a wallet's per-asset amounts, keyed by asset key.
type baseline map[string]baseAsset

// amountsOf sums a wallet's positive holdings per asset. Hidden rows count too, so an asset
// that merely moves between the visible and hidden lists (it became unpriced, say) is not a
// drop. Debts (negative amounts) are not balances and are ignored.
func amountsOf(w portfolio.WalletView) baseline {
	out := baseline{}
	for _, rows := range [][]portfolio.HoldingRow{w.Rows, w.HiddenRows} {
		for _, row := range rows {
			if row.Amount <= 0 {
				continue
			}
			key := row.AssetKey
			if key == "" {
				key = row.Symbol
			}
			if key == "" {
				continue
			}
			a := out[key]
			if a.Sym == "" {
				a.Sym = row.Symbol
				if a.Sym == "" {
					a.Sym = key
				}
			}
			a.Amt += row.Amount
			out[key] = a
		}
	}
	return out
}

type drop struct {
	sym      string
	was, now float64
	pct      float64
}

// findDrops lists the assets whose amount fell by more than minPct percent (and by more
// than dropEpsilon) since base. An asset missing from cur counts as 0.
func findDrops(base, cur baseline, minPct float64) []drop {
	var out []drop
	for key, b := range base {
		if b.Amt <= 0 {
			continue
		}
		now := cur[key].Amt
		d := b.Amt - now
		if d <= dropEpsilon {
			continue
		}
		pct := d * 100 / b.Amt
		if pct <= minPct {
			continue
		}
		out = append(out, drop{sym: b.Sym, was: b.Amt, now: now, pct: pct})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sym < out[j].sym })
	return out
}

func watchesWallet(r Rule, w portfolio.WalletView) bool {
	if len(r.Wallets) > 0 {
		return slices.Contains(r.Wallets, w.ID)
	}
	return w.Kind != core.KindExchange
}

func (e *Engine) evalBalanceDrop(ctx context.Context, r Rule, snap portfolio.Snapshot) {
	st, err := e.loadState(ctx, r.ID)
	if err != nil {
		e.log().Warn("alerts: loading state failed", "rule", r.ID, "err", err)
		return
	}
	present := map[string]bool{}
	for _, w := range snap.Wallets {
		if !watchesWallet(r, w) {
			continue
		}
		present[w.ID] = true
		if w.Status != portfolio.StatusSynced {
			continue // neither compared nor used as the new baseline
		}
		key := "wallet:" + w.ID
		cur := amountsOf(w)
		var base baseline
		if st.get(key, &base) {
			if drops := findDrops(base, cur, r.MinPct); len(drops) > 0 {
				e.fire(ctx, e.dropNotification(r, w, drops))
			}
		}
		st.put(key, cur)
	}
	for _, k := range st.keys("wallet:") {
		if !present[strings.TrimPrefix(k, "wallet:")] {
			st.del(k) // removed or no longer watched: do not compare against an old baseline later
		}
	}
	e.flushState(ctx, st)
}

func (e *Engine) dropNotification(r Rule, w portfolio.WalletView, drops []drop) Notification {
	label := e.walletLabel(w.ID, w.Label)
	parts := make([]string, 0, len(drops))
	for _, d := range drops {
		p := fmt.Sprintf("%s balance dropped %s%%", d.sym, fmtPct(d.pct))
		if !e.Redact {
			p += fmt.Sprintf(" (%s → %s)", fmtAmount(d.was), fmtAmount(d.now))
		}
		parts = append(parts, p)
	}
	return Notification{
		RuleID:   r.ID,
		Title:    titleOr(r, "Balance drop"),
		Body:     label + ": " + strings.Join(parts, "; "),
		Priority: 5,
		Tags:     []string{"rotating_light"},
		Click:    e.click("/w/" + url.PathEscape(w.ID)),
	}
}

// ---- price ----

type priceState struct {
	Threshold float64 `json:"threshold"`
	Dir       string  `json:"dir"`
	Armed     bool    `json:"armed"`
}

func (e *Engine) evalPrice(ctx context.Context, r Rule, snap portfolio.Snapshot, now time.Time) {
	p, ok := snap.Prices[r.Asset]
	if !ok || p.Stale || !(p.USD > 0) {
		return
	}
	st, err := e.loadState(ctx, r.ID)
	if err != nil {
		e.log().Warn("alerts: loading state failed", "rule", r.ID, "err", err)
		return
	}

	thr, dir := r.Above, "above"
	beyond := p.USD >= thr
	rearm := p.USD <= thr*(1-hysteresis)
	if r.Below != 0 {
		thr, dir = r.Below, "below"
		beyond = p.USD <= thr
		rearm = p.USD >= thr*(1+hysteresis)
	}

	var ps priceState
	if !st.get("price", &ps) || ps.Threshold != thr || ps.Dir != dir {
		// First sight, or the threshold was edited: only a later crossing counts, so a
		// price that is already beyond the line does not alert.
		st.put("price", priceState{Threshold: thr, Dir: dir, Armed: !beyond})
		e.flushState(ctx, st)
		return
	}
	switch {
	case ps.Armed && beyond:
		ps.Armed = false // consumed, even if the cooldown swallows the alert
		if cooledDown(st, r, now) {
			e.fire(ctx, e.priceNotification(r, snap, dir, thr, p.USD))
			markFired(st, now)
		}
	case !ps.Armed && rearm:
		ps.Armed = true
	}
	st.put("price", ps)
	e.flushState(ctx, st)
}

func assetSymbol(snap portfolio.Snapshot, key string) string {
	for _, a := range snap.Assets {
		if a.AssetKey == key && a.Symbol != "" {
			return a.Symbol
		}
	}
	return key
}

func (e *Engine) priceNotification(r Rule, snap portfolio.Snapshot, dir string, thr, price float64) Notification {
	sym := assetSymbol(snap, r.Asset)
	body := fmt.Sprintf("%s crossed %s your alert", sym, dir)
	if !e.Redact {
		body = fmt.Sprintf("%s crossed %s %s (now %s)", sym, dir, fmtUSD(thr), fmtUSD(price))
	}
	return Notification{
		RuleID:   r.ID,
		Title:    titleOr(r, sym+" price alert"),
		Body:     body,
		Priority: 3,
		Tags:     []string{"chart_with_upwards_trend"},
		Click:    e.click("/"),
	}
}

// ---- portfolio_move ----

func (e *Engine) evalPortfolioMove(ctx context.Context, r Rule, snap portfolio.Snapshot, now time.Time) {
	if e.Portfolio == nil || e.History == nil {
		return
	}
	st, err := e.loadState(ctx, r.ID)
	if err != nil {
		e.log().Warn("alerts: loading state failed", "rule", r.ID, "err", err)
		return
	}
	if !cooledDown(st, r, now) {
		return
	}
	cur, ok := e.Portfolio(r.Portfolio, snap)
	if !ok {
		return
	}
	window := r.WindowDuration()
	past, ok := e.History(ctx, r.Portfolio, now.Add(-window))
	if !ok || !(past > 0) {
		return
	}
	pct := (cur - past) * 100 / past
	if math.IsNaN(pct) || math.Abs(pct) < r.Pct {
		return
	}
	e.fire(ctx, e.moveNotification(r, window, cur, past, pct))
	markFired(st, now)
	e.flushState(ctx, st)
}

func (e *Engine) moveNotification(r Rule, window time.Duration, cur, past, pct float64) Notification {
	sign, prio, tag := "+", 3, "chart_with_upwards_trend"
	if pct < 0 {
		sign, prio, tag = "−", 4, "chart_with_downwards_trend"
	}
	body := fmt.Sprintf("%s moved %s%s%% in %s", e.portfolioLabel(r.Portfolio), sign, fmtPct(pct), fmtWindow(window))
	if !e.Redact {
		body += fmt.Sprintf(" (%s → %s)", fmtUSD(past), fmtUSD(cur))
	}
	path := "/"
	if r.Portfolio != AllPortfolios {
		path = "/p/" + url.PathEscape(r.Portfolio)
	}
	return Notification{
		RuleID:   r.ID,
		Title:    titleOr(r, "Portfolio move"),
		Body:     body,
		Priority: prio,
		Tags:     []string{tag},
		Click:    e.click(path),
	}
}

// ---- source_down ----

type downState struct {
	Since time.Time `json:"since"`
	Fired bool      `json:"fired"`
}

func (e *Engine) evalSourceDown(ctx context.Context, r Rule, snap portfolio.Snapshot, now time.Time) {
	st, err := e.loadState(ctx, r.ID)
	if err != nil {
		e.log().Warn("alerts: loading state failed", "rule", r.ID, "err", err)
		return
	}
	threshold := time.Duration(r.Minutes) * time.Minute
	present := map[string]bool{}
	for _, w := range snap.Wallets {
		present[w.ID] = true
		key := "down:" + w.ID
		switch w.Status {
		case portfolio.StatusSynced:
			st.del(key) // recovered: the next outage is a new event
		case portfolio.StatusStale, portfolio.StatusError:
			var ds downState
			if !st.get(key, &ds) {
				ds = downState{Since: now}
			}
			if dur := now.Sub(ds.Since); !ds.Fired && dur >= threshold {
				ds.Fired = true
				e.fire(ctx, e.downNotification(r, w, dur))
			}
			st.put(key, ds)
		}
	}
	for _, k := range st.keys("down:") {
		if !present[strings.TrimPrefix(k, "down:")] {
			st.del(k)
		}
	}
	e.flushState(ctx, st)
}

func (e *Engine) downNotification(r Rule, w portfolio.WalletView, dur time.Duration) Notification {
	what := "failing"
	if w.Status == portfolio.StatusStale {
		what = "stale"
	}
	return Notification{
		RuleID:   r.ID,
		Title:    titleOr(r, "Source down"),
		Body:     fmt.Sprintf("%s has been %s for %s", e.walletLabel(w.ID, w.Label), what, fmtOutage(dur)),
		Priority: 4,
		Tags:     []string{"warning"},
		Click:    e.click("/w/" + url.PathEscape(w.ID)),
	}
}

// ---- large_activity ----

type bigSend struct {
	tx    core.Tx
	usd   float64
	known bool
}

// legsUSD adds up the magnitudes of the legs' USD values; known is false when no leg has one.
func legsUSD(tx core.Tx) (total float64, known bool) {
	for _, l := range tx.Legs {
		if l.USD != nil {
			total += math.Abs(*l.USD)
			known = true
		}
	}
	return total, known
}

func (e *Engine) evalLargeActivity(ctx context.Context, r Rule, txs []core.Tx, kinds map[string]core.Kind, now time.Time) {
	st, err := e.loadState(ctx, r.ID)
	if err != nil {
		e.log().Warn("alerts: loading state failed", "rule", r.ID, "err", err)
		return
	}
	maxAge := e.MaxTxAge
	if maxAge == 0 {
		maxAge = defaultMaxTxAge
	}
	var seen []string
	st.get("seen", &seen)

	var hits []bigSend
	for _, tx := range txs {
		if tx.Kind != core.TxSend || tx.Status == core.TxFailed {
			continue
		}
		if k := kinds[tx.SourceID]; k != core.KindHot && k != core.KindHardware {
			continue
		}
		id := tx.ID
		if id == "" {
			id = core.TxID(tx.SourceID, tx.ExternalID)
		}
		if slices.Contains(seen, id) {
			continue
		}
		if maxAge > 0 && !tx.Time.IsZero() && now.Sub(tx.Time) > maxAge {
			continue
		}
		usd, known := legsUSD(tx)
		if r.MinUsd > 0 && (!known || usd < r.MinUsd) {
			continue
		}
		hits = append(hits, bigSend{tx, usd, known})
		seen = append(seen, id)
	}
	if len(hits) == 0 {
		return
	}
	if len(seen) > seenKeep {
		seen = seen[len(seen)-seenKeep:]
	}
	st.put("seen", seen)
	if cooledDown(st, r, now) {
		e.fire(ctx, e.sendNotification(r, hits))
		markFired(st, now)
	}
	e.flushState(ctx, st)
}

// sendNotification groups every qualifying send of one batch into a single notification,
// so a cooldown cannot hide the later sends of a burst.
func (e *Engine) sendNotification(r Rule, hits []bigSend) Notification {
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		lines = append(lines, e.sendLine(h))
	}
	if len(lines) > maxActivityLines {
		extra := len(lines) - maxActivityLines
		lines = append(lines[:maxActivityLines], fmt.Sprintf("+%d more", extra))
	}
	def := "Large send"
	if len(hits) > 1 {
		def = fmt.Sprintf("%d large sends", len(hits))
	}
	return Notification{
		RuleID:   r.ID,
		Title:    titleOr(r, def),
		Body:     strings.Join(lines, "\n"),
		Priority: 4,
		Tags:     []string{"outbox_tray"},
		Click:    e.click("/activity"),
	}
}

func (e *Engine) sendLine(h bigSend) string {
	from := e.walletLabel(h.tx.SourceID, "")
	var what []string
	for _, l := range h.tx.Legs {
		if l.Amount == 0 {
			continue
		}
		sym := l.Symbol
		if sym == "" {
			sym = l.AssetKey
		}
		if e.Redact {
			if !slices.Contains(what, sym) {
				what = append(what, sym)
			}
		} else {
			what = append(what, fmtAmount(math.Abs(l.Amount))+" "+sym)
		}
	}
	if len(what) == 0 {
		what = []string{"funds"}
	}
	sep := ", "
	if !e.Redact {
		sep = " + "
	}
	line := "Sent " + strings.Join(what, sep)
	if !e.Redact && h.known {
		line += " (" + fmtUSD(h.usd) + ")"
	}
	return line + " from " + from
}
