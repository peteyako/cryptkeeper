package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/tax"
	"crypto-portfolio/web/templates"
)

// Closing freezes a filed year into data/tax/<year>.yaml; reopening deletes that file. Both
// ask twice: the first POST replies with a confirmation, the one with confirm=1 acts.

// openYear is the year a close would apply to: the year after the last closed one, or with
// none closed, earliest (the earliest year with activity; 0 = no activity, ok false).
func (s *Server) openYear(earliest int) (int, bool) {
	if s.TaxYears != nil {
		if last, ok := s.TaxYears.LastClosed(); ok {
			return last.Year + 1, true
		}
	}
	return earliest, earliest > 0
}

// closableYear is openYear with the earliest activity year read from the store.
func (s *Server) closableYear(ctx context.Context) (int, bool) {
	earliest := 0
	if s.Activity != nil {
		all, err := s.allActivity(ctx)
		if err != nil {
			return 0, false
		}
		for _, tx := range all {
			if y := tx.Time.In(s.taxLoc()).Year(); earliest == 0 || y < earliest {
				earliest = y
			}
		}
	}
	return s.openYear(earliest)
}

func (s *Server) yearEnded(y int) bool {
	return !s.Now().Before(time.Date(y+1, 1, 1, 0, 0, 0, 0, s.taxLoc()))
}

// idleCloseCard offers the close button once the open year (open, if known) has ended, and
// the reopen button while a year is closed.
func (s *Server) idleCloseCard(open int, known bool) templates.CloseCard {
	var c templates.CloseCard
	if s.TaxYears == nil {
		return c
	}
	if known && s.yearEnded(open) {
		c.CloseYear = open
	}
	if last, ok := s.TaxYears.LastClosed(); ok {
		c.ReopenYear = last.Year
	}
	return c
}

// patchCloseCard replaces only the close card.
func (s *Server) patchCloseCard(w http.ResponseWriter, r *http.Request, c templates.CloseCard) {
	_ = datastar.NewSSE(w, r).PatchElementTempl(templates.CloseCardView(c))
}

func (s *Server) taxCloseCancel(w http.ResponseWriter, r *http.Request) {
	open, ok := s.closableYear(r.Context())
	s.patchCloseCard(w, r, s.idleCloseCard(open, ok))
}

const noTaxData = "Tax data isn't available"

// outsideYear reports whether any frozen row falls outside [Jan 1 y, Jan 1 y+1) in loc.
func outsideYear(res tax.Result, y int, loc *time.Location) bool {
	from, to := time.Date(y, 1, 1, 0, 0, 0, 0, loc), time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)
	out := func(t time.Time) bool { return t.Before(from) || !t.Before(to) }
	for _, d := range res.Disposals {
		if out(d.Sold) {
			return true
		}
	}
	for _, d := range res.GiftsLost {
		if out(d.Sold) {
			return true
		}
	}
	for _, in := range res.Income {
		if out(in.Date) {
			return true
		}
	}
	return false
}

func (s *Server) taxClose(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	if s.TaxYears == nil || s.Activity == nil {
		s.patchReview(w, r, noTaxData)
		return
	}
	y, err := strconv.Atoi(r.PostFormValue("year"))
	if err != nil {
		s.patchReview(w, r, "Choose a year to close.")
		return
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	loc := s.taxLoc()
	last, hasLast := s.TaxYears.LastClosed()
	if hasLast && y <= last.Year {
		s.patchReview(w, r, fmt.Sprintf("%d is already closed", y))
		return
	}
	if !s.yearEnded(y) {
		s.patchReview(w, r, fmt.Sprintf("%d hasn't ended yet", y))
		return
	}
	open, ok := s.closableYear(r.Context())
	if !ok {
		s.patchReview(w, r, "There is no activity to close.")
		return
	}
	if y != open {
		s.patchReview(w, r, fmt.Sprintf("close %d first", open))
		return
	}
	in, ready, err := s.taxInput(r.Context())
	if err != nil {
		s.patchReview(w, r, "Activity couldn't be loaded.")
		return
	}
	if !ready {
		s.patchReview(w, r, "Still syncing; try again in a minute.")
		return
	}
	wantFrom := time.Time{}
	if hasLast {
		wantFrom = time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	}
	if !in.From.Equal(wantFrom) {
		s.patchReview(w, r, "The closed years changed while closing; reload and try again.")
		return
	}
	in.To = time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)
	res := tax.Compute(in)
	if outsideYear(res, y, loc) {
		s.patchReview(w, r, fmt.Sprintf("unexpected rows outside %d; nothing saved", y))
		return
	}
	issues := 0
	for _, is := range res.Issues {
		if is.Time.In(loc).Year() == y {
			issues++
		}
	}
	if r.PostFormValue("confirm") != "1" {
		msg := fmt.Sprintf("Close %d? %d review items are still open. Closed years can't be edited.", y, issues)
		if issues == 1 {
			msg = fmt.Sprintf("Close %d? 1 review item is still open. Closed years can't be edited.", y)
		}
		s.patchCloseCard(w, r, templates.CloseCard{Confirm: "close", Year: y, Msg: msg})
		return
	}
	err = s.TaxYears.Save(tax.Year{
		Year: y, Source: "cryptkeep", ClosedAt: s.Now().UTC(),
		Summary:   tax.Summarize(res.Disposals, res.Income),
		Disposals: res.Disposals, Income: res.Income, GiftsLost: res.GiftsLost,
		ClosingLots: res.Lots, ClosingParked: res.Parked, OpenIssues: issues,
		CarriedPeers: res.CarriedPeers,
	})
	if err != nil {
		s.patchReview(w, r, "Couldn't save: "+err.Error())
		return
	}
	s.invalidateTax()
	s.patchReview(w, r, "")
}

func (s *Server) taxReopen(w http.ResponseWriter, r *http.Request) {
	if !s.readForm(w, r) {
		return
	}
	if s.TaxYears == nil {
		s.patchReview(w, r, noTaxData)
		return
	}
	y, err := strconv.Atoi(r.PostFormValue("year"))
	if err != nil {
		s.patchReview(w, r, "Choose a year to reopen.")
		return
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	last, ok := s.TaxYears.LastClosed()
	if !ok {
		s.patchReview(w, r, fmt.Sprintf("%d isn't closed", y))
		return
	}
	if y != last.Year {
		s.patchReview(w, r, fmt.Sprintf("only %d can be reopened", last.Year))
		return
	}
	if r.PostFormValue("confirm") != "1" {
		msg := fmt.Sprintf("Reopen %d? Its figures will be recomputed from your activity and can change.", y)
		if last.Source == "koinly" {
			msg = fmt.Sprintf("Reopen %d? It was imported from Koinly; its figures will be recomputed by CryptKeep and can change.", y)
		}
		s.patchCloseCard(w, r, templates.CloseCard{Confirm: "reopen", Year: y, Msg: msg})
		return
	}
	if err := s.TaxYears.Delete(y); err != nil {
		s.patchReview(w, r, "Couldn't reopen: "+err.Error())
		return
	}
	s.invalidateTax()
	s.patchReview(w, r, "")
}
