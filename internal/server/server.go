// Package server is the HTTP layer: chi routes, Datastar SSE handlers, the mutation
// guard, optional basic auth and the health endpoint.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"crypto-portfolio/internal/activity"
	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/store"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/web/static"
)

// HistoricalPrices is the daily close of an asset by ticker or Solana mint (ok false when
// unknown); *prices.Service implements it.
type HistoricalPrices interface {
	Historical(ctx context.Context, symbol string, day time.Time) (float64, bool, error)
	HistoricalMint(ctx context.Context, mint string, day time.Time) (float64, bool, error)
}

// Deps are the server's collaborators, wired in main.
type Deps struct {
	Cfg     config.Config
	DataDir string
	// HTTPClient downloads newer IRS form revisions; nil disables downloads.
	HTTPClient *http.Client
	Version    string
	Log        *slog.Logger
	Box        *secrets.Box // the registry's key: opens encrypted imports, seals exports when Cfg.EncryptExports()
	Registry   *sources.Registry
	Portfolios *portfolios.Store
	Activity   *activity.Store
	Syncer     *activity.Syncer       // optional: kicked after a source is added
	CostBasis  *costbasis.ManualStore // the retired manual costs: kept for backup only
	AlertRules *alerts.RuleStore
	Alerts     *alerts.Engine
	DB         *store.DB // UI settings changed from the Settings page
	Refresher  *portfolio.Refresher
	Broker     *portfolio.Broker
	History    *portfolio.History
	Prices     portfolio.PriceFunc
	// Historical prices a past day's close, for the tax report's year-end holdings.
	// Optional: without it those holdings are unpriced.
	Historical HistoricalPrices

	// Tax lots: the settings (data/tax.yaml), the closed years (data/tax/<year>.yaml) and the
	// tax timezone (nil = time.Local). They feed the cost basis and P&L everywhere.
	TaxSettings *tax.SettingsStore
	TaxYears    *tax.YearStore
	TaxLoc      *time.Location

	// Bitcoin helpers, injected so the server doesn't depend on the bitcoin package.
	DetectBitcoin          func(input string) (kind string, err error)
	WalletFileToDescriptor func(data []byte) (string, error)
	// ParseLedgerExport reads Ledger Wallet's account JSON into its xpub and script type.
	ParseLedgerExport func(input string) (key, scriptType string, ok bool)
	// Blockstream checks a Blockstream API key saved from Settings and hands it to the wallets.
	Blockstream BlockstreamKeys

	// Now is overridable in tests.
	Now func() time.Time
}

type Server struct {
	Deps
	taxes taxCache
	// closeMu serialises closing and reopening a tax year (check, compute, save).
	closeMu sync.Mutex
	// draftMu serialises the Koinly import draft's load → change → save.
	draftMu sync.Mutex
}

// New builds the router.
func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{Deps: d}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, s.securityHeaders)

	// Unauthenticated: health and PWA assets (the install prompt fetches them without creds).
	r.Get("/healthz", s.health)
	r.Get("/manifest.webmanifest", s.manifest)
	r.Handle("/static/icons/*", s.staticHandler())

	r.Group(func(r chi.Router) {
		r.Use(s.basicAuth, s.guard)
		r.Handle("/static/*", s.staticHandler())

		r.Get("/", s.overview)
		r.Get("/p/{pf}", s.overview)
		r.Get("/stream", s.stream)
		r.Get("/view", s.view)
		r.Get("/chart", s.chart)
		r.Post("/chart/backfill", s.chartBackfill)
		r.Post("/refresh", s.refresh)
		r.Get("/portfolio.json", s.portfolioJSON)

		r.Get("/a/{asset}", s.coin)
		r.Post("/a/{asset}/chart", s.coinChart)

		r.Get("/wallets", s.wallets)
		r.Get("/w/{id}", s.wallet)
		r.Get("/w/{id}/tab/{tab}", s.walletTab)

		r.Get("/sources/new", s.sourcePick)
		r.Get("/sources/new/{type}", s.sourceNew)
		r.Get("/sources/{id}/edit", s.sourceEdit)
		r.Post("/sources/test", s.sourceTest)
		r.Post("/sources", s.sourceSave)
		r.Post("/sources/{id}", s.sourceSave)
		r.Post("/sources/{id}/delete", s.sourceDelete)

		r.Get("/portfolios", s.portfoliosManager)
		r.Get("/portfolios/new", s.portfolioNew)
		r.Get("/portfolios/export", s.portfolioExport)
		r.Get("/portfolios/{id}/edit", s.portfolioEdit)
		r.Get("/portfolios/{id}/export", s.portfolioExport)
		r.Post("/portfolios", s.portfolioSave)
		r.Post("/portfolios/import", s.portfolioImport)
		r.Post("/portfolios/restore/{name}", s.portfolioRestore)
		r.Post("/portfolios/{id}", s.portfolioSave)
		r.Post("/portfolios/{id}/delete", s.portfolioDelete)

		r.Get("/activity", s.activityFull)
		r.Get("/activity/list", s.activityList)
		r.Get("/activity/sync", s.activitySync)

		r.Get("/reports", s.reports)
		r.Post("/reports/import", s.csvImport)
		r.Get("/reports/koinly", s.koinlyPage)
		r.Post("/reports/koinly/upload", s.koinlyUpload)
		r.Post("/reports/koinly/map", s.koinlyMap)
		r.Post("/reports/koinly/accept", s.koinlyAccept)
		r.Post("/reports/koinly/discard", s.koinlyDiscard)
		r.Get("/reports/tax", s.taxView)
		s.registerTaxDownloads(r)
		r.Post("/reports/tax/year", s.taxViewYear)
		r.Post("/reports/tax/close", s.taxClose)
		r.Post("/reports/tax/close-cancel", s.taxCloseCancel)
		r.Post("/reports/tax/reopen", s.taxReopen)
		r.Post("/reports/tax/tag", s.taxTag)
		r.Post("/reports/tax/tag-bulk", s.taxTagBulk)
		r.Post("/reports/tax/tag/{tx}/delete", s.taxTagDelete)
		r.Post("/reports/tax/price", s.taxPrice)
		r.Post("/reports/tax/writeoff", s.taxWriteOff)
		r.Post("/reports/tax/writeoff/{i}/delete", s.taxWriteOffDelete)

		r.Get("/export/holdings.csv", s.exportHoldings)
		r.Get("/export/history.csv", s.exportHistory)
		r.Get("/export/pnl.csv", s.exportPnL)
		r.Get("/export/activity.csv", s.exportActivity)

		r.Get("/alerts", s.alertsFull)
		r.Get("/alerts/new", s.alertNew)
		r.Get("/alerts/{id}/edit", s.alertEdit)
		r.Post("/alerts", s.alertSave)
		r.Post("/alerts/test", s.alertTest)
		r.Post("/alerts/{id}", s.alertSave)
		r.Post("/alerts/{id}/delete", s.alertDelete)

		r.Get("/settings", s.settings)
		r.Post("/settings/logos", s.settingsLogos)
		r.Post("/settings/prices", s.settingsPrices)
		r.Post("/settings/bitcoin", s.settingsBitcoin)
		r.Get("/backup/export", s.backupExport)
		r.Post("/backup/import", s.backupImport)
	})
	return r
}

func (s *Server) staticHandler() http.Handler {
	sub, _ := fs.Sub(static.FS, ".")
	fsrv := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			// Versioned URL (content hash): safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		fsrv.ServeHTTP(w, r)
	})
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	raw, err := static.FS.ReadFile("manifest.webmanifest")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Write(raw)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// ctxTimeout bounds work done on behalf of one request.
func ctxTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
