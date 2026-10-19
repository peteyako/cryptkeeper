package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"cryptkeeper/internal/tax"
	"cryptkeeper/internal/tax/report"
	"cryptkeeper/web/templates"
)

// The Tax section of Reports: one tax year's report (closed from its frozen rows, open from
// the engine) with year selector, tabs and paging, patched as #tax-section.

// taxYears are the years the selector offers, ascending: every closed year, then every open
// year up to the current one (from the year after the last closed one, or with none closed,
// from the earliest year with activity).
func (s *Server) taxYears() []int { return s.taxYearsCtx(context.Background()) }

func (s *Server) taxYearsCtx(ctx context.Context) []int {
	cur := s.Now().In(s.taxLoc()).Year()
	set := map[int]bool{}
	start := 0
	if s.TaxYears != nil {
		for _, y := range s.TaxYears.Years() {
			set[y] = true
		}
		if last, ok := s.TaxYears.LastClosed(); ok {
			start = last.Year + 1
		}
	}
	if start == 0 {
		if y, ok := s.closableYear(ctx); ok {
			start = y
		}
	}
	for y := start; start > 0 && y <= cur; y++ {
		set[y] = true
	}
	if len(set) == 0 {
		set[cur] = true
	}
	years := make([]int, 0, len(set))
	for y := range set {
		years = append(years, y)
	}
	slices.Sort(years)
	return years
}

// taxSymbols resolves asset keys to ticker, name and Solana mint: the latest snapshot, then
// the legs of stored activity, then the token_meta cache for Solana mints. Each key is
// resolved once (one token_meta read per mint, not per row).
type taxSymbols struct {
	s     *Server
	ctx   context.Context
	once  sync.Once
	info  map[string]taxSymbol
	price map[string]float64

	mu   sync.Mutex
	memo map[string]taxSymbol
}

type taxSymbol struct{ symbol, name, mint string }

func (t *taxSymbols) load() {
	t.once.Do(func() {
		t.info, t.price = map[string]taxSymbol{}, map[string]float64{}
		snap, _ := t.s.latest()
		for _, a := range snap.Assets {
			t.info[a.AssetKey] = taxSymbol{a.Symbol, a.Name, a.Mint}
			if a.Price != nil {
				t.price[a.AssetKey] = a.Price.USD
			}
		}
		if t.s.Activity == nil {
			return
		}
		all, err := t.s.allActivity(t.ctx)
		if err != nil {
			return
		}
		for _, tx := range all {
			for _, l := range tx.Legs {
				if cur := t.info[l.AssetKey]; cur.symbol == "" && l.Symbol != "" {
					cur.symbol = l.Symbol
					t.info[l.AssetKey] = cur
				}
			}
		}
	})
}

func (t *taxSymbols) get(key string) taxSymbol {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	if i, ok := t.memo[key]; ok {
		return i
	}
	if t.memo == nil {
		t.memo = map[string]taxSymbol{}
	}
	i := t.resolve(key)
	t.memo[key] = i
	return i
}

// resolve looks key up: the loaded info, then token_meta for a Solana mint.
func (t *taxSymbols) resolve(key string) taxSymbol {
	i := t.info[key]
	mint, isSPL := strings.CutPrefix(key, "spl:")
	if isSPL && i.mint == "" {
		i.mint = mint
	}
	if isSPL && (i.symbol == "" || i.name == "") && t.s.DB != nil {
		var raw string
		if t.s.DB.QueryRow(`SELECT json FROM token_meta WHERE mint = ?`, mint).Scan(&raw) == nil {
			var m struct{ Symbol, Name string }
			if json.Unmarshal([]byte(raw), &m) == nil {
				if i.symbol == "" {
					i.symbol = m.Symbol
				}
				if i.name == "" {
					i.name = m.Name
				}
			}
		}
	}
	return i
}

// resolver builds the report's lookups. Prices: a Solana token is priced by its mint
// (a ticker alone could be a look-alike), anything else by ticker then by a known mint;
// when neither knows and day is today in the current open year, the live snapshot price
// stands in. A Quiet (auto-hidden) token is never looked up upstream: it is priced from the
// snapshot or not at all, so a page does not send one request per airdropped token held.
func (s *Server) resolver(ctx context.Context, year int, now time.Time) report.Resolver {
	snap, _ := s.latest()
	return s.resolverWith(ctx, year, now, quietSet(snap, spamSet(s.Cfg.Solana.HideMints)))
}

// resolverWith is resolver for a given set of auto-hidden (quiet) asset keys.
func (s *Server) resolverWith(ctx context.Context, year int, now time.Time, quiet map[string]bool) report.Resolver {
	syms := &taxSymbols{s: s, ctx: ctx}
	labels, _ := s.sourceLabels()
	types := s.liveSourceTypes()
	targets := csvTargets(types) // CSV lots acquired "in" their exchange's wallet (see taxActivity)
	for csv, typ := range importedSourceType {
		types[csv] = typ
	}
	var box map[string]string
	if s.TaxSettings != nil {
		box = s.TaxSettings.Get().WalletBox
	}
	loc := s.taxLoc()
	return report.Resolver{
		Symbol: func(key string) (string, string) {
			i := syms.get(key)
			return i.symbol, i.name
		},
		WalletLabel: func(w string) string { return labels[w] },
		WalletType:  func(w string) string { return types[w] },
		Box:         box,
		LotWallet: func(w string) string {
			if t, ok := targets[w]; ok {
				return t
			}
			return w
		},
		Price: func(key string, day time.Time) (float64, bool) {
			i := syms.get(key)
			_, isSPL := strings.CutPrefix(key, "spl:")
			if s.Historical != nil && !quiet[key] {
				if !isSPL {
					sym := i.symbol
					if sym == "" {
						sym = key
					}
					if v, ok, err := s.Historical.Historical(ctx, sym, day); err == nil && ok {
						return v, true
					}
				}
				if i.mint != "" {
					// The year-end close, as Historical's Coinbase close for symbols.
					if v, ok, err := s.mintClose(ctx, i.mint, day); err == nil && ok {
						return v, true
					}
				}
			}
			if year == now.In(loc).Year() && day.In(loc).YearDay() == now.In(loc).YearDay() {
				if v, ok := syms.price[key]; ok {
					return v, true
				}
			}
			return 0, false
		},
	}
}

var errNoTaxData = errors.New(noTaxData)

// openYearRange is the span an open year's report covers: Jan 1 to Jan 1 in the tax
// timezone, except that the first open year starts where the last closed year ended, at
// midnight in the zone it was closed in (tax.OpenYearStart), so a change of tax timezone
// since neither counts a tx twice nor leaves one out.
func (s *Server) openYearRange(year int) (from, to time.Time) {
	loc := s.taxLoc()
	from, to = time.Date(year, 1, 1, 0, 0, 0, 0, loc), time.Date(year+1, 1, 1, 0, 0, 0, 0, loc)
	if s.TaxYears == nil {
		return from, to
	}
	if last, ok := s.TaxYears.LastClosed(); ok && last.Year+1 == year {
		from = tax.OpenYearStart(last, ok, loc)
	}
	return from, to
}

// zoneNote warns, for the first open year after a year closed in another timezone, where
// that year starts; "" otherwise.
func (s *Server) zoneNote(year int) string {
	if s.TaxYears == nil {
		return ""
	}
	last, ok := s.TaxYears.LastClosed()
	loc := s.taxLoc()
	if !ok || last.Year+1 != year || !last.ZoneChanged(loc) {
		return ""
	}
	return fmt.Sprintf("%d was closed in the %s timezone and your tax timezone is now %s: %d starts at Jan 1 00:00 %s, so no transaction is counted twice or left out. Dates and holding periods use %s.",
		last.Year, last.Timezone, loc, year, last.Timezone, loc)
}

// yearReport is the report of one tax year: a closed year from its frozen rows, an open one
// from the engine (the current year from the cached result; an earlier open year computed
// up to its end).
func (s *Server) yearReport(ctx context.Context, year int) (report.YearReport, error) {
	ctx = s.withActivity(ctx)
	return s.yearReportWith(ctx, year, s.resolver(ctx, year, s.Now()))
}

// yearReportWith is yearReport with the resolver given, so a page can reuse it.
func (s *Server) yearReportWith(ctx context.Context, year int, res report.Resolver) (report.YearReport, error) {
	loc, now := s.taxLoc(), s.Now()
	if s.TaxYears != nil {
		if y, ok := s.TaxYears.Get(year); ok {
			return report.FromClosed(y, res, loc), nil
		}
	}
	if s.Activity == nil {
		return report.YearReport{}, errNoTaxData
	}
	from, to := s.openYearRange(year)
	var result tax.Result
	if year >= now.In(loc).Year() {
		r, ok := s.taxResult(ctx)
		if !ok {
			return report.YearReport{}, errNoTaxData
		}
		result = r
	} else {
		c := &s.taxes
		c.ymu.Lock()
		hit, ok := c.years[year]
		c.ymu.Unlock()
		if ok && now.Sub(hit.at) < time.Minute {
			return hit.rep, nil
		}
		in, ready, err := s.taxInput(ctx)
		if err != nil {
			return report.YearReport{}, err
		}
		in.To = to
		rep := report.FromOpenRange(year, tax.Compute(in), res, loc, now, from, to)
		if ready {
			c.ymu.Lock()
			if c.years == nil {
				c.years = map[int]cachedYear{}
			}
			c.years[year] = cachedYear{at: now, rep: rep}
			c.ymu.Unlock()
		}
		return rep, nil
	}
	return report.FromOpenRange(year, result, res, loc, now, from, to), nil
}

// taxPage builds the Tax section for year (0 or unknown = the latest), tab and page. review,
// when set, is reused for the Needs review tab instead of being built again.
func (s *Server) taxPage(ctx context.Context, year int, tab string, page int, review *templates.ReviewPage) templates.TaxPage {
	ctx = s.withActivity(ctx)
	years := s.taxYearsCtx(ctx)
	if !slices.Contains(years, year) {
		year = years[len(years)-1]
	}
	p := templates.TaxPage{Years: years, Year: year, Tab: tab, Page: max(page, 1)}
	res := s.resolver(ctx, year, s.Now())
	rep, err := s.yearReportWith(ctx, year, res)
	if err != nil {
		p.Err = "Tax figures couldn't be loaded: " + err.Error()
		return p
	}
	p.Report = rep
	if !rep.Closed {
		p.ZoneNote = s.zoneNote(year)
		s.fillTaxScope(&p)
	}
	p.ReviewTab = !rep.Closed && year == years[len(years)-1]
	p.Pages = max(1, (len(rep.Rows)+templates.TaxPageSize-1)/templates.TaxPageSize)
	p.Page = min(p.Page, p.Pages)
	switch {
	case tab == "review" && p.ReviewTab:
		if review != nil {
			p.Review = *review
		} else {
			p.Review = s.reviewPage(ctx)
		}
	case tab == "income" || tab == "holdings":
	default:
		p.Tab = "gains"
	}
	if p.Tab == "income" {
		p.Symbols, p.Wallets = map[string]string{}, map[string]string{}
		for _, i := range rep.Income {
			if res.Symbol != nil {
				p.Symbols[i.Asset], _ = res.Symbol(i.Asset)
			}
			if l := res.WalletLabel(i.Wallet); l != "" {
				p.Wallets[i.Wallet] = l
			}
		}
	}
	return p
}

func taxParams(get func(string) string) (year int, tab string, page int) {
	year, _ = strconv.Atoi(get("year"))
	page, _ = strconv.Atoi(get("page"))
	return year, get("tab"), page
}

// taxView serves a year/tab/page of the Tax section: a Datastar request patches just
// #tax-section, a browser load renders the Reports page with it.
func (s *Server) taxView(w http.ResponseWriter, r *http.Request) {
	year, tab, page := taxParams(r.URL.Query().Get)
	ctx, cancel := ctxTimeout(r, 60*time.Second)
	defer cancel()
	if r.Header.Get("Datastar-Request") == "true" {
		_ = datastar.NewSSE(w, r).PatchElementTempl(templates.TaxSection(s.taxPage(ctx, year, tab, page, nil)))
		return
	}
	s.render(w, r, templates.ReportsFull(s.reportsPageTax(r, year, tab, page)))
}

// taxViewYear is the year selector's POST: it shows the chosen year's first capital-gains
// page (or the posted tab).
// fillTaxScope sets the tax section's scope picker: every portfolio that picks wallets
// (one holding them all would change nothing), and the saved choice.
func (s *Server) fillTaxScope(p *templates.TaxPage) {
	if s.TaxSettings == nil || s.Portfolios == nil {
		return
	}
	p.ScopeOptions = []templates.ScopeOption{{ID: "", Name: "All wallets"}}
	for _, pf := range s.Portfolios.List() {
		if !pf.Wallets.All {
			p.ScopeOptions = append(p.ScopeOptions, templates.ScopeOption{ID: pf.ID, Name: pf.Name})
		}
	}
	id := s.TaxSettings.Get().Scope
	_, name, scoped := s.taxScope()
	switch {
	case scoped:
		p.Scope, p.ScopeName = id, name
	case id != "":
		p.ScopeNote = "The portfolio these taxes were limited to is gone or now holds every wallet, so they cover every wallet."
	}
}

// taxViewScope saves which portfolio the tax covers (scope = portfolio ID, "" for every
// wallet) and reloads Reports, whose figures all change with it.
func (s *Server) taxViewScope(w http.ResponseWriter, r *http.Request) {
	bodyErr := parseBody(r) // before the response starts, which closes the request body
	sse := datastar.NewSSE(w, r)
	fail := func(msg string) {
		p := s.taxPage(r.Context(), 0, "", 1, nil)
		p.Err = msg
		_ = sse.PatchElementTempl(templates.TaxSection(p))
	}
	if bodyErr != nil {
		fail("That form could not be read.")
		return
	}
	id := strings.TrimSpace(r.PostFormValue("scope"))
	if id != "" && s.Portfolios != nil {
		if _, ok := s.Portfolios.Get(id); !ok {
			fail("That portfolio no longer exists.")
			return
		}
	}
	if s.TaxSettings == nil {
		fail("Tax settings aren't available.")
		return
	}
	if err := s.TaxSettings.Update(func(st *tax.Settings) { st.Scope = id }); err != nil {
		fail("Couldn't save the choice: " + err.Error())
		return
	}
	s.invalidateTax()
	_ = sse.Redirect("/reports")
}

func (s *Server) taxViewYear(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r, 60*time.Second)
	defer cancel()
	if err := parseBody(r); err != nil {
		p := s.taxPage(ctx, 0, "", 1, nil)
		p.Err = "That form could not be read."
		_ = datastar.NewSSE(w, r).PatchElementTempl(templates.TaxSection(p))
		return
	}
	year, tab, page := taxParams(r.PostFormValue)
	_ = datastar.NewSSE(w, r).PatchElementTempl(templates.TaxSection(s.taxPage(ctx, year, tab, page, nil)))
}
