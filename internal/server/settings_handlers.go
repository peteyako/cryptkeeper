package server

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/sources"
	"crypto-portfolio/web/templates"
)

// uiSettings are display options changed from Settings and stored in the database
// (config.yaml supplies the defaults and is never written by the app).
type uiSettings struct {
	ShowLogos *bool `json:"showLogos,omitempty"`
}

const uiSettingsKey = "ui.settings"

func (s *Server) loadUISettings(ctx context.Context) uiSettings {
	var st uiSettings
	if s.DB != nil {
		if _, err := s.DB.GetJSON(ctx, uiSettingsKey, &st); err != nil {
			s.Log.Warn("load ui settings", "err", err)
		}
	}
	return st
}

// showLogos reports whether token logos replace monograms.
func (s *Server) showLogos(ctx context.Context) bool {
	if st := s.loadUISettings(ctx); st.ShowLogos != nil {
		return *st.ShowLogos
	}
	return s.Cfg.ShowTokenLogos
}

func (s *Server) settingsPage(r *http.Request) templates.SettingsPage {
	snap, _ := s.latest()
	wallets := snap.Wallets
	// Include sources that have no snapshot entry yet.
	have := map[string]bool{}
	for _, wv := range wallets {
		have[wv.ID] = true
	}
	for _, d := range s.Registry.Defs() {
		if !have[d.ID] {
			wallets = append(wallets, pendingWallet(d))
		}
	}
	return templates.SettingsPage{
		Shell: s.shell(r, "Settings", "settings", "all", ""), Wallets: wallets,
		DataDir: s.DataDir, AuthEnabled: s.Cfg.Password != "", Version: s.Version,
		Sync: s.syncRows(r.Context()), Now: s.Now(),
	}
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, templates.SettingsFull(s.settingsPage(r)))
}

// settingsLogos saves the token-logos toggle.
func (s *Server) settingsLogos(w http.ResponseWriter, r *http.Request) {
	on := false
	if err := parseBody(r); err == nil {
		on = r.PostFormValue("showLogos") == "on"
	}
	st := s.loadUISettings(r.Context())
	st.ShowLogos = &on
	if s.DB != nil {
		if err := s.DB.PutJSON(r.Context(), uiSettingsKey, st); err != nil {
			s.Log.Warn("save ui settings", "err", err)
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.SettingsDisplay(s.settingsPage(r)))
}

// sourcesExport downloads data/sources.yaml; ?secrets=1 keeps the sealed secrets.
func (s *Server) sourcesExport(w http.ResponseWriter, r *http.Request) {
	withSecrets := r.URL.Query().Get("secrets") == "1"
	raw, err := s.Registry.Export(withSecrets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := "sources"
	if withSecrets {
		name = "sources-with-encrypted-secrets"
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.yaml"`, name, s.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}

// sourcesImport merges or replaces sources from an exported file.
func (s *Server) sourcesImport(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		outcome.Message = "Choose a sources file to import."
	} else if file, _, err := r.FormFile("file"); err != nil {
		outcome.Message = "Choose a sources file to import."
	} else {
		raw, _ := io.ReadAll(io.LimitReader(file, 1<<20))
		file.Close()
		f, err := sources.ParseFile(raw) // Import validates each source, incl. the seed-phrase check
		if err != nil {
			outcome.Message = "Nothing was imported: " + err.Error()
		} else {
			res, err := s.Registry.Import(f, r.PostFormValue("mode") == "replace")
			if err != nil {
				outcome.Message = "Nothing was imported: " + err.Error()
			} else {
				for _, id := range res.Removed {
					s.cleanupSource(r.Context(), id)
				}
				outcome.OK = true
				outcome.Message = fmt.Sprintf("Imported: %d added, %d replaced, %d unchanged", res.Added, res.Replaced, res.Unchanged)
				if len(res.Removed) > 0 {
					outcome.Message += fmt.Sprintf(", %d removed", len(res.Removed))
				}
				outcome.Message += ". Exchange sources without secrets need their keys re-entered (Edit)."
				s.Refresher.Kick()
				if s.Syncer != nil {
					s.Syncer.Kick()
				}
			}
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.SourcesImportResult(outcome))
}

// cleanupSource removes what belongs to a deleted source: portfolio references and activity.
func (s *Server) cleanupSource(ctx context.Context, id string) {
	s.removeFromPortfolios(id)
	if s.Activity != nil {
		if err := s.Activity.DeleteSource(ctx, id); err != nil {
			s.Log.Warn("delete activity", "source", id, "err", err)
		}
		s.costs.invalidate()
	}
}
