package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/web/templates"
)

// viewSignals are the client signals the overview endpoints read.
type viewSignals struct {
	PF    string `json:"pf"`
	Range string `json:"range"`
}

func (v *viewSignals) normalize() {
	if v.PF == "" {
		v.PF = "all"
	}
	if _, ok := portfolio.Ranges[v.Range]; !ok {
		v.Range = "1W"
	}
}

func readView(r *http.Request) viewSignals {
	var v viewSignals
	_ = datastar.ReadSignals(r, &v)
	v.normalize()
	return v
}

func (s *Server) shell(r *http.Request, title, active, pf, rng string) templates.Shell {
	if pf == "" {
		pf = "all"
	}
	if _, ok := portfolio.Ranges[rng]; !ok {
		rng = "1W"
	}
	sh := templates.Shell{
		Title: title, Active: active, PF: pf, Range: rng, ShowLogos: s.showLogos(r.Context()),
		Portfolios: s.portfolioNav(),
	}
	if s.Cfg.Password != "" {
		if u, _, ok := r.BasicAuth(); ok {
			sh.AuthUser = u
		}
	}
	return sh
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// latest returns the current snapshot, or an empty one before the first refresh.
func (s *Server) latest() (portfolio.Snapshot, bool) {
	snap, ok := s.Refresher.Latest()
	if !ok {
		snap = portfolio.Snapshot{UpdatedAt: s.Now(), Prices: map[string]core.Price{}}
	}
	return snap, ok
}

func (s *Server) syncInfo(snap portfolio.Snapshot, ready bool) templates.SyncInfo {
	if !ready {
		return templates.SyncInfo{Status: portfolio.StatusPending, Label: "Syncing…"}
	}
	var failing, stale int
	var oldest time.Time
	for _, w := range snap.Wallets {
		switch w.Status {
		case portfolio.StatusError:
			failing++
		case portfolio.StatusStale:
			stale++
			if oldest.IsZero() || w.AsOf.Before(oldest) {
				oldest = w.AsOf
			}
		}
	}
	switch {
	case failing > 0:
		return templates.SyncInfo{Status: portfolio.StatusError, Label: plural(failing, "source") + " failing"}
	case stale > 0:
		return templates.SyncInfo{Status: portfolio.StatusStale, Label: "Stale " + templates.Ago(oldest, s.Now())}
	}
	return templates.SyncInfo{Status: portfolio.StatusSynced, Label: "Synced " + templates.Ago(snap.UpdatedAt, s.Now())}
}

// overviewPage builds the overview model for a portfolio and range. Unknown portfolio ids
// fall back to all holdings (the full-page handler 404s them first).
func (s *Server) overviewPage(r *http.Request, v viewSignals) templates.OverviewPage {
	all, ready := s.latest()
	now := s.Now()
	pv, _ := s.resolvePortfolio(v.PF, all)
	if !pv.known {
		v.PF = "all"
	}
	snap := pv.snap
	s.applyChange7d(r.Context(), &snap)
	p := templates.OverviewPage{
		Shell:         s.shell(r, pv.name, "overview", v.PF, v.Range),
		PortfolioName: pv.name,
		Snap:          snap,
		Sync:          s.syncInfo(all, ready),
		Watch:         s.watchRows(r.Context(), pv.watch, snap.Prices),
		NetWorthShare: pv.share,
		PnL:           s.pnlCells(r.Context(), snap.Assets, pv.lotWallets),
		Now:           now,
	}
	if s.History != nil {
		pts, err := s.History.Series(r.Context(), portfolio.RangeStart(v.Range, now), pv.include, ptrIf(ready, &snap))
		if err != nil {
			s.Log.Warn("history series", "err", err)
		}
		p.Chart = pts
		p.Change = portfolio.ChangeFor(v.Range, pts, now)
	}
	return p
}

func ptrIf[T any](ok bool, v *T) *T {
	if ok {
		return v
	}
	return nil
}

// isNav reports a Datastar navigation: an in-app link or back/forward fetched the page
// with @get (which sets Datastar-Request), rather than the browser loading it.
func isNav(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Datastar-Request") == "true"
}

// render writes a full page, or — for a Datastar navigation — only the page regions
// (title, sidebar, #main, bottom bar) as one SSE patch that morphs them by id.
func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	s.renderStatus(w, r, http.StatusOK, c)
}

// renderStatus is render with a status code for full loads (404 pages). A navigation
// always answers 200: Datastar treats other statuses as failed requests and retries them.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, code int, c templ.Component) {
	if isNav(r) {
		r = r.WithContext(templates.WithNav(r.Context()))
		sse := datastar.NewSSE(w, r)
		if err := sse.PatchElementTempl(c); err != nil {
			s.Log.Error("render nav", "path", r.URL.Path, "err", err)
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := c.Render(r.Context(), w); err != nil {
		s.Log.Error("render", "path", r.URL.Path, "err", err)
	}
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	v := viewSignals{PF: chi.URLParam(r, "pf"), Range: r.URL.Query().Get("range")}
	if isNav(r) && v.Range == "" {
		v.Range = readView(r).Range // keep the range picked before navigating away
	}
	v.normalize()
	if _, ok := s.resolvePortfolio(v.PF, portfolio.Snapshot{}); !ok {
		s.renderStatus(w, r, http.StatusNotFound, templates.ErrorFull(s.shell(r, "Not found", "overview", "all", ""), "That portfolio doesn't exist (it may have been deleted)."))
		return
	}
	s.render(w, r, templates.OverviewFull(s.overviewPage(r, v)))
}

// view patches the dashboard after a portfolio switch.
func (s *Server) view(w http.ResponseWriter, r *http.Request) {
	v := readView(r)
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.Dashboard(s.overviewPage(r, v)))
}

// chart patches the value card after a range change.
func (s *Server) chart(w http.ResponseWriter, r *http.Request) {
	v := readView(r)
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.ValueCard(s.overviewPage(r, v)))
}

// stream is the persistent SSE connection: it patches the dashboard with every new
// snapshot and sends a keep-alive comment so proxies (Tailscale serve) don't idle it out.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	v := readView(r)
	ch, cancel := s.Broker.Subscribe()
	defer cancel()
	sse := datastar.NewSSE(w, r)
	if _, ok := s.Refresher.Latest(); ok {
		if err := sse.PatchElementTempl(templates.Dashboard(s.overviewPage(r, v))); err != nil {
			return
		}
	}
	rc := http.NewResponseController(w)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if err := sse.PatchElementTempl(templates.Dashboard(s.overviewPage(r, v))); err != nil {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	v := readView(r)
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.SyncChip(templates.SyncInfo{Status: portfolio.StatusPending, Label: "Syncing…"}))
	if _, err := s.Refresher.Force(r.Context()); errors.Is(err, portfolio.ErrTooSoon) {
		snap, ready := s.latest()
		info := s.syncInfo(snap, ready)
		_ = sse.PatchElementTempl(templates.SyncChip(info))
		return
	}
	_ = sse.PatchElementTempl(templates.Dashboard(s.overviewPage(r, v)))
}

func (s *Server) portfolioJSON(w http.ResponseWriter, r *http.Request) {
	snap, _ := s.latest()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(snap)
}

func (s *Server) wallets(w http.ResponseWriter, r *http.Request) {
	snap, ready := s.latest()
	s.render(w, r, templates.WalletsFull(templates.WalletsPage{
		Shell: s.shell(r, "Wallets", "wallets", "all", ""), Wallets: snap.Wallets, Sync: s.syncInfo(snap, ready), Now: s.Now(),
	}))
}

func (s *Server) findWallet(id string) (portfolio.WalletView, sources.Entry, bool) {
	var entry sources.Entry
	found := false
	for _, e := range s.Registry.Entries() {
		if e.Def.ID == id {
			entry, found = e, true
			break
		}
	}
	if !found {
		return portfolio.WalletView{}, entry, false
	}
	snap, _ := s.latest()
	for _, wv := range snap.Wallets {
		if wv.ID == id {
			return wv, entry, true
		}
	}
	return pendingWallet(entry.Def), entry, true // added but not fetched yet
}

var walletTabs = map[string]bool{"holdings": true, "activity": true, "addresses": true, "utxos": true}

func (s *Server) walletPage(r *http.Request, id, tab string) (templates.WalletPage, bool) {
	wv, entry, ok := s.findWallet(id)
	if !ok {
		return templates.WalletPage{}, false
	}
	if !walletTabs[tab] {
		tab = "holdings"
	}
	isBTC := entry.Def.Type == sources.TypeBitcoin
	if !isBTC && (tab == "addresses" || tab == "utxos") {
		tab = "holdings"
	}
	p := templates.WalletPage{
		Shell: s.shell(r, wv.Label, "wallets", "all", ""),
		W:     wv, Tab: tab, IsBitcoin: isBTC, Now: s.Now(),
		PublicEndpoint: s.publicEndpoint(wv.Type), RetryEvery: time.Duration(s.Cfg.RefreshMinutes) * time.Minute,
	}
	if tab == "activity" {
		s.walletActivity(r.Context(), &p)
	}
	s.walletCost(r.Context(), &p)
	if entry.Source == nil {
		return p, true
	}
	ctx, cancel := ctxTimeout(r, 30*time.Second)
	defer cancel()
	if ra, ok := entry.Source.(core.ReceiveAddresser); ok {
		if addr, err := ra.NextReceiveAddress(ctx); err == nil && addr != "" {
			p.Receive = addr
			if svg, err := qrSVG("bitcoin:" + addr); err == nil {
				p.ReceiveQR = templ.Raw(svg)
			}
		}
	}
	switch tab {
	case "addresses":
		if al, ok := entry.Source.(core.AddressLister); ok {
			p.Addresses, _ = al.Addresses(ctx)
		}
	case "utxos":
		if ul, ok := entry.Source.(core.UTXOLister); ok {
			p.UTXOs, _ = ul.UTXOs(ctx)
		}
	}
	return p, true
}

func (s *Server) wallet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.walletPage(r, chi.URLParam(r, "id"), r.URL.Query().Get("tab"))
	if !ok {
		s.renderStatus(w, r, http.StatusNotFound, templates.ErrorFull(s.shell(r, "Not found", "wallets", "all", ""), "That wallet doesn't exist (it may have been removed)."))
		return
	}
	s.render(w, r, templates.WalletFull(p))
}

func (s *Server) walletTab(w http.ResponseWriter, r *http.Request) {
	p, ok := s.walletPage(r, chi.URLParam(r, "id"), chi.URLParam(r, "tab"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.WalletTab(p))
}

// pendingWallet is the view of a source that hasn't been fetched yet.
func pendingWallet(d sources.Def) portfolio.WalletView {
	return portfolio.WalletView{ID: d.ID, Label: d.Label, Type: d.Type, Kind: d.Kind, Status: portfolio.StatusPending}
}

// qrSVG renders text as an SVG QR code (dark modules on a light quiet zone, as in the design).
func qrSVG(text string) (string, error) {
	code, err := qrEncode(text)
	if err != nil {
		return "", err
	}
	n := code.Size
	const quiet = 2
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="QR code for the receive address" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#ECEEF1"/><path fill="#0D0F12" d="`, n+2*quiet, n+2*quiet)
	for y := range n {
		for x := range n {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
