package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/backup"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/store"
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
		PriceKeyFromEnv: s.Cfg.CoinGeckoKey != "", PriceKey: s.maskedPriceKey(r.Context()),
		BlockstreamFromEnv: s.Cfg.BlockstreamClientID != "", BlockstreamID: s.maskedBlockstreamID(r.Context()),
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

// PricesKeyKV is the kv key holding the box-sealed CoinGecko key saved from Settings.
const PricesKeyKV = "prices.coingeckoKey"

// maskedPriceKey returns the saved CoinGecko key masked, or "" when none is saved or readable.
func (s *Server) maskedPriceKey(ctx context.Context) string {
	if s.DB == nil || s.Box == nil {
		return ""
	}
	var sealed string
	if ok, err := s.DB.GetJSON(ctx, PricesKeyKV, &sealed); err != nil || !ok {
		return ""
	}
	plain, err := s.Box.Open(sealed)
	if err != nil {
		s.Log.Warn("open coingecko key", "err", err)
		return ""
	}
	return secrets.Mask(plain)
}

// settingsPrices saves (sealed) or clears the CoinGecko demo key.
func (s *Server) settingsPrices(w http.ResponseWriter, r *http.Request) {
	_ = parseBody(r)
	if s.DB != nil && s.Box != nil && s.Cfg.CoinGeckoKey == "" {
		if r.URL.Query().Get("clear") == "1" || r.PostFormValue("clear") == "1" {
			if err := s.DB.DeleteKV(r.Context(), PricesKeyKV); err != nil {
				s.Log.Warn("clear coingecko key", "err", err)
			}
		} else if key := strings.TrimSpace(r.PostFormValue("coingeckoKey")); key != "" {
			if sealed, err := s.Box.Seal(key); err != nil {
				s.Log.Warn("seal coingecko key", "err", err)
			} else if err := s.DB.PutJSON(r.Context(), PricesKeyKV, sealed); err != nil {
				s.Log.Warn("save coingecko key", "err", err)
			}
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.SettingsPrices(s.settingsPage(r)))
}

// BlockstreamKeyKV is the kv key holding the box-sealed Blockstream API key saved from Settings.
const BlockstreamKeyKV = "bitcoin.blockstreamKey"

// BlockstreamKey is a Blockstream Explorer API key: an OAuth client ID and secret.
type BlockstreamKey struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

// BlockstreamKeys checks a Blockstream API key and hands it to the Bitcoin wallets.
type BlockstreamKeys interface {
	Check(ctx context.Context, id, secret string) error
	SetCredentials(id, secret string)
}

// LoadBlockstreamKey reads the key saved from Settings; ok is false when none is saved.
func LoadBlockstreamKey(ctx context.Context, db *store.DB, box *secrets.Box) (k BlockstreamKey, ok bool, err error) {
	var sealed string
	if ok, err = db.GetJSON(ctx, BlockstreamKeyKV, &sealed); err != nil || !ok {
		return k, false, err
	}
	plain, err := box.Open(sealed)
	if err != nil {
		return k, false, err
	}
	if err := json.Unmarshal([]byte(plain), &k); err != nil {
		return k, false, err
	}
	return k, true, nil
}

// maskedBlockstreamID returns the saved key's client ID masked, or "" when none is saved or readable.
func (s *Server) maskedBlockstreamID(ctx context.Context) string {
	if s.DB == nil || s.Box == nil {
		return ""
	}
	k, ok, err := LoadBlockstreamKey(ctx, s.DB, s.Box)
	if err != nil {
		s.Log.Warn("open blockstream key", "err", err)
	}
	if !ok {
		return ""
	}
	return secrets.Mask(k.ID)
}

// settingsBitcoin checks and saves (sealed) or clears the Blockstream API key. A key is only
// saved once Blockstream has issued a token for it.
func (s *Server) settingsBitcoin(w http.ResponseWriter, r *http.Request) {
	_ = parseBody(r)
	note, noteErr := "", false
	if s.DB != nil && s.Box != nil && s.Blockstream != nil && s.Cfg.BlockstreamClientID == "" {
		ctx := r.Context()
		id, secret := strings.TrimSpace(r.PostFormValue("blockstreamId")), strings.TrimSpace(r.PostFormValue("blockstreamSecret"))
		switch {
		case r.URL.Query().Get("clear") == "1" || r.PostFormValue("clear") == "1":
			if err := s.DB.DeleteKV(ctx, BlockstreamKeyKV); err != nil {
				s.Log.Warn("clear blockstream key", "err", err)
			}
			s.Blockstream.SetCredentials("", "")
		case id == "" || secret == "":
			note, noteErr = "Enter both the client ID and the client secret.", true
		default:
			if err := s.Blockstream.Check(ctx, id, secret); err != nil {
				note, noteErr = err.Error(), true
			} else if err := s.saveBlockstreamKey(ctx, BlockstreamKey{ID: id, Secret: secret}); err != nil {
				s.Log.Warn("save blockstream key", "err", err)
				note, noteErr = "Couldn't save the key.", true
			} else {
				s.Blockstream.SetCredentials(id, secret)
				note = "Saved. Bitcoin wallets now go through Blockstream's API."
				if s.Refresher != nil {
					s.Refresher.Kick()
				}
			}
		}
	}
	p := s.settingsPage(r)
	p.BlockstreamNote, p.BlockstreamNoteErr = note, noteErr
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.SettingsBitcoin(p))
}

func (s *Server) saveBlockstreamKey(ctx context.Context, k BlockstreamKey) error {
	plain, err := json.Marshal(k)
	if err != nil {
		return err
	}
	sealed, err := s.Box.Seal(string(plain))
	if err != nil {
		return err
	}
	return s.DB.PutJSON(ctx, BlockstreamKeyKV, sealed)
}

func (s *Server) backupStores() backup.Stores {
	return backup.Stores{
		Sources: s.Registry, Portfolios: s.Portfolios, Alerts: s.AlertRules, CostBasis: s.CostBasis,
		Tax: s.TaxSettings, TaxYears: s.TaxYears,
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

// backupRestoreLimit is the biggest backup file a restore reads. A larger one is refused
// rather than read cut off, which would restore only part of it.
const (
	backupRestoreLimit = 64 << 20
	backupTooBig       = "That file is too large to restore (the limit is 64 MB), so nothing was restored."
)

// backupImport restores a backup (or an older sources or portfolios export).
func (s *Server) backupImport(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{}
	r.Body = http.MaxBytesReader(w, r.Body, backupRestoreLimit+multipartSlack)
	var raw []byte
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		outcome.Message = uploadErrMsg(err, "Choose a backup file to restore.", backupTooBig)
	} else if file, _, err := r.FormFile("file"); err != nil {
		outcome.Message = "Choose a backup file to restore."
	} else {
		raw, _ = io.ReadAll(io.LimitReader(file, backupRestoreLimit+1))
		file.Close()
		if len(raw) > backupRestoreLimit {
			outcome.Message = backupTooBig
		}
	}
	if outcome.Message == "" {
		raw, err := s.Box.OpenFile(raw)
		var res backup.Result
		if err == nil {
			res, err = backup.Import(s.backupStores(), raw, r.PostFormValue("mode") == "replace")
		}
		switch {
		case err != nil && res.Sources == nil && res.Portfolios == nil && res.Alerts == nil && !res.CostBasis && !res.TouchedTax():
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
		if res.TouchedTax() {
			s.invalidateTax()
		}
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
		s.invalidateTax()
	}
}
