package server

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/web/templates"
)

// The "Needs review" actions: tag, bulk tag, price override and write-off. Each validates,
// saves data/tax.yaml in one SettingsStore.Update, drops the tax cache and patches the
// review panel and the P&L table. A refused action shows its reason at the top of the panel.

// patchReview re-renders the Tax section on its review tab (errMsg atop the panel) and the P&L table.
func (s *Server) patchReview(w http.ResponseWriter, r *http.Request, errMsg string) {
	r = r.WithContext(s.withActivity(r.Context()))
	p := s.reportsPage(r)
	p.Review.Err = errMsg
	sse := datastar.NewSSE(w, r)
	s.patchTaxSection(sse, r, &p)
	_ = sse.PatchElementTempl(templates.PnLTable(p))
}

// patchTaxSection re-renders the Tax section on its review tab with p's review panel. The
// section holds the review panel (its tab for the latest open year) and the tiles and counts
// a change just moved, so it is re-rendered whole. r must carry the activity (withActivity).
func (s *Server) patchTaxSection(sse *datastar.ServerSentEventGenerator, r *http.Request, p *templates.ReportsPage) {
	tp := s.taxPage(r.Context(), 0, "review", 1, &p.Review)
	_ = sse.PatchElementTempl(templates.TaxSection(tp))
	if !tp.ReviewTab {
		// Latest year closed: no review tab, but the refusal still goes to the panel.
		_ = sse.PatchElementTempl(templates.ReviewPanel(p.Review))
	}
}

// readForm parses the POST body; on failure it patches the refusal and reports false.
func (s *Server) readForm(w http.ResponseWriter, r *http.Request) bool {
	if err := parseBody(r); err != nil {
		s.patchReview(w, r, "That form could not be read.")
		return false
	}
	return true
}

// reviewSave runs fn as the one settings update. fn returns a refusal to show (it then must
// leave st unchanged), "" on success.
func (s *Server) reviewSave(w http.ResponseWriter, r *http.Request, fn func(*tax.Settings) string) {
	msg := ""
	if s.TaxSettings == nil {
		msg = "Tax settings are not available."
	} else if err := s.TaxSettings.Update(func(st *tax.Settings) { msg = fn(st) }); err != nil {
		msg = "Couldn't save: " + err.Error()
	} else {
		s.invalidateTax()
	}
	s.patchReview(w, r, msg)
}

// closedMsg is the refusal for a change dated at (""  when the year is open).
func (s *Server) closedMsg(at time.Time) string {
	if s.TaxYears == nil {
		return ""
	}
	last, ok := s.TaxYears.LastClosed()
	if !ok || !at.Before(tax.OpenYearStart(last, ok, s.taxLoc())) {
		return ""
	}
	return fmt.Sprintf("%d is closed; reopen it to change it", at.In(s.taxLoc()).Year())
}

// activityByID is every stored transaction by ID; ok is false without readable activity.
func (s *Server) activityByID(r *http.Request) (map[string]core.Tx, bool) {
	if s.Activity == nil {
		return nil, false
	}
	all, err := s.allActivity(r.Context())
	if err != nil {
		return nil, false
	}
	m := make(map[string]core.Tx, len(all))
	for _, tx := range all {
		m[tx.ID] = tx
	}
	return m, true
}

// tagRefusal is why tag may not be set on tx for asset ("" when it may); unpaired says tx is
// a transfer half with no usable peer (tax.Result.Unpaired). An empty tag removes the tag and
// is always fine (but a closed year still refuses it).
func (s *Server) tagRefusal(tx core.Tx, asset string, tag tax.Tag, current tax.Tag, unpaired bool) string {
	if m := s.closedMsg(tx.Time); m != "" {
		return m
	}
	if tag == "" {
		return ""
	}
	dir := assetDirection(tx, asset)
	if !slices.Contains(reviewTagOptions(tx, dir, current, unpaired), string(tag)) {
		what := map[string]string{"in": "receive", "out": "send"}[dir]
		if what == "" {
			what = "transaction"
		}
		return fmt.Sprintf("tag %s doesn't apply to a %s", tag, what)
	}
	return ""
}

func (s *Server) taxTag(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	id, tag, note := r.PostFormValue("tx"), tax.Tag(strings.TrimSpace(r.PostFormValue("tag"))), strings.TrimSpace(r.PostFormValue("note"))
	txs, ok := s.activityByID(r)
	tx, found := txs[id]
	if !ok || !found {
		s.patchReview(w, r, "That transaction was not found.")
		return
	}
	// The form names the item's asset; direction comes from that asset's leg. Without one,
	// the first leg stands for the tx.
	asset := r.PostFormValue("asset")
	if asset == "" && len(tx.Legs) > 0 {
		asset = tx.Legs[0].AssetKey
	} else if asset == "" && tx.Fee != nil {
		asset = tx.Fee.AssetKey
	}
	var current tax.Tag
	if s.TaxSettings != nil {
		current = s.TaxSettings.Get().Tags[id].Tag
	}
	unpaired := false
	if tx.PeerID != "" {
		if res, ok := s.taxResult(r.Context()); ok {
			_, unpaired = slices.BinarySearch(res.Unpaired, id)
		}
	}
	if m := s.tagRefusal(tx, asset, tag, current, unpaired); m != "" {
		s.patchReview(w, r, m)
		return
	}
	s.reviewSave(w, r, func(st *tax.Settings) string {
		if tag == "" {
			delete(st.Tags, id)
			return ""
		}
		if st.Tags == nil {
			st.Tags = map[string]tax.TagEntry{}
		}
		st.Tags[id] = tax.TagEntry{Tag: tag, Note: note}
		return ""
	})
}

// taxTagBulk tags every transaction in the (kind, asset) group, not just the ones shown.
func (s *Server) taxTagBulk(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	kind, asset, tag := tax.IssueKind(r.PostFormValue("kind")), r.PostFormValue("asset"), tax.Tag(strings.TrimSpace(r.PostFormValue("tag")))
	res, ok := s.taxResult(r.Context())
	txs, ok2 := s.activityByID(r)
	if !ok || !ok2 {
		s.patchReview(w, r, "Activity couldn't be loaded.")
		return
	}
	var current map[string]tax.TagEntry
	if s.TaxSettings != nil {
		current = s.TaxSettings.Get().Tags
	}
	var ids []string
	refused, firstMsg := 0, ""
	seen := map[string]bool{}
	for _, is := range res.Issues {
		if is.Kind != kind || is.Asset != asset || seen[is.TxID] {
			continue
		}
		tx, found := txs[is.TxID]
		if !found {
			continue
		}
		seen[is.TxID] = true
		_, unpaired := slices.BinarySearch(res.Unpaired, is.TxID)
		if m := s.tagRefusal(tx, asset, tag, current[is.TxID].Tag, unpaired); m != "" {
			if refused == 0 {
				firstMsg = m
			}
			refused++
			continue
		}
		ids = append(ids, is.TxID)
	}
	if refused > 0 {
		if strings.HasSuffix(firstMsg, "reopen it to change it") {
			s.patchReview(w, r, firstMsg)
		} else {
			s.patchReview(w, r, fmt.Sprintf("tag %s doesn't apply to %d of these transactions", tag, refused))
		}
		return
	}
	if len(ids) == 0 {
		s.patchReview(w, r, "")
		return
	}
	s.reviewSave(w, r, func(st *tax.Settings) string {
		if st.Tags == nil {
			st.Tags = map[string]tax.TagEntry{}
		}
		for _, id := range ids {
			if tag == "" {
				delete(st.Tags, id)
				continue
			}
			st.Tags[id] = tax.TagEntry{Tag: tag, Note: st.Tags[id].Note}
		}
		return ""
	})
}

// thousands matches a number whose commas are thousands separators (1,234.5).
var thousands = regexp.MustCompile(`^\d{1,3}(,\d{3})+(\.\d+)?$`)

// parseNumber reads a finite number; blank is not a number. Commas are accepted only as
// thousands separators, so "1,5" is refused rather than read as 15.
func parseNumber(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if thousands.MatchString(v) {
		v = strings.ReplaceAll(v, ",", "")
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

func (s *Server) taxPrice(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	key, raw := r.PostFormValue("key"), strings.TrimSpace(r.PostFormValue("usd"))
	var usd float64
	if raw != "" {
		f, ok := parseNumber(raw)
		if !ok || f < 0 {
			s.patchReview(w, r, "enter a number")
			return
		}
		usd = f
	}
	i := strings.LastIndex(key, "#")
	txs, loaded := s.activityByID(r)
	if !loaded {
		s.patchReview(w, r, "Activity couldn't be loaded.")
		return
	}
	tx, found := txs[key[:max(i, 0)]]
	// Removing an override is always fine for a stale key (its tx is gone); setting one
	// needs a known transaction.
	if raw != "" && (i <= 0 || !found) {
		s.patchReview(w, r, "unknown transaction")
		return
	}
	if found {
		if m := s.closedMsg(tx.Time); m != "" {
			s.patchReview(w, r, m)
			return
		}
	}
	s.reviewSave(w, r, func(st *tax.Settings) string {
		if raw == "" {
			delete(st.Prices, key)
			return ""
		}
		if st.Prices == nil {
			st.Prices = map[string]float64{}
		}
		st.Prices[key] = usd
		return ""
	})
}

func (s *Server) taxWriteOff(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	wo := tax.WriteOff{Wallet: r.PostFormValue("wallet"), Asset: r.PostFormValue("asset"), Note: strings.TrimSpace(r.PostFormValue("note"))}
	amount, ok := parseNumber(r.PostFormValue("amount"))
	if !ok || amount <= 0 {
		s.patchReview(w, r, "enter a number")
		return
	}
	wo.Amount = amount
	if raw := strings.TrimSpace(r.PostFormValue("proceeds")); raw != "" {
		f, ok := parseNumber(raw)
		if !ok || f < 0 {
			s.patchReview(w, r, "enter a number")
			return
		}
		wo.ProceedsUSD = f
	}
	loc := s.taxLoc()
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.PostFormValue("date")), loc)
	if err != nil {
		s.patchReview(w, r, "enter the date as YYYY-MM-DD")
		return
	}
	wo.Date = time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc)
	if wo.Wallet == "" || wo.Asset == "" {
		s.patchReview(w, r, "choose a wallet and an asset")
		return
	}
	if m := s.closedMsg(wo.Date); m != "" {
		s.patchReview(w, r, m)
		return
	}
	s.reviewSave(w, r, func(st *tax.Settings) string {
		st.WriteOffs = append(st.WriteOffs, wo)
		return ""
	})
}

// taxWriteOffDelete removes the write-off at index i. The form also names it (wallet, asset,
// date and amount, see writeOffAt); when the write-off now at i is another one (the list
// changed since the page was drawn), nothing is deleted.
func (s *Server) taxWriteOffDelete(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	i, err := strconv.Atoi(chi.URLParam(r, "i"))
	if err != nil {
		s.patchReview(w, r, "That write-off no longer exists.")
		return
	}
	wallet, asset, at, amount := r.PostFormValue("wallet"), r.PostFormValue("asset"), r.PostFormValue("at"), r.PostFormValue("amount")
	s.reviewSave(w, r, func(st *tax.Settings) string {
		if i < 0 || i >= len(st.WriteOffs) {
			return "That write-off no longer exists."
		}
		if wo := st.WriteOffs[i]; wo.Wallet != wallet || wo.Asset != asset || writeOffAt(wo) != at || writeOffAmount(wo) != amount {
			return "The write-offs changed; reload and try again."
		}
		if m := s.closedMsg(st.WriteOffs[i].Date); m != "" {
			return m
		}
		st.WriteOffs = slices.Delete(st.WriteOffs, i, i+1)
		return ""
	})
}

// taxTagDelete removes a tag whose transaction is gone.
func (s *Server) taxTagDelete(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(chi.URLParam(r, "tx"))
	if err != nil {
		s.patchReview(w, r, "That tag no longer exists.")
		return
	}
	s.reviewSave(w, r, func(st *tax.Settings) string {
		delete(st.Tags, id)
		return ""
	})
}
