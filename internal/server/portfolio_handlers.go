package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/web/templates"
)

// portfolioView resolves a portfolio id against the latest snapshot. ok is false for an
// unknown id. For "all" the snapshot is returned unfiltered with a nil include func.
type portfolioView struct {
	name    string
	snap    portfolio.Snapshot
	include portfolio.Include
	share   float64
	watch   []string
	known   bool
	// lotWallets picks the wallets whose tax lots cost this view's holdings: nil (every
	// wallet but the untracked one) unless the portfolio lists its wallets.
	lotWallets func(string) bool
}

func (s *Server) resolvePortfolio(pf string, all portfolio.Snapshot) (portfolioView, bool) {
	if pf == "" || pf == "all" || s.Portfolios == nil {
		return portfolioView{name: "All holdings", snap: all, known: true}, pf == "" || pf == "all"
	}
	p, ok := s.Portfolios.Get(pf)
	if !ok {
		return portfolioView{name: "All holdings", snap: all}, false
	}
	filtered := portfolios.Apply(p, all)
	var lotWallets func(string) bool
	if !p.Wallets.All {
		ids := slices.Clone(p.Wallets.IDs)
		lotWallets = func(id string) bool { return slices.Contains(ids, id) }
	}
	return portfolioView{
		name:       p.Name,
		snap:       filtered,
		include:    portfolios.Include(p),
		share:      portfolios.NetWorthShare(filtered, all),
		watch:      portfolios.WatchKeys(p),
		known:      true,
		lotWallets: lotWallets,
	}, true
}

// portfolioNav lists "All holdings" plus every portfolio, with source counts.
func (s *Server) portfolioNav() []templates.PortfolioNav {
	snap, _ := s.latest()
	nav := []templates.PortfolioNav{{ID: "all", Name: "All holdings", Count: plural(len(s.Registry.Defs()), "source")}}
	if s.Portfolios == nil {
		return nav
	}
	for _, p := range s.Portfolios.List() {
		f := portfolios.Apply(p, snap)
		count := plural(len(f.Wallets), "source")
		if len(f.Wallets) == 0 && len(p.Watch) > 0 {
			count = fmt.Sprintf("%d watched", len(p.Watch))
		}
		nav = append(nav, templates.PortfolioNav{ID: p.ID, Name: p.Name, Count: count})
	}
	return nav
}

// watchRows prices a portfolio's watch list (assets not necessarily held).
func (s *Server) watchRows(ctx context.Context, keys []string, held map[string]core.Price) []templates.WatchRow {
	if len(keys) == 0 {
		return nil
	}
	var reqs []portfolio.PriceReq
	for _, k := range keys {
		if _, ok := held[k]; ok {
			continue
		}
		req := portfolio.PriceReq{Key: k}
		if m, ok := core.IsSPLKey(k); ok {
			req.Mint = m
		}
		reqs = append(reqs, req)
	}
	prices := map[string]core.Price{}
	if len(reqs) > 0 && s.Prices != nil {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		prices = s.Prices(pctx, reqs)
		cancel()
	}
	rows := make([]templates.WatchRow, 0, len(keys))
	for _, k := range keys {
		row := templates.WatchRow{AssetKey: k, Symbol: displayKey(k)}
		if p, ok := held[k]; ok {
			row.Price = &p
		} else if p, ok := prices[k]; ok {
			row.Price = &p
		}
		rows = append(rows, row)
	}
	return rows
}

// applyChange7d fills AssetRow.Change7d from history.
func (s *Server) applyChange7d(ctx context.Context, snap *portfolio.Snapshot) {
	if s.History == nil || len(snap.Assets) == 0 {
		return
	}
	cur := make(map[string]float64, len(snap.Assets))
	for _, a := range snap.Assets {
		if a.Price != nil {
			cur[a.AssetKey] = a.Price.USD
		}
	}
	ch, err := s.History.PriceChanges(ctx, cur, 7*24*time.Hour, s.Now())
	if err != nil {
		s.Log.Warn("7d price changes", "err", err)
		return
	}
	for i := range snap.Assets {
		if v, ok := ch[snap.Assets[i].AssetKey]; ok {
			snap.Assets[i].Change7d = &v
		}
	}
}

// displayKey shortens "spl:<mint>" keys for display ("JUPy…DvCN"); symbols pass through.
func displayKey(k string) string {
	m, ok := core.IsSPLKey(k)
	if !ok || len(m) <= 10 {
		return k
	}
	return m[:4] + "…" + m[len(m)-4:]
}

// walletOptions lists configured sources for pickers; without handEntered, only those with
// activity (not manual holdings or brokerage accounts).
func (s *Server) walletOptions(handEntered bool) []templates.WalletOption {
	var out []templates.WalletOption
	for _, d := range s.Registry.Defs() {
		if handEntered || !sources.HandEntered(d.Type) {
			out = append(out, templates.WalletOption{ID: d.ID, Label: d.Label, Type: d.Type, Kind: d.Kind})
		}
	}
	return out
}

// assetOptions lists currently held assets (by value) for pickers.
func (s *Server) assetOptions() []templates.AssetOption {
	snap, _ := s.latest()
	out := make([]templates.AssetOption, 0, len(snap.Assets))
	for _, a := range snap.Assets {
		out = append(out, templates.AssetOption{Key: a.AssetKey, Symbol: a.Symbol, Name: a.Name, ValueUSD: a.ValueUSD})
	}
	return out
}

// --- manager ---

func (s *Server) portfoliosPage(r *http.Request) templates.PortfoliosPage {
	snap, _ := s.latest()
	p := templates.PortfoliosPage{Shell: s.shell(r, "Portfolios", "portfolios", "all", ""), Now: s.Now(), EncryptedExports: s.Cfg.EncryptExports()}
	known := make([]string, 0)
	for _, d := range s.Registry.Defs() {
		known = append(known, d.ID)
	}
	for _, pf := range s.Portfolios.List() {
		f := portfolios.Apply(pf, snap)
		stable, hasValue := f.StableShare()
		p.Items = append(p.Items, templates.PortfolioItem{
			ID: pf.ID, Name: pf.Name, ValueUSD: f.TotalUSD, Share: portfolios.NetWorthShare(f, snap),
			StableShare: stable, HasValue: hasValue,
			WalletsText: walletsText(pf.Wallets), AssetsText: assetsText(pf.Assets, snap),
			WatchCount: len(pf.Watch), Missing: portfolios.MissingWallets(pf, known),
		})
	}
	if bs, err := s.Portfolios.Backups(); err == nil {
		for _, b := range bs {
			p.Backups = append(p.Backups, templates.BackupRow{Name: b.Name, Time: b.Time, Size: b.Size})
		}
	}
	if err := s.Portfolios.LoadError(); err != nil {
		p.LoadError = err.Error()
	}
	return p
}

func walletsText(sel portfolios.Selector) string {
	switch {
	case sel.All:
		return "All wallets"
	case len(sel.IDs) == 0:
		return "No wallets"
	}
	return plural(len(sel.IDs), "wallet")
}

func assetsText(sel portfolios.Selector, snap portfolio.Snapshot) string {
	if sel.All {
		return "All assets"
	}
	if len(sel.IDs) == 0 {
		return "No assets"
	}
	symbol := map[string]string{}
	for _, a := range snap.Assets {
		symbol[a.AssetKey] = a.Symbol
	}
	var names []string
	for _, k := range sel.IDs[:min(3, len(sel.IDs))] {
		n := symbol[k]
		if n == "" {
			n = displayKey(k)
		}
		names = append(names, n)
	}
	out := strings.Join(names, ", ")
	if extra := len(sel.IDs) - 3; extra > 0 {
		out += fmt.Sprintf(" +%d", extra)
	}
	return out
}

func (s *Server) portfoliosManager(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, templates.PortfoliosFull(s.portfoliosPage(r)))
}

// patchManager re-renders the list and the save/restore panel (which shows the outcome).
func (s *Server) patchManager(sse *datastar.ServerSentEventGenerator, r *http.Request, outcome templates.ImportOutcome) {
	p := s.portfoliosPage(r)
	p.Import = outcome
	_ = sse.PatchElementTempl(templates.PortfolioList(p))
	_ = sse.PatchElementTempl(templates.PortfolioIO(p))
}

// --- editor ---

func (s *Server) portfolioForm(r *http.Request, editID string) (templates.PortfolioFormPage, bool) {
	snap, _ := s.latest()
	title := "New portfolio"
	p := templates.PortfolioFormPage{
		AllWallets: true, AllAssets: true,
		SelectedWallets: map[string]bool{}, SelectedAssets: map[string]bool{}, Errors: map[string]string{},
	}
	if editID != "" {
		pf, ok := s.Portfolios.Get(editID)
		if !ok {
			return p, false
		}
		title = "Edit " + pf.Name
		p.EditID, p.Name = pf.ID, pf.Name
		p.AllWallets, p.AllAssets = pf.Wallets.All, pf.Assets.All
		for _, id := range pf.Wallets.IDs {
			p.SelectedWallets[id] = true
		}
		held := map[string]bool{}
		for _, a := range snap.Assets {
			held[a.AssetKey] = true
		}
		var extra []string
		for _, k := range pf.Assets.IDs {
			if held[k] {
				p.SelectedAssets[k] = true
			} else {
				extra = append(extra, k)
			}
		}
		p.ExtraAssets = strings.Join(extra, " ")
		p.Watch = strings.Join(pf.Watch, " ")
	}
	p.Shell = s.shell(r, title, "portfolios", "all", "")
	p.WalletOptions = s.walletOptions(true)
	p.AssetOptions = s.assetOptions()
	return p, true
}

func (s *Server) portfolioNew(w http.ResponseWriter, r *http.Request) {
	p, _ := s.portfolioForm(r, "")
	s.render(w, r, templates.PortfolioFormFull(p))
}

func (s *Server) portfolioEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.portfolioForm(r, chi.URLParam(r, "id"))
	if !ok {
		http.Redirect(w, r, "/portfolios", http.StatusSeeOther)
		return
	}
	s.render(w, r, templates.PortfolioFormFull(p))
}

// splitKeys parses free-text asset lists: "ETH, avax spl:Mint…" → ["ETH" "AVAX" "spl:Mint…"].
func splitKeys(text string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
		out = append(out, manual.NormalizeAsset(f))
	}
	return out
}

func (s *Server) portfolioSave(w http.ResponseWriter, r *http.Request) {
	editID := chi.URLParam(r, "id")
	errs := map[string]string{}
	var p portfolios.Portfolio
	if err := parseBody(r); err != nil {
		errs[""] = "Could not read the form: " + err.Error()
	} else {
		p = portfolios.Portfolio{ID: editID, Name: strings.TrimSpace(r.PostFormValue("name"))}
		if p.Name == "" {
			errs["name"] = "Give the portfolio a name."
		}
		if r.PostFormValue("walletsMode") == "selected" {
			p.Wallets = portfolios.Selector{IDs: r.PostForm["wallets"]}
		} else {
			p.Wallets = portfolios.AllSelector()
		}
		if r.PostFormValue("assetsMode") == "selected" {
			ids := append([]string(nil), r.PostForm["assets"]...)
			ids = append(ids, splitKeys(r.PostFormValue("extraAssets"))...)
			p.Assets = portfolios.Selector{IDs: ids}
			if len(ids) == 0 {
				errs["assets"] = "Pick at least one asset, or choose All assets."
			}
		} else {
			p.Assets = portfolios.AllSelector()
		}
		p.Watch = splitKeys(r.PostFormValue("watch"))
		if p.Wallets.IsNone() && len(p.Watch) == 0 {
			errs["wallets"] = "Pick at least one wallet, or add coins to the watch list."
		}
	}
	sse := datastar.NewSSE(w, r)
	if len(errs) == 0 {
		saved, err := s.Portfolios.Upsert(p)
		if err != nil {
			errs[""] = err.Error()
		} else {
			_ = sse.Redirect("/p/" + saved.ID)
			return
		}
	}
	_ = sse.PatchElementTempl(templates.FormErrors(errs))
}

func (s *Server) portfolioDelete(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	if err := s.Portfolios.Delete(chi.URLParam(r, "id")); err != nil {
		s.patchManager(sse, r, templates.ImportOutcome{OK: false, Message: err.Error()})
		return
	}
	s.patchManager(sse, r, templates.ImportOutcome{})
}

// --- save & restore ---

func (s *Server) portfolioExport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	raw, err := s.Portfolios.Export(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// "Export all" also carries the alert rules, so one file backs up both.
	if id == "" && s.AlertRules != nil {
		if withAlerts, err := portfolios.WithAlerts(raw, s.AlertRules.List()); err == nil {
			raw = withAlerts
		} else {
			s.Log.Warn("export alert rules", "err", err)
		}
	}
	name := "portfolios"
	if id != "" {
		name = "portfolio-" + id
	}
	if raw, name, err = s.sealExport(raw, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.yaml"`, name, s.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}

func (s *Server) portfolioImport(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		outcome.Message = "Choose a YAML file to import."
	} else if file, _, err := r.FormFile("file"); err != nil {
		outcome.Message = "Choose a YAML file to import."
	} else {
		raw, _ := io.ReadAll(io.LimitReader(file, 1<<20))
		file.Close()
		mode := portfolios.ImportMerge
		if r.PostFormValue("mode") == string(portfolios.ImportReplace) {
			mode = portfolios.ImportReplace
		}
		raw, err := s.Box.OpenFile(raw)
		var res portfolios.ImportResult
		if err == nil {
			res, err = s.Portfolios.Import(raw, mode)
		}
		if err != nil {
			outcome.Message = "Nothing was imported: " + err.Error()
		} else {
			outcome.OK = true
			outcome.Message = fmt.Sprintf("Imported: %d added, %d replaced, %d unchanged", res.Added, res.Replaced, res.Unchanged)
			if res.Removed > 0 {
				outcome.Message += fmt.Sprintf(", %d removed", res.Removed)
			}
			outcome.Message += "."
			outcome.Message += s.importAlertSection(raw, mode == portfolios.ImportReplace)
		}
	}
	sse := datastar.NewSSE(w, r)
	s.patchManager(sse, r, outcome)
}

func (s *Server) portfolioRestore(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	if err := s.Portfolios.Restore(chi.URLParam(r, "name")); err != nil {
		s.patchManager(sse, r, templates.ImportOutcome{OK: false, Message: "Restore failed: " + err.Error()})
		return
	}
	s.patchManager(sse, r, templates.ImportOutcome{OK: true, Message: "Backup restored. The previous list was backed up first."})
}

// importAlertSection applies the optional alerts section of a portfolios file and returns
// a sentence for the import message ("" when the file has no alerts section).
func (s *Server) importAlertSection(raw []byte, replace bool) string {
	f, err := portfolios.Parse(raw)
	if err != nil || len(f.Alerts) == 0 || s.AlertRules == nil {
		return ""
	}
	res, err := s.AlertRules.Import(f.Alerts, replace)
	if err != nil {
		return " Alert rules were not imported: " + err.Error()
	}
	return fmt.Sprintf(" Alert rules: %d added, %d replaced, %d unchanged.", res.Added, res.Replaced, res.Unchanged)
}

// removeFromPortfolios drops a deleted source from explicit wallet lists.
func (s *Server) removeFromPortfolios(id string) {
	if s.Portfolios == nil {
		return
	}
	if n, err := s.Portfolios.RemoveWallet(id); err != nil {
		s.Log.Warn("remove wallet from portfolios", "id", id, "err", err)
	} else if n > 0 {
		s.Log.Info("removed wallet from portfolios", "id", id, "portfolios", n)
	}
}
