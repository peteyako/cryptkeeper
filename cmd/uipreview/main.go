// Command uipreview serves every Vault page from demo fixtures, so the UI can be viewed
// (and screenshotted) before any real data exists:
//
//	go run ./cmd/uipreview            # then open http://localhost:8790
//
// It wires the same Datastar endpoints the real server has (/view, /chart, /stream,
// /refresh, /w/{id}/tab/{tab}, /sources/test ...), answering from internal/demo.
//
// Useful query strings: ?range=1M on /, ?empty=1 on / and /wallets, ?tab=addresses on
// /w/cold, ?edit=1 and ?invalid=1 on /sources/new/{type}. On /portfolios: ?empty=1,
// ?missing=1 (a deleted wallet), ?error=1 (a bad hand edit), ?reset=1 (undo deletes); on
// /portfolios/new: ?invalid=1. The portfolio POSTs are stubs: a delete, import or restore
// sticks until the server restarts or ?reset=1.
//
// Activity (/activity?source=&asset=&kind=, ?empty=1) pages through the demo feed eight rows
// at a time. Reports (?empty=1) and Alerts (?empty=1, ?error=1 for a bad hand edit) keep
// what the stub POSTs change (a manual cost, an alert rule) until the server restarts or
// ?reset=1. /alerts/new and /alerts/{id}/edit take ?invalid=1.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/demo"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/web/static"
	"crypto-portfolio/web/templates"
)

// signals are the client signals the overview endpoints read.
type signals struct {
	PF    string `json:"pf"`
	Range string `json:"range"`
}

func readSignals(r *http.Request) signals {
	var s signals
	_ = datastar.ReadSignals(r, &s)
	if s.PF == "" {
		s.PF = "all"
	}
	if s.Range == "" {
		s.Range = "1W"
	}
	return s
}

func main() {
	addr := flag.String("addr", ":8790", "listen address")
	flag.Parse()

	srv := &http.Server{Addr: *addr, Handler: router(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Vault UI preview on http://localhost%s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := c.Render(r.Context(), w); err != nil {
		log.Printf("render %s: %v", r.URL.Path, err)
	}
}

func router() http.Handler {
	r := chi.NewRouter()

	r.Handle("/static/*", http.StripPrefix("/static/", noStore(http.FileServerFS(static.FS))))
	r.Get("/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(static.FS, "manifest.webmanifest")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/manifest+json")
		_, _ = w.Write(b)
	})

	// Overview.
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { overview(w, r, "all") })
	r.Get("/p/{id}", func(w http.ResponseWriter, r *http.Request) { overview(w, r, chi.URLParam(r, "id")) })
	r.Get("/view", func(w http.ResponseWriter, r *http.Request) {
		s := readSignals(r)
		p, ok := demo.OverviewFor(s.PF, s.Range, time.Now())
		if !ok {
			http.NotFound(w, r)
			return
		}
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.Dashboard(p))
	})
	r.Get("/chart", func(w http.ResponseWriter, r *http.Request) {
		s := readSignals(r)
		p, ok := demo.OverviewFor(s.PF, s.Range, time.Now())
		if !ok {
			http.NotFound(w, r)
			return
		}
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.ValueCard(p))
	})
	r.Get("/stream", stream)
	r.Post("/refresh", func(w http.ResponseWriter, r *http.Request) {
		s := readSignals(r)
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.SyncChip(templates.SyncInfo{Status: portfolio.StatusPending, Label: "Syncing…"}))
		time.Sleep(900 * time.Millisecond)
		if p, ok := demo.OverviewFor(s.PF, s.Range, time.Now()); ok {
			_ = sse.PatchElementTempl(templates.Dashboard(p))
		}
	})

	// Wallets.
	r.Get("/wallets", func(w http.ResponseWriter, r *http.Request) {
		p := demo.WalletsPage(time.Now())
		if r.URL.Query().Get("empty") != "" {
			p.Wallets = nil
		}
		render(w, r, templates.WalletsFull(p))
	})
	r.Get("/w/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := demo.Wallet(chi.URLParam(r, "id"), r.URL.Query().Get("tab"), time.Now())
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			render(w, r, templates.ErrorFull(demo.Shell("Not found", "wallets", "all", ""), "That wallet doesn't exist (it may have been removed)."))
			return
		}
		render(w, r, templates.WalletFull(p))
	})
	r.Get("/w/{id}/tab/{tab}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := demo.Wallet(chi.URLParam(r, "id"), chi.URLParam(r, "tab"), time.Now())
		if !ok {
			http.NotFound(w, r)
			return
		}
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.WalletTab(p))
	})

	// Add / edit a source.
	r.Get("/sources/new", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.SourcePickFull(demo.SourcePick()))
	})
	r.Get("/sources/new/{type}", func(w http.ResponseWriter, r *http.Request) {
		typ := chi.URLParam(r, "type")
		p := demo.SourceForm(typ)
		switch {
		case r.URL.Query().Get("edit") != "":
			p = demo.SourceFormEdit(typ)
		case r.URL.Query().Get("invalid") != "":
			p = demo.SourceFormInvalid(typ)
		}
		render(w, r, templates.SourceFormFull(p))
	})
	r.Get("/sources/{id}/edit", func(w http.ResponseWriter, r *http.Request) {
		typ := map[string]string{
			demo.IDCold: "bitcoin", demo.IDCoinbase: "coinbase", demo.IDKraken: "kraken",
			demo.IDPhantom: "solana", demo.IDMom: "manual",
		}[chi.URLParam(r, "id")]
		if typ == "" {
			http.Redirect(w, r, "/wallets", http.StatusSeeOther)
			return
		}
		render(w, r, templates.SourceFormFull(demo.SourceFormEdit(typ)))
	})
	r.Post("/sources/test", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.FormErrors(map[string]string{}))
		time.Sleep(700 * time.Millisecond)
		if r.PostFormValue("label") == "fail" {
			_ = sse.PatchElementTempl(templates.TestResultView(demo.TestFailed()))
			return
		}
		_ = sse.PatchElementTempl(templates.TestResultView(demo.TestOK(time.Now())))
	})
	r.Post("/sources", func(w http.ResponseWriter, r *http.Request) { saveSource(w, r) })
	r.Post("/sources/{id}", func(w http.ResponseWriter, r *http.Request) { saveSource(w, r) })
	r.Post("/sources/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		sse := datastar.NewSSE(w, r)
		_ = sse.Redirect("/")
	})

	r.Get("/settings", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.SettingsFull(demo.Settings(time.Now())))
	})
	r.Get("/error", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.ErrorFull(demo.Shell("Error", "overview", "all", ""), "The server could not read your holdings: sources.yaml is locked by another process."))
	})

	activityRoutes(r)
	reportsRoutes(r)
	alertsRoutes(r)
	portfolioRoutes(r)
	return r
}

func overview(w http.ResponseWriter, r *http.Request, pf string) {
	rng := r.URL.Query().Get("range")
	if r.URL.Query().Get("empty") != "" {
		render(w, r, templates.OverviewFull(demo.OverviewEmpty(time.Now())))
		return
	}
	p, ok := demo.OverviewFor(pf, rng, time.Now())
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		render(w, r, templates.ErrorFull(demo.Shell("Not found", "overview", "all", ""), "That portfolio doesn't exist."))
		return
	}
	render(w, r, templates.OverviewFull(p))
}

func saveSource(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseMultipartForm(1 << 20)
	sse := datastar.NewSSE(w, r)
	errs := map[string]string{}
	if r.PostFormValue("label") == "" {
		errs["label"] = "Give it a name, e.g. \"Cold storage\"."
	}
	if len(errs) > 0 {
		_ = sse.PatchElementTempl(templates.FormErrors(errs))
		return
	}
	_ = sse.Redirect("/wallets")
}

// stream sends one dashboard patch, then holds the connection open with a keep-alive
// until the client goes away, like the real /stream.
func stream(w http.ResponseWriter, r *http.Request) {
	s := readSignals(r)
	sse := datastar.NewSSE(w, r)
	// ?empty=1 previews the empty state, so do not overwrite it with demo data.
	if !strings.Contains(r.Referer(), "empty=1") {
		if p, ok := demo.OverviewFor(s.PF, s.Range, time.Now()); ok {
			_ = sse.PatchElementTempl(templates.Dashboard(p))
		}
	}
	rc := http.NewResponseController(w)
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// ---- portfolios ---------------------------------------------------------------------------

// pfState is what the preview remembers between portfolio requests, so a delete or an
// import is visible in the next patch. It resets with ?reset=1.
var pfState = struct {
	sync.Mutex
	deleted map[string]bool
	changes int // each change adds a backup, as the real store does
}{deleted: map[string]bool{}}

func portfoliosPage(r *http.Request) templates.PortfoliosPage {
	now := time.Now()
	p := demo.Portfolios(now)
	pfState.Lock()
	defer pfState.Unlock()
	q := r.URL.Query()
	if q.Get("reset") != "" {
		pfState.deleted, pfState.changes = map[string]bool{}, 0
	}
	items := p.Items[:0:0]
	for _, it := range p.Items {
		if !pfState.deleted[it.ID] {
			items = append(items, it)
		}
	}
	p.Items = items
	for i := range pfState.changes {
		row := templates.BackupRow{Name: fmt.Sprintf("new-%d.yaml", i), Time: now.Add(-time.Duration(i+1) * 20 * time.Second), Size: 1300 + int64(i)*12}
		p.Backups = append([]templates.BackupRow{row}, p.Backups...)
	}
	if q.Get("empty") != "" {
		p.Items, p.Backups = nil, nil
	}
	if q.Get("missing") != "" && len(p.Items) > 1 {
		p.Items[1].Missing = []string{"old-ledger", "paper-2019"}
	}
	if q.Get("error") != "" {
		p.LoadError = `line 7: field walet not found in type portfolios.Portfolio`
	}
	return p
}

func portfolioRoutes(r chi.Router) {
	r.Get("/portfolios", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.PortfoliosFull(portfoliosPage(r)))
	})
	r.Get("/portfolios/new", func(w http.ResponseWriter, r *http.Request) {
		p := demo.PortfolioForm("", time.Now())
		if r.URL.Query().Get("invalid") != "" {
			p = demo.PortfolioFormInvalid(time.Now())
		}
		render(w, r, templates.PortfolioFormFull(p))
	})
	r.Get("/portfolios/{id}/edit", func(w http.ResponseWriter, r *http.Request) {
		p := demo.PortfolioForm(chi.URLParam(r, "id"), time.Now())
		if p.EditID == "" {
			http.Redirect(w, r, "/portfolios", http.StatusSeeOther)
			return
		}
		render(w, r, templates.PortfolioFormFull(p))
	})
	r.Get("/portfolios/export", func(w http.ResponseWriter, r *http.Request) {
		exportYAML(w, "portfolios.yaml", "long-term", "trading", "majors")
	})
	r.Get("/portfolios/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		exportYAML(w, id+".yaml", id)
	})

	r.Post("/portfolios", savePortfolio)
	r.Post("/portfolios/{id}", savePortfolio)
	r.Post("/portfolios/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		pfState.Lock()
		pfState.deleted[chi.URLParam(r, "id")] = true
		pfState.changes++
		pfState.Unlock()
		p := portfoliosPage(r)
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.PortfolioList(p))
		_ = sse.PatchElementTempl(templates.PortfolioIO(p))
	})
	r.Post("/portfolios/import", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, hdr, err := r.FormFile("file")
		sse := datastar.NewSSE(w, r)
		time.Sleep(700 * time.Millisecond)
		switch {
		case err != nil:
			_ = sse.PatchElementTempl(templates.ImportResultView(templates.ImportOutcome{Message: "Choose a file to import."}))
		case strings.Contains(strings.ToLower(hdr.Filename), "bad"):
			_ = sse.PatchElementTempl(templates.ImportResultView(templates.ImportOutcome{Message: `Couldn't read that file: line 3: field walet not found. Nothing was changed.`}))
		default:
			msg := "Imported: 2 added, 1 replaced, 3 unchanged."
			if r.FormValue("mode") == "replace" {
				msg = "Replaced the whole list with 6 portfolios."
			}
			pfState.Lock()
			pfState.changes++
			pfState.Unlock()
			p := portfoliosPage(r)
			p.Import = templates.ImportOutcome{OK: true, Message: msg}
			_ = sse.PatchElementTempl(templates.PortfolioIO(p))
			_ = sse.PatchElementTempl(templates.PortfolioList(p))
		}
	})
	r.Post("/portfolios/restore/{name}", func(w http.ResponseWriter, r *http.Request) {
		pfState.Lock()
		pfState.changes++
		pfState.Unlock()
		p := portfoliosPage(r)
		p.Import = templates.ImportOutcome{OK: true, Message: "Restored " + chi.URLParam(r, "name") + ". The previous list was backed up first."}
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.PortfolioIO(p))
		_ = sse.PatchElementTempl(templates.PortfolioList(p))
	})
}

// savePortfolio validates like the real handler will: a name, and at least one wallet or
// asset when the "only selected" mode is chosen.
func savePortfolio(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	errs := map[string]string{}
	if strings.TrimSpace(r.PostFormValue("name")) == "" {
		errs["name"] = "Give the portfolio a name."
	}
	if r.PostFormValue("walletsMode") == "selected" && len(r.PostForm["wallets"]) == 0 {
		errs["wallets"] = "Pick at least one wallet, or choose All wallets."
	}
	if r.PostFormValue("assetsMode") == "selected" && len(r.PostForm["assets"]) == 0 && strings.TrimSpace(r.PostFormValue("extraAssets")) == "" {
		errs["assets"] = "Pick at least one asset, or choose All assets."
	}
	sse := datastar.NewSSE(w, r)
	time.Sleep(500 * time.Millisecond)
	if len(errs) > 0 {
		errs[""] = "Nothing was saved. Fix the fields below and try again."
		_ = sse.PatchElementTempl(templates.FormErrors(errs))
		return
	}
	pfState.Lock()
	pfState.changes++
	pfState.Unlock()
	_ = sse.PatchElementTempl(templates.FormErrors(map[string]string{}))
	_ = sse.Redirect("/portfolios")
}

func exportYAML(w http.ResponseWriter, filename string, ids ...string) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	var b strings.Builder
	b.WriteString("version: 1\nportfolios:\n")
	for _, id := range ids {
		fmt.Fprintf(&b, "  - { id: %s, name: %s, wallets: all, assets: all }\n", id, id)
	}
	_, _ = w.Write([]byte(b.String()))
}
