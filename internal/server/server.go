// Package server is the HTTP layer: chi routes, Datastar SSE handlers, the mutation
// guard, optional basic auth and the health endpoint.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/web/static"
)

// Deps are the server's collaborators, wired in main.
type Deps struct {
	Cfg       config.Config
	DataDir   string
	Version   string
	Log       *slog.Logger
	Registry  *sources.Registry
	Refresher *portfolio.Refresher
	Broker    *portfolio.Broker
	History   *portfolio.History
	Prices    portfolio.PriceFunc

	// Bitcoin helpers, injected so the server doesn't depend on the bitcoin package.
	DetectBitcoin          func(input string) (kind string, err error)
	WalletFileToDescriptor func(data []byte) (string, error)

	// Now is overridable in tests.
	Now func() time.Time
}

type Server struct {
	Deps
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
		r.Post("/refresh", s.refresh)
		r.Get("/portfolio.json", s.portfolioJSON)

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

		r.Get("/settings", s.settings)
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
