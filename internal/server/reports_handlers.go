package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/export"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/web/templates"
)

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

// pnlRows pairs current holdings with the cost of their open tax lots in the wallets that
// pass the filter (nil = every wallet but the untracked one). Basis, AvgCost and
// Unrealized cover only units held in lots with a known cost (tax.Result.Unrealized), and
// need a price; UnknownUnits is the rest of the holding.
func pnlRows(res tax.Result, assets []portfolio.AssetRow, wallets func(string) bool) []templates.PnLRow {
	var rows []templates.PnLRow
	for _, a := range taxedAssets(assets) {
		row := templates.PnLRow{AssetKey: a.AssetKey, Symbol: a.Symbol, Name: a.Name, Amount: a.Amount, Value: a.ValueUSD}
		held, _, missing := res.Holding(a.AssetKey, wallets)
		covered := max(0, min(a.Amount, held-missing))
		row.MissingUnits = missing
		if unknown := a.Amount - covered; unknown > unknownDust(a.Amount) {
			row.UnknownUnits = unknown
		}
		if a.Price != nil {
			if basis, unreal, pct, ok := res.Unrealized(a.AssetKey, wallets, a.Amount, a.Price.USD); ok {
				row.Basis, row.Unrealized, row.UnrealizedPct, row.HasBasis = basis, unreal, pct, true
				row.AvgCost = basis / covered
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// taxedAssets leaves out stocks and funds: brokerage accounts have no tax lots here (the
// broker's 1099-B covers them), so they must not count as holdings of unknown cost.
func taxedAssets(rows []portfolio.AssetRow) []portfolio.AssetRow {
	out := make([]portfolio.AssetRow, 0, len(rows))
	for _, a := range rows {
		if _, stock := core.IsStockKey(a.AssetKey); !stock {
			out = append(out, a)
		}
	}
	return out
}

// unknownDust is the leftover below which a holding of amount counts as fully covered by
// lots: relative to the amount (1e-9 of it, at least 1e-9), since float residue grows with
// it and a huge-supply token must not show "0.0000 BONK with unknown cost".
func unknownDust(amount float64) float64 { return 1e-9 * max(1, amount) }

// pnlCells is the overview's optional P/L column, from the lots of the given wallets (nil =
// every wallet but the untracked one).
func (s *Server) pnlCells(ctx context.Context, assets []portfolio.AssetRow, wallets func(string) bool) map[string]templates.PnLCell {
	res, ok := s.taxResult(ctx)
	if !ok {
		return nil
	}
	cells := map[string]templates.PnLCell{}
	for _, row := range pnlRows(res, assets, wallets) {
		cells[row.AssetKey] = templates.PnLCell{Unrealized: row.Unrealized, Pct: row.UnrealizedPct, OK: row.HasBasis}
	}
	return cells
}

// walletCost fills the wallet page's Cost basis and Unrealized tiles from that wallet's own
// lots, and CostUnits, the units that basis covers. Rows of one asset (liquid and staked
// SOL, say) are costed together.
func (s *Server) walletCost(ctx context.Context, p *templates.WalletPage) {
	res, ok := s.taxResult(ctx)
	if !ok {
		return
	}
	amounts, prices := map[string]float64{}, map[string]float64{}
	var keys []string
	for _, r := range p.W.Rows {
		if r.Price == nil {
			continue
		}
		if _, seen := amounts[r.AssetKey]; !seen {
			keys = append(keys, r.AssetKey)
		}
		amounts[r.AssetKey] += r.Amount
		prices[r.AssetKey] = r.Price.USD
	}
	id := p.W.ID
	only := func(wallet string) bool { return wallet == id }
	var basis, unreal, units float64
	found := false
	for _, k := range keys {
		if b, u, _, ok := res.Unrealized(k, only, amounts[k], prices[k]); ok {
			basis, unreal, found = basis+b, unreal+u, true
			held, _, missing := res.Holding(k, only)
			units += max(0, min(amounts[k], held-missing)) // the units the basis covers
		}
	}
	if found {
		p.CostBasis, p.Unrealized, p.CostUnits = &basis, &unreal, units
	}
}

// realizedYears is the realized profit or loss per tax year, newest first: each closed
// year's frozen Summary (short- plus long-term gain), and the open years' disposals from
// res when ok.
func (s *Server) realizedYears(res tax.Result, ok bool) []templates.YearAmount {
	years := map[int]float64{}
	closed := map[int]bool{}
	if s.TaxYears != nil {
		for _, y := range s.TaxYears.Years() {
			if yr, found := s.TaxYears.Get(y); found {
				years[y], closed[y] = yr.Summary.ShortGain+yr.Summary.LongGain, true
			}
		}
	}
	if ok {
		for y, v := range res.RealizedByYear(s.taxLoc()) {
			if !closed[y] { // the engine starts after the last closed year: belt and braces
				years[y] += v
			}
		}
	}
	out := make([]templates.YearAmount, 0, len(years))
	for y, v := range years {
		out = append(out, templates.YearAmount{Year: y, USD: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year > out[j].Year })
	return out
}

// realizedByAsset is the realized profit or loss per asset and tax year: the closed years'
// disposals, then the open years' from res. A Koinly year lists Koinly's asset names, so
// it only lines up with an asset key that is the plain symbol (BTC, SOL).
func (s *Server) realizedByAsset(res tax.Result) map[string]map[int]float64 {
	out := map[string]map[int]float64{}
	add := func(asset string, year int, gain float64) {
		if out[asset] == nil {
			out[asset] = map[int]float64{}
		}
		out[asset][year] += gain
	}
	closed := map[int]bool{}
	if s.TaxYears != nil {
		for _, y := range s.TaxYears.Years() {
			yr, found := s.TaxYears.Get(y)
			if !found {
				continue
			}
			closed[y] = true
			for _, d := range yr.Disposals {
				add(d.Asset, y, d.GainUSD)
			}
		}
	}
	loc := s.taxLoc()
	for _, d := range res.Disposals {
		if y := d.Sold.In(loc).Year(); !closed[y] {
			add(d.Asset, y, d.GainUSD)
		}
	}
	return out
}

func (s *Server) reportsPage(r *http.Request) templates.ReportsPage {
	snap, _ := s.latest()
	p := templates.ReportsPage{Shell: s.shell(r, "Reports", "reports", "all", ""), Now: s.Now()}
	for _, a := range taxedAssets(snap.Assets) {
		p.TotalValue += a.ValueUSD
	}
	if s.TaxSettings != nil {
		if err := s.TaxSettings.Err(); err != nil {
			p.TaxErr = "data/tax.yaml has an error and was not reloaded: " + err.Error()
		}
	}
	p.Review = s.reviewPage(r.Context())
	res, ok := s.taxResult(r.Context())
	p.Realized = s.realizedYears(res, ok)
	if !ok {
		return p
	}
	p.Rows = pnlRows(res, snap.Assets, nil)
	// Basis and unrealized cover only units with a known cost; the Value tile is everything.
	for _, row := range p.Rows {
		if row.HasBasis {
			p.TotalBasis += row.Basis
			p.TotalUnrealized += row.Unrealized
		}
	}
	p.Coverage = tax.Coverage(res, taxedAssets(snap.Assets))
	p.MissingBasisSales = res.MissingBasisSales()
	return p
}

// reportsPageTax is the Reports page with the Tax section on the given year, tab and page.
func (s *Server) reportsPageTax(r *http.Request, year int, tab string, page int) templates.ReportsPage {
	r = r.WithContext(s.withActivity(r.Context()))
	p := s.reportsPage(r)
	p.Tax = s.taxPage(r.Context(), year, tab, page, &p.Review)
	return p
}

func (s *Server) reports(w http.ResponseWriter, r *http.Request) {
	year, tab, page := taxParams(r.URL.Query().Get)
	s.render(w, r, templates.ReportsFull(s.reportsPageTax(r, year, tab, page)))
}

func (s *Server) patchReports(sse *datastar.ServerSentEventGenerator, r *http.Request, mutate func(*templates.ReportsPage)) {
	r = r.WithContext(s.withActivity(r.Context()))
	p := s.reportsPage(r)
	if mutate != nil {
		mutate(&p)
	}
	s.patchTaxSection(sse, r, &p)
	_ = sse.PatchElementTempl(templates.PnLTable(p))
	_ = sse.PatchElementTempl(templates.CSVResultView(p.Import))
}

// csvImportLimit is the biggest CSV the importer reads; multipartSlack covers the multipart
// framing around the file in the request body.
const (
	csvImportLimit = 8 << 20
	multipartSlack = 1 << 20
	csvTooBig      = "That file is too large to import (the limit is 8 MB)."
)

// uploadErrMsg is the message for a failed upload parse: tooBig when the body hit its size
// limit, else fallback.
func uploadErrMsg(err error, fallback, tooBig string) string {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return tooBig
	}
	return fallback
}

// csvImport parses a Coinbase or Kraken CSV export into activity under a pseudo-source.
func (s *Server) csvImport(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{}
	r.Body = http.MaxBytesReader(w, r.Body, csvImportLimit+multipartSlack)
	var raw []byte
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		outcome.Message = uploadErrMsg(err, "Choose a CSV file to import.", csvTooBig)
	} else if file, _, err := r.FormFile("file"); err != nil {
		outcome.Message = "Choose a CSV file to import."
	} else {
		raw, _ = io.ReadAll(io.LimitReader(file, csvImportLimit+1))
		file.Close()
		if len(raw) > csvImportLimit {
			outcome.Message = csvTooBig
		}
	}
	if outcome.Message == "" {
		var txs []core.Tx
		var perr error
		skipped := 0
		opts := costbasis.ParseOptions{OnSkip: func(int, string) { skipped++ }}
		src := r.PostFormValue("source")
		switch src {
		case "kraken":
			opts.SourceID = "kraken-csv"
			txs, perr = costbasis.ParseKrakenLedgersCSV(bytes.NewReader(raw), opts)
		default:
			opts.SourceID = "coinbase-csv"
			txs, perr = costbasis.ParseCoinbaseCSV(bytes.NewReader(raw), opts)
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
	s.invalidateTax()
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
	res, _ := s.taxResult(r.Context())
	realized := s.realizedByAsset(res)
	var rows []export.PnLRow
	for _, p := range pnlRows(res, snap.Assets, nil) {
		rows = append(rows, export.PnLRow{
			AssetKey: p.AssetKey, Symbol: p.Symbol, Amount: p.Amount, AvgCost: p.AvgCost, Basis: p.Basis,
			Value: p.Value, Unrealized: p.Unrealized, UnrealizedPct: p.UnrealizedPct, HasBasis: p.HasBasis,
			MissingUnits: p.MissingUnits, Realized: realized[p.AssetKey],
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
	f := filterFromQuery(r)
	var ids []string
	if f.Source != "" {
		ids = []string{f.Source}
	}
	txs, err := s.Activity.All(r.Context(), ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	since, until := f.period().Bounds(s.Now(), templates.Location)
	var keep []core.Tx
	for _, tx := range txs {
		inPeriod := (since.IsZero() || !tx.Time.Before(since)) && (until.IsZero() || tx.Time.Before(until))
		if inPeriod && (f.Kind == "" || string(tx.Kind) == f.Kind) && (f.Asset == "" || txHasAsset(tx, f.Asset)) {
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
