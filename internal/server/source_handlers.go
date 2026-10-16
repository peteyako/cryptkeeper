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
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/web/templates"
)

func (s *Server) sourcePick(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, templates.SourcePickFull(templates.SourcePickPage{Shell: s.shell(r, "Add a source", "wallets", "all", "")}))
}

func (s *Server) sourceNew(w http.ResponseWriter, r *http.Request) {
	t := chi.URLParam(r, "type")
	if !slices.Contains(sources.Types, t) {
		http.Redirect(w, r, "/sources/new", http.StatusSeeOther)
		return
	}
	s.render(w, r, templates.SourceFormFull(templates.SourceFormPage{
		Shell:  s.shell(r, "Add a source", "wallets", "all", ""),
		Type:   t,
		Values: map[string]string{"kind": string(sources.DefaultKind(t))},
		Errors: map[string]string{},
	}))
}

func (s *Server) sourceEdit(w http.ResponseWriter, r *http.Request) {
	d, ok := s.Registry.Get(chi.URLParam(r, "id"))
	if !ok {
		http.Redirect(w, r, "/wallets", http.StatusSeeOther)
		return
	}
	masks := map[string]string{}
	if opened, err := s.Registry.Open(d); err == nil {
		for _, name := range sources.SecretFields {
			if v := *opened.Secret(name); v != "" {
				masks[name] = secrets.Mask(v)
			}
		}
	}
	s.render(w, r, templates.SourceFormFull(templates.SourceFormPage{
		Shell:       s.shell(r, "Edit "+d.Label, "wallets", "all", ""),
		Type:        d.Type,
		EditID:      d.ID,
		Values:      formValues(d),
		SecretMasks: masks,
		Errors:      map[string]string{},
	}))
}

// formValues is the non-secret re-fill for the edit form. Secrets are never included.
func formValues(d sources.Def) map[string]string {
	v := map[string]string{"label": d.Label, "kind": string(d.Kind)}
	switch d.Type {
	case sources.TypeCoinbase:
		v["keyName"] = d.KeyName
	case sources.TypeBitcoin:
		v["key"] = d.Key
		if d.Descriptor != "" {
			v["key"] = d.Descriptor
		}
		v["scriptType"] = d.ScriptType
	case sources.TypeSolana:
		v["address"] = d.Address
	case sources.TypeManual:
		v["holdings"] = manual.FormatLines(d.Holdings)
	}
	return v
}

// parseSourceForm turns the posted form into a Def with plaintext secrets. Field errors
// are keyed by form field name.
func (s *Server) parseSourceForm(r *http.Request, editID string) (sources.Def, map[string]string) {
	errs := map[string]string{}
	if err := parseBody(r); err != nil {
		errs[""] = "Could not read the form: " + err.Error()
		return sources.Def{}, errs
	}
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }

	d := sources.Def{Type: f("type"), Label: f("label"), Kind: core.Kind(f("kind"))}
	if editID != "" {
		existing, ok := s.Registry.Get(editID)
		if !ok {
			errs[""] = "That source no longer exists."
			return d, errs
		}
		d.ID, d.Type = existing.ID, existing.Type // type can't change on edit
	}
	if !slices.Contains(sources.Types, d.Type) {
		errs[""] = "Unknown source type."
		return d, errs
	}
	if d.Label == "" {
		errs["label"] = "Give it a name, e.g. \"Cold storage\"."
	}
	if d.Kind == "" {
		d.Kind = sources.DefaultKind(d.Type)
	}
	isEdit := editID != ""
	switch d.Type {
	case sources.TypeCoinbase:
		d.KeyName, d.PrivateKey = f("keyName"), strings.TrimSpace(r.PostFormValue("privateKey"))
		if d.KeyName == "" {
			errs["keyName"] = "Paste the API key name from the Coinbase Developer Platform."
		}
		if d.PrivateKey == "" && !isEdit {
			errs["privateKey"] = "Paste the private key."
		}
	case sources.TypeKraken:
		d.APIKey, d.APISecret = f("apiKey"), f("apiSecret")
		if d.APIKey == "" && !isEdit {
			errs["apiKey"] = "Paste the API key."
		}
		if d.APISecret == "" && !isEdit {
			errs["apiSecret"] = "Paste the private key (secret)."
		}
	case sources.TypeBitcoin:
		d.ScriptType = f("scriptType")
		key := f("key")
		if file, _, err := r.FormFile("walletFile"); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(file, 1<<20))
			file.Close()
			if len(raw) > 0 && s.WalletFileToDescriptor != nil {
				desc, err := s.WalletFileToDescriptor(raw)
				if err != nil {
					errs["walletFile"] = "Couldn't read that wallet file: " + err.Error()
				} else {
					key = desc
					d.ImportedVia = "wallet file"
				}
			}
		}
		switch {
		case key == "":
			if errs["walletFile"] == "" {
				errs["key"] = "Paste an extended public key (xpub/zpub), an output descriptor or an address, or upload a wallet file."
			}
		case s.DetectBitcoin != nil:
			kind, err := s.DetectBitcoin(key)
			if err != nil {
				errs["key"] = err.Error()
			} else if kind == "descriptor" {
				d.Descriptor = key
			} else {
				d.Key = key
			}
		default:
			d.Key = key
		}
	case sources.TypeSolana:
		d.Address = f("address")
		if d.Address == "" {
			errs["address"] = "Paste the wallet's public address."
		}
	case sources.TypeManual:
		hs, err := manual.ParseLines(r.PostFormValue("holdings"))
		if err != nil {
			errs["holdings"] = err.Error()
		}
		d.Holdings = hs
		d.Kind = core.KindManual
	}
	if lower := strings.ToLower(r.PostFormValue("key") + r.PostFormValue("address")); sources.LooksLikeSeed(lower) {
		errs[""] = "That looks like a seed phrase or private key. CryptKeep only needs public keys — never enter a seed phrase here."
	}
	return d, errs
}

// formEditID reads the hidden "editId" field (the Test button posts the form to
// /sources/test, so the id of the source being edited travels in the form).
func formEditID(r *http.Request) string {
	_ = parseBody(r)
	return strings.TrimSpace(r.PostFormValue("editId"))
}

// parseBody parses a Datastar form post: multipart when the form has a file input
// (wallet file upload), URL-encoded otherwise. Parsing twice is a no-op.
func parseBody(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(1 << 20)
	}
	return r.ParseForm()
}

// mergeForTest fills blank secrets from the stored source when testing an edit.
func (s *Server) mergeForTest(d sources.Def, editID string) sources.Def {
	if editID == "" {
		return d
	}
	stored, ok := s.Registry.Get(editID)
	if !ok {
		return d
	}
	opened, err := s.Registry.Open(stored)
	if err != nil {
		return d
	}
	for _, name := range sources.SecretFields {
		if p := d.Secret(name); *p == "" {
			*p = *opened.Secret(name)
		}
	}
	return d
}

// sourceTest builds the source from the posted form and fetches it once.
func (s *Server) sourceTest(w http.ResponseWriter, r *http.Request) {
	editID := r.URL.Query().Get("id")
	if editID == "" {
		editID = formEditID(r)
	}
	d, errs := s.parseSourceForm(r, editID)
	sse := datastar.NewSSE(w, r)
	if len(errs) > 0 {
		_ = sse.PatchElementTempl(templates.FormErrors(errs))
		_ = sse.PatchElementTempl(templates.TestResultView(templates.TestResult{OK: false, Message: "Fix the highlighted fields first."}))
		return
	}
	_ = sse.PatchElementTempl(templates.FormErrors(map[string]string{}))
	d = s.mergeForTest(d, editID)
	if d.ID == "" {
		d.ID = "test"
	}
	res := s.testSource(r.Context(), d)
	_ = sse.PatchElementTempl(templates.TestResultView(res))
}

func (s *Server) testSource(ctx context.Context, d sources.Def) templates.TestResult {
	src, err := s.Registry.Build(d)
	if err != nil {
		return templates.TestResult{OK: false, Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	res, err := src.Fetch(ctx)
	if err != nil {
		return templates.TestResult{OK: false, Message: err.Error()}
	}
	prices := map[string]core.Price{}
	if reqs := portfolio.PriceRequests([]core.WalletResult{res}); s.Prices != nil && len(reqs) > 0 {
		prices = s.Prices(ctx, reqs)
	}
	var rows []portfolio.HoldingRow
	for _, h := range res.Holdings {
		rows = append(rows, portfolio.PriceRow(h, prices))
	}
	msg := fmt.Sprintf("Connected. Found %s.", plural(len(rows), "balance"))
	if len(res.Warnings) > 0 {
		msg += " " + strings.Join(res.Warnings, " ")
	}
	return templates.TestResult{OK: true, Message: msg, Rows: rows}
}

func (s *Server) sourceSave(w http.ResponseWriter, r *http.Request) {
	editID := chi.URLParam(r, "id")
	d, errs := s.parseSourceForm(r, editID)
	sse := datastar.NewSSE(w, r)
	if len(errs) == 0 {
		if d.ID == "" {
			d.ID = s.Registry.UniqueID(d.Label)
		}
		// Validate the type-specific input by building it (parses keys/addresses, no network).
		if _, err := s.Registry.Build(s.mergeForTest(d, editID)); err != nil {
			errs[""] = err.Error()
		}
	}
	if len(errs) == 0 {
		if _, err := s.Registry.Upsert(d); err != nil {
			errs[""] = err.Error()
		}
	}
	if len(errs) > 0 {
		_ = sse.PatchElementTempl(templates.FormErrors(errs))
		return
	}
	s.Refresher.Kick()
	if s.Syncer != nil {
		s.Syncer.Kick()
	}
	_ = sse.Redirect("/w/" + d.ID)
}

func (s *Server) sourceDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sse := datastar.NewSSE(w, r)
	if err := s.Registry.Delete(id); err != nil {
		_ = sse.PatchElementTempl(templates.FormErrors(map[string]string{"": err.Error()}))
		return
	}
	s.cleanupSource(r.Context(), id)
	s.Refresher.Kick()
	_ = sse.Redirect("/")
}
