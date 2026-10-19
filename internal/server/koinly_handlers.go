package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/tax"
	"cryptkeeper/internal/tax/koinly"
	"cryptkeeper/web/templates"
)

// The Koinly import wizard (/reports/koinly): upload Koinly's year-end reports, map its
// wallets and assets to CryptKeeper's, reconcile Koinly's closing quantities against what
// CryptKeeper's wallets held at the cutover, and save the year as closed. The upload is kept
// as a draft in the kv table until it is accepted or discarded.

const (
	koinlyDraftKey = "tax.koinly.draft"
	koinlyMaxFile  = 8 << 20  // per uploaded report
	koinlyMaxBody  = 64 << 20 // per upload request, whatever the number of files
)

// koinlyDraft is the import in progress.
type koinlyDraft struct {
	Year     int
	AsOf     time.Time // the holdings report's date, in the tax timezone
	Holdings []koinly.Holding
	Txns     []koinly.Txn
	Gains    []tax.Disposal
	Income   []tax.Income
	// Files is the uploaded file name per report kind (koinly.Kind): a report uploaded
	// again replaces the earlier one, name included.
	Files map[string]string
	// WalletMap: Koinly wallet name → source ID | tax.Untracked. AssetMap: Koinly
	// "SYM (Name)" → asset key | "" (skip). ExtraBasis: asset key → the user's total USD
	// basis for CryptKeeper's units beyond Koinly's. SkipOK: Koinly assets the user chose to
	// skip although their symbol matched several assets ("skip anyway").
	WalletMap  map[string]string
	AssetMap   map[string]string
	ExtraBasis map[string]float64
	SkipOK     map[string]bool
	// WalletWhy says why a Koinly wallet is mapped as it is, for the ones CryptKeeper matched;
	// choosing another wallet drops it.
	WalletWhy map[string]string
}

// koinlyKinds are the report kinds the import uses, in display order.
var koinlyKinds = []koinly.Kind{koinly.KindHoldings, koinly.KindTransactions, koinly.KindGains, koinly.KindIncome}

func (d koinlyDraft) has(k koinly.Kind) bool { return d.Files[string(k)] != "" }

// cutover is the first instant of the year after the imported one.
func (d koinlyDraft) cutover(loc *time.Location) time.Time {
	return time.Date(d.Year+1, 1, 1, 0, 0, 0, 0, loc)
}

func (s *Server) loadDraft(ctx context.Context) (koinlyDraft, bool, error) {
	var d koinlyDraft
	if s.DB == nil {
		return d, false, nil
	}
	ok, err := s.DB.GetJSON(ctx, koinlyDraftKey, &d)
	return d, ok, err
}

// koinlyState is what CryptKeeper knows that the wizard reconciles against.
type koinlyState struct {
	snap     portfolio.Snapshot
	ready    bool      // a snapshot exists
	txs      []core.Tx // all activity, without CSV rows the live API already synced
	txErr    error
	backfill map[string]bool // source ID → its activity backfill is done
}

func (s *Server) koinlyState(ctx context.Context) koinlyState {
	var st koinlyState
	st.snap, st.ready = s.latest()
	if s.Activity == nil {
		return st
	}
	st.txs, st.txErr = s.taxActivity(ctx)
	statuses, err := s.Activity.Statuses(ctx)
	if err != nil && st.txErr == nil {
		st.txErr = err
	}
	st.backfill = map[string]bool{}
	for id, ss := range statuses {
		st.backfill[id] = ss.BackfillDone
	}
	return st
}

// ---- handlers -------------------------------------------------------------------------------

func (s *Server) koinlyPage(w http.ResponseWriter, r *http.Request) {
	p := templates.KoinlyPage{Shell: s.shell(r, "Import from Koinly", "reports", "all", ""), Wizard: s.koinlyWizard(r.Context())}
	s.render(w, r, templates.KoinlyFull(p))
}

// patchKoinly re-renders the wizard with the outcomes of the action just taken.
func (s *Server) patchKoinly(w http.ResponseWriter, r *http.Request, upload, accept templates.ImportOutcome) {
	wiz := s.koinlyWizard(r.Context())
	if upload.Message != "" {
		wiz.Outcome = upload
	}
	if accept.Message != "" && accept.Message != wiz.Blocked { // a blocked Accept already says why
		wiz.AcceptOutcome = accept
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.KoinlyWizardView(wiz))
}

// koinlyUpload parses any number of Koinly reports (field "file", 8 MB each) into the
// draft. Every file must be a Koinly report of the same year as the others and as the
// draft; otherwise nothing changes.
func (s *Server) koinlyUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, koinlyMaxBody)
	var outcome templates.ImportOutcome
	var tooBig *http.MaxBytesError
	switch err := r.ParseMultipartForm(koinlyMaxFile); {
	case errors.As(err, &tooBig):
		outcome.Message = fmt.Sprintf("That upload is too large: upload at most %d MB at a time.", koinlyMaxBody>>20)
	case err != nil || len(r.MultipartForm.File["file"]) == 0:
		outcome.Message = "Choose Koinly's CSV reports to upload."
	default:
		s.draftMu.Lock()
		outcome = s.koinlyMerge(r.Context(), r.MultipartForm.File["file"])
		s.draftMu.Unlock()
	}
	s.patchKoinly(w, r, outcome, templates.ImportOutcome{})
}

// upload is one parsed Koinly report.
type upload struct {
	name     string
	kind     koinly.Kind
	year     int
	asOf     time.Time
	holdings []koinly.Holding
	txns     []koinly.Txn
	gains    []tax.Disposal
	income   []tax.Income
}

// koinlyMerge parses the files and merges them into the draft: each report replaces the
// draft's report of the same kind. The mappings of new wallet and asset names are
// suggested; the ones already chosen are kept. The caller holds s.draftMu.
func (s *Server) koinlyMerge(ctx context.Context, files []*multipart.FileHeader) templates.ImportOutcome {
	fail := func(format string, args ...any) templates.ImportOutcome {
		return templates.ImportOutcome{Message: "Nothing was loaded: " + fmt.Sprintf(format, args...)}
	}
	if s.DB == nil {
		return fail("storage is not available.")
	}
	d, hasDraft, err := s.loadDraft(ctx)
	if err != nil {
		return fail("couldn't read the import in progress (%v).", err)
	}
	loc := s.taxLoc()
	var ups []upload
	var skipped []string
	byKind := map[koinly.Kind]string{}
	for _, fh := range files {
		u, err := parseUpload(fh, loc)
		switch {
		case err != nil:
			return fail("%s: %v.", fh.Filename, err)
		case u.kind == koinly.KindOtherGains:
			skipped = append(skipped, fh.Filename)
			continue
		case byKind[u.kind] != "":
			return fail("%s and %s are both %s; upload one of them.", byKind[u.kind], u.name, kindName(u.kind))
		}
		byKind[u.kind] = u.name
		ups = append(ups, u)
	}
	if len(ups) == 0 {
		return fail("%s is Koinly's other gains report, which the import doesn't use. Upload the holdings, transaction history, capital gains and income reports.", strings.Join(skipped, ", "))
	}
	for _, u := range ups[1:] {
		if u.year != ups[0].year {
			return fail("%s is for %d but %s is for %d; upload the reports of one year.", ups[0].name, ups[0].year, u.name, u.year)
		}
	}
	if hasDraft && d.Year != ups[0].year {
		return fail("%s is for %d, but the import in progress is for %d. Discard it first to import another year.", ups[0].name, ups[0].year, d.Year)
	}

	d.Year = ups[0].year
	var names []string
	for _, u := range ups {
		switch u.kind {
		case koinly.KindHoldings:
			d.AsOf, d.Holdings = u.asOf, u.holdings
		case koinly.KindTransactions:
			d.Txns = u.txns
		case koinly.KindGains:
			d.Gains = u.gains
		case koinly.KindIncome:
			d.Income = u.income
		}
		if d.Files == nil {
			d.Files = map[string]string{}
		}
		d.Files[string(u.kind)] = u.name
		names = append(names, fmt.Sprintf("%s (%s)", u.name, kindName(u.kind)))
	}
	s.suggestMappings(ctx, &d, false)
	if err := s.DB.PutJSON(ctx, koinlyDraftKey, d); err != nil {
		return fail("couldn't save the import (%v).", err)
	}
	msg := fmt.Sprintf("Loaded %s for %d.", strings.Join(names, ", "), d.Year)
	if len(skipped) > 0 {
		msg += " Skipped " + strings.Join(skipped, ", ") + ": the other gains report isn't used."
	}
	return templates.ImportOutcome{OK: true, Message: msg}
}

// kindName is how the wizard names a report kind.
func kindName(k koinly.Kind) string {
	switch k {
	case koinly.KindHoldings:
		return "end of year holdings"
	case koinly.KindTransactions:
		return "transaction history"
	case koinly.KindGains:
		return "capital gains"
	case koinly.KindIncome:
		return "income"
	}
	return string(k)
}

// parseUpload reads and parses one Koinly report. Its year is the holdings report's date,
// or else the year in the report's title ("Transaction report 2025").
func parseUpload(fh *multipart.FileHeader, loc *time.Location) (upload, error) {
	u := upload{name: fh.Filename}
	if fh.Size > koinlyMaxFile {
		return u, fmt.Errorf("larger than %d MB", koinlyMaxFile>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return u, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, koinlyMaxFile+1))
	f.Close()
	switch {
	case err != nil:
		return u, err
	case len(raw) > koinlyMaxFile:
		return u, fmt.Errorf("larger than %d MB", koinlyMaxFile>>20)
	}
	if u.kind, err = koinly.Detect(raw); err != nil {
		return u, err
	}
	switch u.kind {
	case koinly.KindHoldings:
		var at time.Time
		if at, u.holdings, err = koinly.ParseHoldings(raw); err == nil {
			// The title's time is wall time in the tax timezone.
			u.asOf = time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), 0, 0, loc)
			u.year = at.Year()
		}
	case koinly.KindTransactions:
		u.txns, err = koinly.ParseTransactions(raw)
	case koinly.KindGains:
		u.gains, err = koinly.ParseGains(raw, loc)
	case koinly.KindIncome:
		u.income, err = koinly.ParseIncome(raw, loc)
	case koinly.KindOtherGains:
		return u, nil
	}
	if err != nil {
		return u, err
	}
	if u.year == 0 {
		if u.year = titleYear(raw); u.year == 0 {
			return u, errors.New("its title names no year")
		}
	}
	return u, nil
}

var reYear = regexp.MustCompile(`\b(19|20)\d\d\b`)

// titleYear is the last year named on a report's first line, or 0.
func titleYear(raw []byte) int {
	line, _, _ := strings.Cut(strings.TrimPrefix(string(raw[:min(len(raw), 256)]), "\ufeff"), "\n")
	all := reYear.FindAllString(line, -1)
	if len(all) == 0 {
		return 0
	}
	y, _ := strconv.Atoi(all[len(all)-1])
	return y
}

// suggestMappings keeps only the names the draft's reports contain and maps each new one:
// to what tax.yaml mapped it to on an earlier import, or else to what CryptKeeper matches it
// to. With rematch every name is new: earlier choices and imports are set aside.
func (s *Server) suggestMappings(ctx context.Context, d *koinlyDraft, rematch bool) {
	var saved tax.Settings
	if s.TaxSettings != nil && !rematch {
		saved = s.TaxSettings.Get()
	}
	if rematch {
		d.WalletMap, d.AssetMap, d.SkipOK = nil, nil, nil
	}
	walletNames := koinly.WalletNames(d.Txns, d.Gains)
	wm := make(map[string]string, len(walletNames))
	why := make(map[string]string, len(walletNames))
	var fresh []string
	for _, n := range walletNames {
		if v, ok := d.WalletMap[n]; ok {
			wm[n] = v
			if w := d.WalletWhy[n]; w != "" {
				why[n] = w
			}
		} else if v := saved.WalletMap[n]; v != "" && s.validWallet(v) {
			wm[n], why[n] = v, "As in your last import."
		} else {
			fresh = append(fresh, n)
		}
	}
	if len(fresh) > 0 {
		matched := koinly.MatchWallets(d.Txns, d.Gains, s.koinlyWallets())
		for _, n := range fresh {
			wm[n], why[n] = matched[n].ID, matched[n].Why
		}
	}
	d.WalletMap, d.WalletWhy = wm, why

	am := make(map[string]string, len(d.Holdings))
	var suggested map[string]string
	for _, h := range d.Holdings {
		if v, ok := d.AssetMap[h.Asset]; ok {
			am[h.Asset] = v
		} else if v, ok := saved.AssetMap[h.Asset]; ok {
			am[h.Asset] = v
		} else {
			if suggested == nil {
				st := s.koinlyState(ctx)
				suggested, _ = koinly.SuggestAssets(d.Holdings, s.knownAssets(st))
			}
			am[h.Asset] = suggested[h.Asset]
		}
	}
	d.AssetMap = am
}

// validWallet reports whether v can be a wallet mapping: untracked or a source ID.
func (s *Server) validWallet(v string) bool {
	if v == tax.Untracked {
		return true
	}
	_, ok := s.Registry.Get(v)
	return ok
}

// koinlyDerive is how many receive and change addresses of an HD wallet the matcher derives:
// Koinly names a Bitcoin wallet after one of them.
const koinlyDerive = 250

// koinlyWallets is every crypto source as the wallet matcher sees it: its configured address
// and, for an HD Bitcoin wallet, the first addresses its key derives (no network needed, so
// it works while the wallet can't sync), for Koinly's TxSrc/TxDest and wallet names to
// match.
func (s *Server) koinlyWallets() []koinly.Wallet {
	var out []koinly.Wallet
	for _, e := range s.Registry.Entries() {
		if e.Def.Type == sources.TypeBrokerage {
			continue
		}
		w := koinly.Wallet{ID: e.Def.ID, Label: e.Def.Label, Type: e.Def.Type}
		for _, a := range []string{e.Def.Address, e.Def.JupiterVault, e.Def.Key} {
			if a != "" {
				w.Addresses = append(w.Addresses, a)
			}
		}
		if d, ok := e.Source.(core.AddressDeriver); ok {
			w.Addresses = append(w.Addresses, d.DerivedAddresses(koinlyDerive)...)
		}
		out = append(out, w)
	}
	return out
}

// koinlyRematch maps every Koinly wallet and asset again from scratch, setting aside the
// choices made so far, and saves the draft.
func (s *Server) koinlyRematch(w http.ResponseWriter, r *http.Request) {
	var outcome templates.ImportOutcome
	s.draftMu.Lock()
	d, ok, err := s.loadDraft(r.Context())
	switch {
	case err != nil:
		outcome.Message = "Couldn't read the import in progress: " + err.Error()
	case !ok:
		outcome.Message = "Upload Koinly's reports first."
	default:
		s.suggestMappings(r.Context(), &d, true)
		if err := s.DB.PutJSON(r.Context(), koinlyDraftKey, d); err != nil {
			outcome.Message = "Couldn't save the mapping: " + err.Error()
		} else {
			outcome = templates.ImportOutcome{OK: true, Message: "Matched every wallet and asset again."}
		}
	}
	s.draftMu.Unlock()
	s.patchKoinly(w, r, templates.ImportOutcome{}, outcome)
}

// knownAssets is every asset CryptKeeper has seen, from the snapshot's rows and the activity
// legs, without spam, sorted by symbol. A snapshot row's name wins over an activity leg.
func (s *Server) knownAssets(st koinlyState) []koinly.AssetInfo {
	spam := spamSet(s.Cfg.Solana.HideMints)
	for k := range quietSet(st.snap, spam) {
		spam[k] = true // auto-hidden tokens are no mapping choice either
	}
	at := map[string]int{}
	var out []koinly.AssetInfo
	add := func(key, sym, name string) {
		if key == "" || spam[key] {
			return
		}
		if i, ok := at[key]; ok {
			out[i].Symbol, out[i].Name = cmp.Or(out[i].Symbol, sym), cmp.Or(out[i].Name, name)
			return
		}
		at[key] = len(out)
		out = append(out, koinly.AssetInfo{Key: key, Symbol: sym, Name: name})
	}
	for _, w := range st.snap.Wallets {
		for _, r := range w.Rows {
			add(r.AssetKey, r.Symbol, r.Name)
		}
	}
	for _, tx := range st.txs {
		for _, l := range tx.Legs {
			add(l.AssetKey, l.Symbol, "")
		}
		if tx.Fee != nil {
			add(tx.Fee.AssetKey, tx.Fee.Symbol, "")
		}
	}
	slices.SortStableFunc(out, func(a, b koinly.AssetInfo) int {
		return cmp.Or(strings.Compare(strings.ToUpper(a.Symbol), strings.ToUpper(b.Symbol)), strings.Compare(a.Key, b.Key))
	})
	return out
}

// koinlyMap saves the mapping form: w.<Koinly wallet> = source ID | untracked,
// a.<Koinly asset> = asset key | "" (skip), s.<Koinly asset> = 1 to skip an ambiguous
// asset anyway (a form that posts a.<asset> without it clears it), x.<asset key> = extra
// basis in USD (empty = none). Names the draft doesn't contain are ignored. A bad wallet
// or amount saves nothing.
func (s *Server) koinlyMap(w http.ResponseWriter, r *http.Request) {
	s.draftMu.Lock()
	outcome := s.koinlySaveMap(r)
	s.draftMu.Unlock()
	s.patchKoinly(w, r, templates.ImportOutcome{}, outcome)
}

// koinlySaveMap applies the mapping form to the draft. The caller holds s.draftMu.
func (s *Server) koinlySaveMap(r *http.Request) templates.ImportOutcome {
	fail := func(msg string) templates.ImportOutcome { return templates.ImportOutcome{Message: msg} }
	if err := parseBody(r); err != nil {
		return fail("Couldn't read the form: " + err.Error())
	}
	ctx := r.Context()
	d, ok, err := s.loadDraft(ctx)
	switch {
	case err != nil:
		return fail("Couldn't read the import in progress: " + err.Error())
	case !ok:
		return fail("Upload Koinly's reports first.")
	}
	walletNames := koinly.WalletNames(d.Txns, d.Gains)
	if d.WalletMap == nil {
		d.WalletMap = map[string]string{}
	}
	if d.AssetMap == nil {
		d.AssetMap = map[string]string{}
	}
	if d.ExtraBasis == nil {
		d.ExtraBasis = map[string]float64{}
	}
	if d.SkipOK == nil {
		d.SkipOK = map[string]bool{}
	}
	assets := map[string]bool{}
	for _, h := range d.Holdings {
		assets[h.Asset] = true
	}
	value := func(key string) string { return strings.TrimSpace(r.PostForm.Get(key)) }
	for key := range r.PostForm {
		switch {
		case strings.HasPrefix(key, "w."):
			name, v := strings.TrimPrefix(key, "w."), value(key)
			if !slices.Contains(walletNames, name) {
				continue
			}
			if v == "" {
				v = tax.Untracked
			}
			if !s.validWallet(v) {
				return fail(fmt.Sprintf("%q, chosen for the Koinly wallet %s, is not one of your wallets.", v, name))
			}
			if d.WalletMap[name] != v {
				delete(d.WalletWhy, name) // the user's choice now, not a match
			}
			d.WalletMap[name] = v
		case strings.HasPrefix(key, "a."):
			if name := strings.TrimPrefix(key, "a."); assets[name] {
				d.AssetMap[name] = value(key)
				d.SkipOK[name] = value("s."+name) == "1" // an unticked box is not posted
			}
		case strings.HasPrefix(key, "s."):
			if name := strings.TrimPrefix(key, "s."); assets[name] && value(key) == "1" {
				d.SkipOK[name] = true
			}
		}
	}
	for name, ok := range d.SkipOK {
		if !ok {
			delete(d.SkipOK, name)
		}
	}
	// Extra bases are by asset key: only keys some holding maps to (after the selects
	// above) take one.
	mapped := map[string]bool{}
	for _, k := range d.AssetMap {
		mapped[k] = k != ""
	}
	for key := range r.PostForm {
		k, ok := strings.CutPrefix(key, "x.")
		if !ok || !mapped[k] {
			continue
		}
		usd, err := parseUSD(value(key))
		if err != nil {
			return fail(fmt.Sprintf("Your cost for %s must be a dollar amount, like 1,250.00.", k))
		}
		if usd > 0 {
			d.ExtraBasis[k] = usd
		} else {
			delete(d.ExtraBasis, k)
		}
	}
	if err := s.DB.PutJSON(ctx, koinlyDraftKey, d); err != nil {
		return fail("Couldn't save the mapping: " + err.Error())
	}
	return templates.ImportOutcome{}
}

// parseUSD reads a dollar amount such as "$1,250.00"; empty is 0.
func parseUSD(v string) (float64, error) {
	v = strings.NewReplacer("$", "", ",", "", " ", "").Replace(v)
	if v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, errors.New("not a dollar amount")
	}
	return f, nil
}

func (s *Server) koinlyDiscard(w http.ResponseWriter, r *http.Request) {
	var outcome templates.ImportOutcome
	if s.DB != nil {
		s.draftMu.Lock()
		err := s.DB.DeleteKV(r.Context(), koinlyDraftKey)
		s.draftMu.Unlock()
		if err != nil {
			outcome.Message = "Couldn't discard the import: " + err.Error()
		} else {
			outcome = templates.ImportOutcome{OK: true, Message: "The import was discarded."}
		}
	}
	s.patchKoinly(w, r, outcome, templates.ImportOutcome{})
}

// koinlyAccept closes the draft's year: its closing lots are the reconciled opening lots,
// its disposals and income are Koinly's rows verbatim. The mappings go to tax.yaml, the
// draft is deleted and the browser goes back to Reports. Like a close, it holds closeMu, so
// it never races a close or reopen of the years (lock order: draftMu, then closeMu).
func (s *Server) koinlyAccept(w http.ResponseWriter, r *http.Request) {
	s.draftMu.Lock()
	s.closeMu.Lock()
	msg := s.koinlyClose(r.Context())
	s.closeMu.Unlock()
	s.draftMu.Unlock()
	if msg != "" {
		s.patchKoinly(w, r, templates.ImportOutcome{}, templates.ImportOutcome{Message: msg})
		return
	}
	s.invalidateTax()
	if s.Syncer != nil {
		s.Syncer.Kick() // the sync horizon now reaches back to the open year's start
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/reports")
}

// koinlyClose does the accept: it returns why it refused, or "" once the year is closed
// and the draft deleted. The year is saved first (a refused save writes nothing), then the
// mappings; should those fail, the year stays closed and the draft is kept so Accept can be
// retried (re-importing the latest closed year replaces it). The caller holds s.draftMu and
// s.closeMu.
func (s *Server) koinlyClose(ctx context.Context) string {
	d, ok, err := s.loadDraft(ctx)
	switch {
	case err != nil:
		return "Couldn't read the import in progress: " + err.Error()
	case !ok:
		return "Upload Koinly's reports first."
	}
	rev := s.koinlyReview(d, s.koinlyState(ctx))
	if rev.blocked != "" {
		return rev.blocked
	}
	op := rev.op
	y := tax.Year{
		Year: d.Year, Source: "koinly", ClosedAt: s.Now().UTC(), Timezone: s.taxLoc().String(),
		Summary: tax.Summarize(d.Gains, d.Income), Disposals: d.Gains, Income: d.Income,
		ClosingLots: op.Lots, ClosingParked: op.Parked,
	}
	if err := s.TaxYears.Save(y); err != nil {
		return fmt.Sprintf("Couldn't close %d: %v", d.Year, err)
	}
	names := koinly.WalletNames(d.Txns, d.Gains)
	err = s.TaxSettings.Update(func(t *tax.Settings) {
		if t.WalletMap == nil {
			t.WalletMap = map[string]string{}
		}
		if t.AssetMap == nil {
			t.AssetMap = map[string]string{}
		}
		for _, n := range names {
			if v, ok := d.WalletMap[n]; ok {
				t.WalletMap[n] = v
			}
		}
		for _, h := range d.Holdings {
			if v, ok := d.AssetMap[h.Asset]; ok {
				t.AssetMap[h.Asset] = v
			}
		}
	})
	if err != nil {
		s.invalidateTax() // the year is closed all the same
		return fmt.Sprintf("%d is closed, but the wallet and asset mappings couldn't be saved: %v. Accept again to save them.", d.Year, err)
	}
	if err := s.DB.DeleteKV(ctx, koinlyDraftKey); err != nil {
		s.Log.Warn("koinly import: delete draft", "err", err)
	}
	return ""
}

// koinlyReview is a draft's reconciliation and whether it can be accepted, shared by the
// wizard and Accept so they always agree.
type koinlyReview struct {
	known     []koinly.AssetInfo  // the assets CryptKeeper knows (the asset options)
	ambiguous map[string][]string // Koinly asset → the known keys its symbol matches
	op        koinly.Opening
	// problems: the wallet warnings, the closed year being replaced, the skipped assets
	// with a cost, then the opening's own problems.
	problems []string
	blocked  string // why Accept is refused; "" when it isn't
}

func (s *Server) koinlyReview(d koinlyDraft, st koinlyState) koinlyReview {
	rev := koinlyReview{known: s.knownAssets(st)}
	_, rev.ambiguous = koinly.SuggestAssets(d.Holdings, rev.known)
	if d.has(koinly.KindHoldings) {
		op, warnings := s.koinlyOpening(d, st)
		rev.op = op
		rev.problems = warnings
		if s.TaxYears != nil { // re-importing the latest closed year replaces it
			if last, ok := s.TaxYears.LastClosed(); ok && last.Year == d.Year {
				rev.problems = append(rev.problems, fmt.Sprintf("Accept replaces the closed %d year", d.Year))
			}
		}
		rev.problems = append(rev.problems, skippedProblems(d)...)
		rev.problems = append(rev.problems, op.Problems...)
	}
	rev.blocked = s.koinlyBlocked(d, st, rev)
	return rev
}

// skippedProblems names each holding mapped to Skip that has a cost or value: it won't
// carry over. (A holding with no mapping at all is BuildOpening's problem.)
func skippedProblems(d koinlyDraft) []string {
	type worth struct{ cost, value float64 }
	var order []string
	sum := map[string]*worth{}
	for _, h := range d.Holdings {
		if v, ok := d.AssetMap[h.Asset]; !ok || v != "" {
			continue
		}
		if sum[h.Asset] == nil {
			sum[h.Asset] = &worth{}
			order = append(order, h.Asset)
		}
		sum[h.Asset].cost += h.CostUSD
		sum[h.Asset].value += h.ValueUSD
	}
	var out []string
	for _, a := range order {
		switch w := sum[a]; {
		case w.cost > 0:
			out = append(out, fmt.Sprintf("%s: skipped; Koinly's %s cost won't carry over", a, templates.Fiat(w.cost)))
		case w.value > 0:
			out = append(out, fmt.Sprintf("%s: skipped; its %s year-end value won't carry over", a, templates.Fiat(w.value)))
		}
	}
	return out
}

// needsSkipConfirm reports whether holding h, left on Skip because its symbol matches
// several known assets, has a cost the user must not lose by accident.
func (rev koinlyReview) needsSkipConfirm(d koinlyDraft, h koinly.Holding) bool {
	v, mapped := d.AssetMap[h.Asset]
	return mapped && v == "" && len(rev.ambiguous[h.Asset]) > 0 && h.CostUSD > 0
}

// koinlyBlocked says why the draft can't be accepted yet, or "".
func (s *Server) koinlyBlocked(d koinlyDraft, st koinlyState, rev koinlyReview) string {
	switch {
	case !d.has(koinly.KindHoldings):
		return "Upload the end of year holdings report first: it has the quantities and costs the year closes with."
	case s.TaxYears == nil || s.TaxSettings == nil || s.DB == nil:
		return "Tax storage is not available."
	}
	if ys := s.TaxYears.Years(); len(ys) > 0 && ys[len(ys)-1] > d.Year {
		return fmt.Sprintf("%d is already closed; reopen it first", ys[len(ys)-1])
	}
	if s.Now().Before(d.cutover(s.taxLoc())) {
		return fmt.Sprintf("%d hasn't ended yet: import a year Koinly has closed.", d.Year)
	}
	switch err := s.TaxSettings.Err(); {
	case err != nil:
		return "data/tax.yaml has an error; fix it first: " + err.Error()
	case !st.ready:
		return "CryptKeeper is still loading your balances; try again in a minute."
	case st.txErr != nil:
		return "Couldn't read the activity history: " + st.txErr.Error()
	}
	cost := 0.0
	for _, h := range d.Holdings {
		if rev.needsSkipConfirm(d, h) && !d.SkipOK[h.Asset] {
			return fmt.Sprintf(`%s matches several assets: pick the right one under Assets, or tick "Skip anyway" to leave out its %s cost.`,
				h.Asset, templates.Fiat(h.CostUSD))
		}
		cost += h.CostUSD
	}
	if len(rev.op.Lots) == 0 && cost > costTolUSD {
		return fmt.Sprintf("Koinly's holdings cost %s, but nothing would carry over — map at least one asset.", templates.Fiat(cost))
	}
	return ""
}

// costTolUSD is the dollar amount below which a cost is float residue.
const costTolUSD = 0.005

// koinlyOpening reconciles the draft against CryptKeeper's wallets at the cutover: today's
// holdings (stake accounts and positions included) rolled back over the activity since,
// with tagged position moves keeping their units. warnings say which wallets' quantities
// may be off.
func (s *Server) koinlyOpening(d koinlyDraft, st koinlyState) (op koinly.Opening, warnings []string) {
	loc := s.taxLoc()
	cutover := d.cutover(loc)
	var settings tax.Settings
	if s.TaxSettings != nil {
		settings = s.TaxSettings.Get()
	}
	isPosition := func(id string) bool { return settings.Tags[id].Tag == tax.TagPosition }
	current, positions := map[string]map[string]float64{}, map[string]map[string]float64{}
	add := func(m map[string]map[string]float64, wallet, asset string, v float64) {
		if m[wallet] == nil {
			m[wallet] = map[string]float64{}
		}
		m[wallet][asset] += v
	}
	for _, wv := range st.snap.Wallets {
		for _, r := range wv.Rows {
			add(current, wv.ID, r.AssetKey, r.Amount)
			if r.Position != "" {
				add(positions, wv.ID, r.AssetKey, r.Amount)
			}
		}
		if wv.Status == portfolio.StatusError || wv.Status == portfolio.StatusPending {
			warnings = append(warnings, fmt.Sprintf("balances for %s haven't loaded; its quantity may be off", wv.Label))
		}
	}
	if !st.ready {
		warnings = append(warnings, "CryptKeeper hasn't loaded your balances yet, so every unit shows as untracked")
	}
	warnings = append(warnings, koinlyHistoryWarnings(s.Registry.Entries(), st.backfill, st.txs, cutover)...)
	op = koinly.BuildOpening(koinly.OpeningInput{
		Year: d.Year, Loc: loc, Holdings: d.Holdings, Txns: d.Txns,
		WalletMap: d.WalletMap, AssetMap: d.AssetMap, ExtraBasis: d.ExtraBasis,
		Balances: tax.RollBack(current, st.txs, cutover, isPosition),
		Parked:   tax.RollBackParked(positions, st.txs, cutover, isPosition),
	})
	return op, warnings
}

// koinlyHistoryWarnings names the activity sources whose stored history may not reach the
// cutover: the backfill isn't done, or the earliest stored entry is after the cutover.
// Their quantity at the cutover may be off.
func koinlyHistoryWarnings(entries []sources.Entry, backfillDone map[string]bool, txs []core.Tx, cutover time.Time) []string {
	earliest := map[string]time.Time{}
	for _, tx := range txs {
		if e, ok := earliest[tx.SourceID]; !ok || tx.Time.Before(e) {
			earliest[tx.SourceID] = tx.Time
		}
	}
	var out []string
	for _, e := range entries {
		if _, ok := e.Source.(core.ActivitySource); !ok {
			continue
		}
		first, has := earliest[e.Def.ID]
		if !backfillDone[e.Def.ID] || (has && first.After(cutover)) {
			out = append(out, fmt.Sprintf("activity history for %s doesn't reach %s; its quantity may be off",
				e.Def.Label, cutover.Format("2006-01-02")))
		}
	}
	return out
}

// ---- the wizard's view ----------------------------------------------------------------------

// koinlyWizard builds every step from the draft: the files loaded, the mapping tables, the
// reconciliation with its problems, and whether Accept is possible.
func (s *Server) koinlyWizard(ctx context.Context) templates.KoinlyWizard {
	var w templates.KoinlyWizard
	d, ok, err := s.loadDraft(ctx)
	if err != nil {
		w.Outcome.Message = "Couldn't read the import in progress: " + err.Error()
		return w
	}
	if !ok {
		return w
	}
	w.HasDraft, w.Year, w.AsOf = true, d.Year, d.AsOf
	for _, k := range koinlyKinds {
		if d.has(k) {
			w.Kinds, w.Files = append(w.Kinds, string(k)), append(w.Files, d.Files[string(k)])
		}
	}
	w.Holdings, w.Txns, w.Gains, w.Income = len(d.Holdings), len(d.Txns), len(d.Gains), len(d.Income)

	rev := s.koinlyReview(d, s.koinlyState(ctx))
	w.Wallets, w.WalletOptions = s.walletRows(d)
	w.Assets, w.AssetOptions = assetRows(d, rev)

	if d.has(koinly.KindHoldings) {
		op := rev.op
		symbols := map[string]string{}
		for _, h := range d.Holdings {
			if k := d.AssetMap[h.Asset]; k != "" && symbols[k] == "" {
				symbols[k] = h.Symbol
			}
		}
		for _, r := range op.Rows {
			w.Recon = append(w.Recon, templates.KoinlyReconRow{
				KoinlyAsset: r.KoinlyAsset, AssetKey: r.AssetKey, Symbol: symbols[r.AssetKey],
				KoinlyQty: r.KoinlyQty, KoinlyCost: r.KoinlyCost, CryptKeeperQty: r.CryptKeeperQty,
				Dated: r.Dated, Various: r.Various, Untracked: r.Untracked, MissingBasis: r.MissingBasis,
				ExtraBasis: d.ExtraBasis[r.AssetKey],
			})
		}
		w.Problems = rev.problems
	}
	w.Blocked = rev.blocked
	return w
}

// walletRows is one row per Koinly wallet name, and the options: every source as
// "<Label> (<type>)", then untracked. A mapped source that no longer exists stays
// selectable, marked removed.
func (s *Server) walletRows(d koinlyDraft) ([]templates.KoinlyMapRow, []templates.KoinlyOption) {
	var opts []templates.KoinlyOption
	have := map[string]bool{tax.Untracked: true}
	for _, def := range s.Registry.Defs() {
		if def.Type == sources.TypeBrokerage {
			continue // stocks aren't crypto tax records
		}
		opts = append(opts, templates.KoinlyOption{Value: def.ID, Label: templates.KoinlyWalletLabel(def.Label, def.Type)})
		have[def.ID] = true
	}
	var rows []templates.KoinlyMapRow
	for _, n := range koinly.WalletNames(d.Txns, d.Gains) {
		v := d.WalletMap[n]
		if v == "" {
			v = tax.Untracked
		}
		if !have[v] {
			opts = append(opts, templates.KoinlyOption{Value: v, Label: v + " (removed)"})
			have[v] = true
		}
		rows = append(rows, templates.KoinlyMapRow{Name: n, Selected: v, Note: d.WalletWhy[n]})
	}
	opts = append(opts, templates.KoinlyOption{Value: tax.Untracked, Label: "Untracked (no longer synced)"})
	return rows, opts
}

// assetRows is one row per Koinly holding (by its "SYM (Name)"), and the options: Skip,
// then every known asset, then any mapped key CryptKeeper doesn't hold now. An ambiguous
// holding with a cost left on Skip gets the "Skip anyway" box.
func assetRows(d koinlyDraft, rev koinlyReview) ([]templates.KoinlyMapRow, []templates.KoinlyOption) {
	opts := []templates.KoinlyOption{{Value: "", Label: "Skip"}}
	have := map[string]bool{"": true}
	for _, a := range rev.known {
		opts = append(opts, templates.KoinlyOption{Value: a.Key, Label: assetLabel(a)})
		have[a.Key] = true
	}
	var rows []templates.KoinlyMapRow
	seen := map[string]bool{}
	for _, h := range d.Holdings {
		if seen[h.Asset] {
			continue
		}
		seen[h.Asset] = true
		v := d.AssetMap[h.Asset]
		if !have[v] {
			opts = append(opts, templates.KoinlyOption{Value: v, Label: v + " (not in your wallets now)"})
			have[v] = true
		}
		row := templates.KoinlyMapRow{Name: h.Asset, Selected: v, Symbol: h.Symbol, Qty: h.Quantity, Cost: h.CostUSD}
		if len(rev.ambiguous[h.Asset]) > 0 && v == "" {
			row.Note = "Several assets use the symbol " + h.Symbol + ": choose the right one."
		}
		row.ConfirmSkip, row.SkipOK = rev.needsSkipConfirm(d, h), d.SkipOK[h.Asset]
		rows = append(rows, row)
	}
	return rows, opts
}

// assetLabel is an asset option's text: "BTC · Bitcoin", "JUP · Jupiter (spl:JUPyiw…DvCN)".
func assetLabel(a koinly.AssetInfo) string {
	l := cmp.Or(a.Symbol, a.Key)
	if a.Name != "" && !strings.EqualFold(a.Name, a.Symbol) {
		l += " · " + a.Name
	}
	if a.Key != a.Symbol {
		key := a.Key
		if mint, ok := strings.CutPrefix(key, "spl:"); ok {
			key = "spl:" + templates.ShortAddr(mint)
		}
		l += " (" + key + ")"
	}
	return l
}
