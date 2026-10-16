package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/export"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/web/templates"
)

// costCache keeps the last cost-basis computation briefly: the overview's P&L column would
// otherwise recompute it from all activity on every live push.
type costCache struct {
	mu  sync.Mutex
	at  time.Time
	res costbasis.Result
	ok  bool
}

func (c *costCache) invalidate() { c.mu.Lock(); c.ok = false; c.mu.Unlock() }

// importedSourceType maps CSV pseudo-sources to the live source type they duplicate.
var importedSourceType = map[string]string{"coinbase-csv": sources.TypeCoinbase, "kraken-csv": sources.TypeKraken}

// dedupeImported drops CSV-imported entries that the live API already synced (same
// external id from a source of the same type), so a trade isn't counted twice.
func dedupeImported(txs []core.Tx, typeOf map[string]string) []core.Tx {
	seen := map[string]bool{}
	for _, tx := range txs {
		if t := typeOf[tx.SourceID]; t != "" {
			seen[t+"|"+tx.ExternalID] = true
		}
	}
	out := txs[:0:0]
	for _, tx := range txs {
		if t, ok := importedSourceType[tx.SourceID]; ok && seen[t+"|"+tx.ExternalID] {
			continue
		}
		out = append(out, tx)
	}
	return out
}

// costResult computes average-cost results from all activity plus manual entries.
func (s *Server) costResult(ctx context.Context) (costbasis.Result, bool) {
	if s.Activity == nil || s.CostBasis == nil {
		return costbasis.Result{}, false
	}
	c := &s.costs
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && s.Now().Sub(c.at) < time.Minute {
		return c.res, true
	}
	txs, err := s.Activity.All(ctx, nil)
	if err != nil {
		s.Log.Warn("cost basis: load activity", "err", err)
		return costbasis.Result{}, false
	}
	typeOf := map[string]string{}
	for _, d := range s.Registry.Defs() {
		typeOf[d.ID] = d.Type
	}
	c.res, c.at, c.ok = costbasis.Compute(dedupeImported(txs, typeOf), s.CostBasis.Get()), s.Now(), true
	return c.res, true
}

// pnlRows pairs current holdings with their cost basis.
func pnlRows(res costbasis.Result, assets []portfolio.AssetRow) []templates.PnLRow {
	var rows []templates.PnLRow
	for _, a := range assets {
		row := templates.PnLRow{AssetKey: a.AssetKey, Symbol: a.Symbol, Name: a.Name, Amount: a.Amount, Value: a.ValueUSD}
		ar, known := res.Assets[a.AssetKey]
		if known {
			row.Overridden, row.UnknownUnits, row.AvgCost = ar.Overridden, ar.UnknownBasisUnits, ar.AvgCost
		}
		if a.Price != nil {
			if basis, unreal, pct, ok := res.Unrealized(a.AssetKey, a.Amount, a.Price.USD); ok {
				row.Basis, row.Unrealized, row.UnrealizedPct, row.HasBasis = basis, unreal, pct, true
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// pnlCells is the overview's optional P/L column.
func (s *Server) pnlCells(ctx context.Context, assets []portfolio.AssetRow) map[string]templates.PnLCell {
	res, ok := s.costResult(ctx)
	if !ok {
		return nil
	}
	cells := map[string]templates.PnLCell{}
	for _, row := range pnlRows(res, assets) {
		cells[row.AssetKey] = templates.PnLCell{Unrealized: row.Unrealized, Pct: row.UnrealizedPct, OK: row.HasBasis}
	}
	return cells
}

// walletCost fills the wallet page's Cost basis and Unrealized tiles.
func (s *Server) walletCost(ctx context.Context, p *templates.WalletPage) {
	res, ok := s.costResult(ctx)
	if !ok {
		return
	}
	var basis, unreal float64
	found := false
	for _, r := range p.W.Rows {
		if r.Price == nil {
			continue
		}
		if b, u, _, ok := res.Unrealized(r.AssetKey, r.Amount, r.Price.USD); ok {
			basis, unreal, found = basis+b, unreal+u, true
		}
	}
	if found {
		p.CostBasis, p.Unrealized = &basis, &unreal
	}
}

func (s *Server) reportsPage(r *http.Request) templates.ReportsPage {
	snap, _ := s.latest()
	p := templates.ReportsPage{Shell: s.shell(r, "Reports", "reports", "all", ""), TotalValue: snap.TotalUSD, Now: s.Now()}
	p.AssetOptions = s.assetOptions()
	if s.CostBasis != nil {
		for _, m := range s.CostBasis.Get().Assets {
			p.Manual = append(p.Manual, templates.ManualBasisRow{Asset: m.Asset, Amount: m.Amount, CostUSD: m.CostUSD, Note: m.Note})
		}
		if err := s.CostBasis.Err(); err != nil {
			p.BasisErr = "data/costbasis.yaml has an error and was not reloaded: " + err.Error()
		}
	}
	res, ok := s.costResult(r.Context())
	if !ok {
		return p
	}
	p.Rows = pnlRows(res, snap.Assets)
	// Basis and unrealized cover only units with a known cost; the Value tile is everything.
	p.TotalBasis, _, p.TotalUnrealized = costbasis.ForPortfolio(res, snap.Assets)
	p.TotalValue = snap.TotalUSD
	p.Coverage = costbasis.Coverage(res, snap.Assets)
	years := map[int]float64{}
	for _, a := range res.Assets {
		for y, v := range a.RealizedByYear {
			years[y] += v
		}
	}
	for y, v := range years {
		p.Realized = append(p.Realized, templates.YearAmount{Year: y, USD: v})
	}
	sort.Slice(p.Realized, func(i, j int) bool { return p.Realized[i].Year > p.Realized[j].Year })
	return p
}

func (s *Server) reports(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, templates.ReportsFull(s.reportsPage(r)))
}

func (s *Server) patchReports(sse *datastar.ServerSentEventGenerator, r *http.Request, mutate func(*templates.ReportsPage)) {
	p := s.reportsPage(r)
	if mutate != nil {
		mutate(&p)
	}
	_ = sse.PatchElementTempl(templates.PnLTable(p))
	_ = sse.PatchElementTempl(templates.CostBasisPanel(p))
	_ = sse.PatchElementTempl(templates.CSVResultView(p.Import))
}

func (s *Server) basisSave(w http.ResponseWriter, r *http.Request) {
	var msg string
	if err := parseBody(r); err != nil {
		msg = "Could not read the form."
	} else {
		asset := manual.NormalizeAsset(r.PostFormValue("asset"))
		amount, err1 := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(r.PostFormValue("amount")), ",", ""), 64)
		cost, err2 := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("cost")), "$"), ",", ""), 64)
		switch {
		case asset == "":
			msg = "Choose an asset."
		case err1 != nil || amount <= 0:
			msg = "Enter the amount you hold, e.g. 0.5."
		case err2 != nil || cost < 0:
			msg = "Enter the total you paid in USD, e.g. 15000."
		default:
			if err := s.CostBasis.SetAsset(asset, amount, cost, strings.TrimSpace(r.PostFormValue("note"))); err != nil {
				msg = err.Error()
			}
		}
	}
	s.costs.invalidate()
	sse := datastar.NewSSE(w, r)
	s.patchReports(sse, r, func(p *templates.ReportsPage) {
		if msg != "" {
			p.BasisErr = msg
		}
	})
}

func (s *Server) basisDelete(w http.ResponseWriter, r *http.Request) {
	asset, _ := url.PathUnescape(chi.URLParam(r, "asset"))
	err := s.CostBasis.DeleteAsset(asset)
	s.costs.invalidate()
	sse := datastar.NewSSE(w, r)
	s.patchReports(sse, r, func(p *templates.ReportsPage) {
		if err != nil {
			p.BasisErr = err.Error()
		}
	})
}

// csvImport parses a Coinbase or Kraken CSV export into activity under a pseudo-source.
func (s *Server) csvImport(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		outcome.Message = "Choose a CSV file to import."
	} else if file, _, err := r.FormFile("file"); err != nil {
		outcome.Message = "Choose a CSV file to import."
	} else {
		defer file.Close()
		var txs []core.Tx
		var perr error
		skipped := 0
		opts := costbasis.ParseOptions{OnSkip: func(int, string) { skipped++ }}
		src := r.PostFormValue("source")
		switch src {
		case "kraken":
			opts.SourceID = "kraken-csv"
			txs, perr = costbasis.ParseKrakenLedgersCSV(io.LimitReader(file, 8<<20), opts)
		default:
			opts.SourceID = "coinbase-csv"
			txs, perr = costbasis.ParseCoinbaseCSV(io.LimitReader(file, 8<<20), opts)
		}
		switch {
		case perr != nil:
			outcome.Message = "Nothing was imported: " + perr.Error()
		case s.Activity == nil:
			outcome.Message = "Activity storage is not available."
		default:
			n, err := s.Activity.Upsert(r.Context(), txs)
			if err != nil {
				outcome.Message = "Import failed: " + err.Error()
			} else {
				outcome.OK = true
				outcome.Message = fmt.Sprintf("Imported %d transactions (%d new)", len(txs), n)
				if skipped > 0 {
					outcome.Message += fmt.Sprintf(", skipped %d fiat or unsupported rows", skipped)
				}
				outcome.Message += "."
			}
		}
	}
	s.costs.invalidate()
	sse := datastar.NewSSE(w, r)
	s.patchReports(sse, r, func(p *templates.ReportsPage) { p.Import = outcome })
}

// --- CSV exports ---

func (s *Server) csvHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, name, s.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) exportHoldings(w http.ResponseWriter, r *http.Request) {
	all, _ := s.latest()
	pv, _ := s.resolvePortfolio(r.URL.Query().Get("pf"), all)
	s.csvHeaders(w, "holdings")
	if err := export.Holdings(w, pv.snap); err != nil {
		s.Log.Warn("export holdings", "err", err)
	}
}

func (s *Server) exportHistory(w http.ResponseWriter, r *http.Request) {
	all, ready := s.latest()
	q := r.URL.Query()
	pv, _ := s.resolvePortfolio(q.Get("pf"), all)
	rng := q.Get("range")
	if _, ok := portfolio.Ranges[rng]; !ok {
		rng = "All"
	}
	pts, err := s.History.Series(r.Context(), portfolio.RangeStart(rng, s.Now()), pv.include, ptrIf(ready, &pv.snap))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.csvHeaders(w, "history")
	if err := export.History(w, pts); err != nil {
		s.Log.Warn("export history", "err", err)
	}
}

func (s *Server) exportPnL(w http.ResponseWriter, r *http.Request) {
	snap, _ := s.latest()
	res, _ := s.costResult(r.Context())
	var rows []export.PnLRow
	for _, p := range pnlRows(res, snap.Assets) {
		rows = append(rows, export.PnLRow{
			AssetKey: p.AssetKey, Symbol: p.Symbol, Amount: p.Amount, AvgCost: p.AvgCost, Basis: p.Basis,
			Value: p.Value, Unrealized: p.Unrealized, UnrealizedPct: p.UnrealizedPct, HasBasis: p.HasBasis,
			Realized: res.Assets[p.AssetKey].RealizedByYear,
		})
	}
	s.csvHeaders(w, "pnl")
	if err := export.PnL(w, rows); err != nil {
		s.Log.Warn("export pnl", "err", err)
	}
}

func (s *Server) exportActivity(w http.ResponseWriter, r *http.Request) {
	if s.Activity == nil {
		http.Error(w, "activity not available", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	f := activityFilter{Source: q.Get("source"), Asset: q.Get("asset"), Kind: q.Get("kind")}
	var ids []string
	if f.Source != "" {
		ids = []string{f.Source}
	}
	txs, err := s.Activity.All(r.Context(), ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var keep []core.Tx
	for _, tx := range txs {
		if (f.Kind == "" || string(tx.Kind) == f.Kind) && (f.Asset == "" || txHasAsset(tx, f.Asset)) {
			keep = append(keep, tx)
		}
	}
	labels, _ := s.sourceLabels()
	s.csvHeaders(w, "activity")
	if err := export.Activity(w, keep, labels); err != nil {
		s.Log.Warn("export activity", "err", err)
	}
}

func txHasAsset(tx core.Tx, key string) bool {
	for _, l := range tx.Legs {
		if l.AssetKey == key {
			return true
		}
	}
	return tx.Fee != nil && tx.Fee.AssetKey == key
}
