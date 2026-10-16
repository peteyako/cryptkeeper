package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/activity"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/web/templates"
)

const activityPageSize = 50

// activityFilter is the Activity page's filter state (query params on a full load,
// Datastar signals on a patch).
type activityFilter struct {
	Source string `json:"actSource"`
	Asset  string `json:"actAsset"`
	Kind   string `json:"actKind"`
	Range  string `json:"actRange"` // period key: "", 7d, 30d, 90d, mtd, ytd, y2025, custom
	From   string `json:"actFrom"`  // custom period, YYYY-MM-DD inclusive
	To     string `json:"actTo"`
}

func filterFromQuery(r *http.Request) activityFilter {
	q := r.URL.Query()
	return activityFilter{Source: q.Get("source"), Asset: q.Get("asset"), Kind: q.Get("kind"),
		Range: q.Get("range"), From: q.Get("from"), To: q.Get("to")}
}

func (f activityFilter) period() activity.Period {
	return activity.Period{Key: f.Range, From: f.From, To: f.To}
}

// toStore turns the filters into a store query. before is the paging cursor (strictly
// older than); the period's end applies too, whichever is earlier.
func (f activityFilter) toStore(before time.Time, limit int, now time.Time) activity.Filter {
	since, until := f.period().Bounds(now, templates.Location)
	if !until.IsZero() && (before.IsZero() || until.Before(before)) {
		before = until
	}
	af := activity.Filter{Before: before, Since: since, Limit: limit}
	if f.Source != "" {
		af.SourceIDs = []string{f.Source}
	}
	if f.Asset != "" {
		af.AssetKeys = []string{f.Asset}
	}
	if f.Kind != "" {
		af.Kinds = []core.TxKind{core.TxKind(f.Kind)}
	}
	return af
}

// sourceLabels maps source ids (including CSV-import pseudo-sources) to display labels.
func (s *Server) sourceLabels() (map[string]string, map[string]core.Kind) {
	labels := map[string]string{"coinbase-csv": "Coinbase CSV", "kraken-csv": "Kraken CSV"}
	kinds := map[string]core.Kind{"coinbase-csv": core.KindExchange, "kraken-csv": core.KindExchange}
	for _, d := range s.Registry.Defs() {
		labels[d.ID], kinds[d.ID] = d.Label, d.Kind
	}
	return labels, kinds
}

func (s *Server) activityRows(txs []core.Tx) []templates.ActivityRow {
	labels, kinds := s.sourceLabels()
	rows := make([]templates.ActivityRow, 0, len(txs))
	for _, tx := range txs {
		label := labels[tx.SourceID]
		if label == "" {
			label = tx.SourceID
		}
		rows = append(rows, templates.ActivityRow{Tx: tx, SourceLabel: label, SourceKind: kinds[tx.SourceID], PeerLabel: labels[tx.PeerSource]})
	}
	return rows
}

// cursor encodes a "before" time for the next page as unix nanoseconds.
func cursor(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func parseCursor(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (s *Server) syncRows(ctx context.Context) []templates.SyncRow {
	if s.Activity == nil {
		return nil
	}
	st, err := s.Activity.Statuses(ctx)
	if err != nil {
		s.Log.Warn("activity statuses", "err", err)
	}
	var rows []templates.SyncRow
	for _, d := range s.Registry.Defs() {
		if d.Type == sources.TypeManual {
			continue
		}
		row := templates.SyncRow{ID: d.ID, Label: d.Label, Kind: d.Kind}
		if x, ok := st[d.ID]; ok {
			row.Count, row.BackfillDone, row.LastSync, row.LastError = x.Count, x.BackfillDone, x.LastSync, x.LastError
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) activityPage(r *http.Request, f activityFilter, before time.Time) templates.ActivityPage {
	p := templates.ActivityPage{
		Shell:    s.shell(r, "Activity", "activity", "all", ""),
		SourceID: f.Source, AssetKey: f.Asset, Kind: f.Kind,
		Period: f.Range, From: f.From, To: f.To, PeriodLabel: f.period().Label(),
		Sync: s.syncRows(r.Context()), Now: s.Now(),
	}
	p.Sources = s.walletOptions(false) // manual holdings have no activity
	p.Assets = s.assetOptions()
	if s.Activity == nil {
		return p
	}
	if ys, err := s.Activity.Years(r.Context(), templates.Location); err == nil {
		p.Years = ys
	}
	txs, err := s.Activity.List(r.Context(), f.toStore(before, activityPageSize+1, s.Now()))
	if err != nil {
		s.Log.Warn("activity list", "err", err)
	}
	if len(txs) > activityPageSize {
		txs = txs[:activityPageSize]
		p.More = cursor(txs[len(txs)-1].Time)
	}
	p.Rows = s.activityRows(txs)
	return p
}

func (s *Server) activityFull(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, templates.ActivityFull(s.activityPage(r, filterFromQuery(r), time.Time{})))
}

// activityList patches the list after a filter change, or appends the next page.
func (s *Server) activityList(w http.ResponseWriter, r *http.Request) {
	var f activityFilter
	_ = datastar.ReadSignals(r, &f)
	before := parseCursor(r.URL.Query().Get("before"))
	p := s.activityPage(r, f, before)
	sse := datastar.NewSSE(w, r)
	if before.IsZero() {
		_ = sse.PatchElementTempl(templates.ActivityList(p))
		return
	}
	_ = sse.PatchElementTempl(templates.ActivityRows(p.Rows, p.Now), datastar.WithSelectorID("activity-rows"), datastar.WithModeAppend())
	_ = sse.PatchElementTempl(templates.ActivityMore(p))
}

// walletActivity fills the wallet page's Activity tab.
func (s *Server) walletActivity(ctx context.Context, p *templates.WalletPage) {
	if s.Activity == nil {
		return
	}
	txs, err := s.Activity.List(ctx, activity.Filter{SourceIDs: []string{p.W.ID}, Limit: 26})
	if err != nil {
		s.Log.Warn("wallet activity", "err", err)
		return
	}
	if len(txs) > 25 {
		txs, p.ActivityMore = txs[:25], true
	}
	p.Activity = s.activityRows(txs)
}
