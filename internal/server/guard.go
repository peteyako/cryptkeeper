package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"crypto-portfolio/internal/portfolio"
)

// guard rejects state-changing requests that did not come from this app's own pages.
// Every POST must carry Datastar's "Datastar-Request: true" header (a custom header forces
// a CORS preflight, which this server never approves) and, when the browser sends an
// Origin, it must match the request host. With no login on the LAN, this is what stops a
// malicious page you visit from posting to http://192.168.x.x:8787.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Datastar-Request") != "true" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// basicAuth is active only when PORTFOLIO_PASSWORD is set.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	if s.Cfg.Password == "" {
		return next
	}
	wantUser := sha256.Sum256([]byte(s.Cfg.User))
	wantPass := sha256.Sum256([]byte(s.Cfg.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(u))
		gotPass := sha256.Sum256([]byte(p))
		if !ok || subtle.ConstantTimeCompare(gotUser[:], wantUser[:])&subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="CryptKeeper", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type healthJSON struct {
	Status      string            `json:"status"`
	LastSuccess *time.Time        `json:"lastSuccess,omitempty"`
	AgeSeconds  int64             `json:"ageSeconds"`
	Sources     map[string]string `json:"sources"`
}

// health returns 200 when a refresh succeeded within 3 refresh intervals, else 503.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	out := healthJSON{Status: "ok", Sources: map[string]string{}}
	last := s.Refresher.LastSuccess()
	code := http.StatusOK
	if last.IsZero() {
		out.Status, code = "starting", http.StatusServiceUnavailable
	} else {
		out.LastSuccess = &last
		out.AgeSeconds = int64(s.Now().Sub(last).Seconds())
		if s.Now().Sub(last) > 3*s.Cfg.RefreshInterval() {
			out.Status, code = "stale", http.StatusServiceUnavailable
		}
	}
	if snap, ok := s.Refresher.Latest(); ok {
		for _, wv := range snap.Wallets {
			st := "ok"
			switch wv.Status {
			case portfolio.StatusStale:
				st = "stale"
			case portfolio.StatusError:
				st = "error"
			}
			out.Sources[wv.ID] = st
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(out)
}
