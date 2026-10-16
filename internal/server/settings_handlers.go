package server

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/backup"
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
		Sync: s.syncRows(r.Context()), Now: s.Now(), EncryptedExports: s.Cfg.EncryptExports(),
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

func (s *Server) backupStores() backup.Stores {
	return backup.Stores{
		Sources: s.Registry, Portfolios: s.Portfolios, Alerts: s.AlertRules, CostBasis: s.CostBasis,
		OnSourceRemoved: func(id string) { s.cleanupSource(context.Background(), id) },
	}
}

// backupExport downloads everything the user configured; ?secrets=1 keeps sealed secrets.
// With CRYPTKEEP_SECRET_KEY set the whole file is encrypted, secrets always included.
func (s *Server) backupExport(w http.ResponseWriter, r *http.Request) {
	encrypt := s.Cfg.EncryptExports()
	withSecrets := encrypt || r.URL.Query().Get("secrets") == "1"
	raw, err := backup.Export(s.backupStores(), withSecrets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := "cryptkeep-backup"
	if withSecrets && !encrypt {
		name = "cryptkeep-backup-with-encrypted-secrets"
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

// sealExport encrypts a YAML export when the user set CRYPTKEEP_SECRET_KEY, and marks the
// download name as encrypted. Otherwise it returns raw and name unchanged.
func (s *Server) sealExport(raw []byte, name string) ([]byte, string, error) {
	if !s.Cfg.EncryptExports() {
		return raw, name, nil
	}
	sealed, err := s.Box.SealFile(raw)
	return sealed, name + "-encrypted", err
}

// backupImport restores a backup (or an older sources or portfolios export).
func (s *Server) backupImport(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{}
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		outcome.Message = "Choose a backup file to restore."
	} else if file, _, err := r.FormFile("file"); err != nil {
		outcome.Message = "Choose a backup file to restore."
	} else {
		raw, _ := io.ReadAll(io.LimitReader(file, 4<<20))
		file.Close()
		raw, err := s.Box.OpenFile(raw)
		var res backup.Result
		if err == nil {
			res, err = backup.Import(s.backupStores(), raw, r.PostFormValue("mode") == "replace")
		}
		switch {
		case err != nil && res.Sources == nil && res.Portfolios == nil && res.Alerts == nil && !res.CostBasis:
			outcome.Message = "Nothing was restored: " + err.Error()
		case err != nil:
			outcome.Message = res.Summary() + " Then stopped: " + err.Error()
		default:
			outcome.OK = true
			outcome.Message = res.Summary()
			if res.Sources != nil {
				outcome.Message += " Exchange sources restored without secrets need their keys re-entered (Edit)."
			}
		}
		s.costs.invalidate()
		s.Refresher.Kick()
		if s.Syncer != nil {
			s.Syncer.Kick()
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.BackupImportResult(outcome))
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
