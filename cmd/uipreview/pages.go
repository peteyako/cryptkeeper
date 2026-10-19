package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"cryptkeeper/internal/demo"
	"cryptkeeper/web/templates"
)

// ---- activity -----------------------------------------------------------------------------

// actSignals are the filter signals the Activity page sends with every list request.
type actSignals struct {
	Source string `json:"actSource"`
	Asset  string `json:"actAsset"`
	Kind   string `json:"actKind"`
}

// activityNow is the clock of the demo feed. The fixtures are dated relative to it and the
// "before" cursor is one of their times, so it stays put for the life of the process:
// with a moving clock every row would drift between a page and the next.
var activityNow = time.Now()

func activityRoutes(r chi.Router) {
	r.Get("/activity", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := demo.Activity(q.Get("source"), q.Get("asset"), q.Get("kind"), "", activityNow)
		if q.Get("empty") != "" {
			p = demo.ActivityEmpty(activityNow)
		}
		render(w, r, templates.ActivityFull(p))
	})
	// A filter change replaces the list; "before" appends the next page of rows and
	// replaces the load-more control.
	r.Get("/activity/list", func(w http.ResponseWriter, r *http.Request) {
		var f actSignals
		_ = datastar.ReadSignals(r, &f)
		before := r.URL.Query().Get("before")
		p := demo.Activity(f.Source, f.Asset, f.Kind, before, activityNow)
		sse := datastar.NewSSE(w, r)
		time.Sleep(450 * time.Millisecond)
		if before == "" {
			_ = sse.PatchElementTempl(templates.ActivityList(p))
			return
		}
		_ = sse.PatchElementTempl(templates.ActivityRows(p.Rows, p.Now), datastar.WithSelectorID("activity-rows"), datastar.WithModeAppend())
		_ = sse.PatchElementTempl(templates.ActivityMore(p))
	})
}

// ---- reports ------------------------------------------------------------------------------

func reportsPage(r *http.Request) templates.ReportsPage {
	if r.URL.Query().Get("empty") != "" {
		return demo.ReportsEmpty(time.Now())
	}
	return demo.Reports(time.Now())
}

func patchReports(sse *datastar.ServerSentEventGenerator, r *http.Request, mutate func(*templates.ReportsPage)) {
	p := reportsPage(r)
	if mutate != nil {
		mutate(&p)
	}
	_ = sse.PatchElementTempl(templates.PnLTable(p))
	_ = sse.PatchElementTempl(templates.CSVResultView(p.Import))
}

func reportsRoutes(r chi.Router) {
	r.Get("/reports", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.ReportsFull(reportsPage(r)))
	})
	// The Tax section: /reports/tax?year=&tab= (?empty=1 for a fresh install), the year
	// selector's POST, and the printable pages of a year. A Datastar request patches just
	// #tax-section, a browser load renders Reports with it.
	taxPage := func(year, tab string, empty bool) templates.TaxPage {
		y, _ := strconv.Atoi(year)
		return demo.TaxPage(y, tab, empty)
	}
	r.Get("/reports/tax", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tp := taxPage(q.Get("year"), q.Get("tab"), q.Get("empty") != "")
		if r.Header.Get("Datastar-Request") == "true" {
			_ = datastar.NewSSE(w, r).PatchElementTempl(templates.TaxSection(tp))
			return
		}
		p := reportsPage(r)
		p.Tax = tp
		render(w, r, templates.ReportsFull(p))
	})
	r.Post("/reports/tax/year", func(w http.ResponseWriter, r *http.Request) {
		_ = datastar.NewSSE(w, r).PatchElementTempl(templates.TaxSection(taxPage(r.PostFormValue("year"), r.PostFormValue("tab"), r.URL.Query().Get("empty") != "")))
	})
	taxYear := func(w http.ResponseWriter, r *http.Request) (int, bool) {
		y, err := strconv.Atoi(chi.URLParam(r, "year"))
		if err != nil || (y != 2025 && y != 2026) {
			http.NotFound(w, r)
			return 0, false
		}
		return y, true
	}
	r.Get("/reports/tax/{year}/8949", func(w http.ResponseWriter, r *http.Request) {
		if y, ok := taxYear(w, r); ok {
			rep := demo.YearReport(y)
			if r.URL.Query().Get("empty") != "" {
				rep = demo.TaxPage(y, "", true).Report
			}
			render(w, r, templates.Form8949Doc(rep))
		}
	})
	r.Get("/reports/tax/{year}/report", func(w http.ResponseWriter, r *http.Request) {
		if y, ok := taxYear(w, r); ok {
			v := demo.TaxReport(y)
			if r.URL.Query().Get("empty") != "" {
				v = templates.TaxReportView{Report: demo.TaxPage(y, "", true).Report, Generated: v.Generated, Method: v.Method}
			}
			render(w, r, templates.TaxReportPage(v))
		}
	})
	r.Post("/reports/import", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, hdr, err := r.FormFile("file")
		var out templates.ImportOutcome
		switch {
		case err != nil:
			out.Message = "Choose a CSV file to import."
		case strings.Contains(strings.ToLower(hdr.Filename), "bad"):
			out.Message = "Nothing was imported: line 1: this does not look like a " + r.FormValue("source") + " export."
		default:
			out = templates.ImportOutcome{OK: true, Message: "Imported 148 transactions (112 new), skipped 6 fiat or unsupported rows."}
		}
		sse := datastar.NewSSE(w, r)
		time.Sleep(700 * time.Millisecond)
		patchReports(sse, r, func(p *templates.ReportsPage) { p.Import = out })
	})
	for name, body := range map[string]string{
		"holdings": "asset,symbol,amount,price_usd,value_usd\nBTC,BTC,0.47114114,64120.00,30208.98\n",
		"history":  "time,value_usd\n2026-10-09T12:00:00Z,47900.00\n2026-10-16T12:00:00Z,48214.77\n",
		"pnl":      "asset,amount,avg_cost,basis,value,unrealized,unrealized_pct,missing_basis_units\nBTC,0.47114114,42800.00,17980.40,30208.98,12228.58,68.01,0.05\n",
		"activity": "time,source,kind,asset,amount,usd\n2026-10-16T11:28:00Z,Cold storage,transfer,BTC,0.05,3206.00\n",
	} {
		r.Get("/export/"+name+".csv", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
			_, _ = w.Write([]byte(body))
		})
	}

	// The Koinly import wizard: ?empty=1 before any upload, ?blocked=1 without the holdings
	// report. Its POSTs re-render the demo wizard; accept goes back to Reports.
	koinlyPage := func(r *http.Request) templates.KoinlyPage {
		switch q := r.URL.Query(); {
		case q.Get("empty") != "":
			return demo.KoinlyEmpty(time.Now())
		case q.Get("blocked") != "":
			return demo.KoinlyBlocked(time.Now())
		case q.Get("step") != "":
			step, _ := strconv.Atoi(q.Get("step"))
			return demo.KoinlyPage(step)
		}
		return demo.KoinlyPage(3)
	}
	r.Get("/reports/koinly", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.KoinlyFull(koinlyPage(r)))
	})
	for _, action := range []string{"upload", "map", "discard"} {
		r.Post("/reports/koinly/"+action, func(w http.ResponseWriter, r *http.Request) {
			p := koinlyPage(r)
			if action == "discard" {
				p = demo.KoinlyEmpty(time.Now())
			}
			_ = datastar.NewSSE(w, r).PatchElementTempl(templates.KoinlyWizardView(p.Wizard))
		})
	}
	r.Post("/reports/koinly/accept", func(w http.ResponseWriter, r *http.Request) {
		_ = datastar.NewSSE(w, r).Redirect("/reports")
	})
}

// ---- alerts -------------------------------------------------------------------------------

// alertState is what the preview remembers between Alerts requests: deleted fixture rules
// and rules created in the form.
var alertState = struct {
	sync.Mutex
	deleted map[string]bool
	added   []templates.AlertRuleRow
}{deleted: map[string]bool{}}

func alertsPage(r *http.Request) templates.AlertsPage {
	now := time.Now()
	q := r.URL.Query()
	alertState.Lock()
	defer alertState.Unlock()
	if q.Get("reset") != "" {
		alertState.deleted, alertState.added = map[string]bool{}, nil
	}
	switch {
	case q.Get("empty") != "":
		return demo.AlertsEmpty(now)
	case q.Get("error") != "":
		return demo.AlertsLoadError(now)
	}
	p := demo.Alerts(now)
	var rules []templates.AlertRuleRow
	for _, rule := range p.Rules {
		if !alertState.deleted[rule.ID] {
			rules = append(rules, rule)
		}
	}
	p.Rules = append(rules, alertState.added...)
	if q.Get("plain") != "" {
		p.Redact = false
	}
	return p
}

func alertSummary(typ string, f func(string) string) string {
	switch typ {
	case "balance_drop":
		return "Any decrease in every self-custody wallet"
	case "price":
		if f("above") != "" {
			return f("asset") + " above $" + f("above")
		}
		return f("asset") + " below $" + f("below")
	case "portfolio_move":
		return fmt.Sprintf("A portfolio moves ±%s%% within %s", f("pct"), f("window"))
	case "source_down":
		return "A source is failing or stale for " + f("minutes") + " min"
	}
	return "A send of $" + f("minUsd") + " or more from a self-custody wallet"
}

func alertsRoutes(r chi.Router) {
	r.Get("/alerts", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, templates.AlertsFull(alertsPage(r)))
	})
	r.Get("/alerts/new", func(w http.ResponseWriter, r *http.Request) {
		p := demo.AlertForm("", time.Now())
		if r.URL.Query().Get("invalid") != "" {
			p = demo.AlertFormInvalid(time.Now())
		}
		render(w, r, templates.AlertFormFull(p))
	})
	r.Get("/alerts/{id}/edit", func(w http.ResponseWriter, r *http.Request) {
		p := demo.AlertForm(chi.URLParam(r, "id"), time.Now())
		if p.EditID == "" {
			http.Redirect(w, r, "/alerts", http.StatusSeeOther)
			return
		}
		render(w, r, templates.AlertFormFull(p))
	})
	r.Post("/alerts/test", func(w http.ResponseWriter, r *http.Request) {
		sse := datastar.NewSSE(w, r)
		time.Sleep(800 * time.Millisecond)
		_ = sse.PatchElementTempl(templates.AlertTestResult(templates.ImportOutcome{OK: true, Message: "Test notification sent. Check your phone."}))
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
		errs := map[string]string{}
		switch f("type") {
		case "price":
			if f("asset") == "" {
				errs["asset"] = "Choose an asset."
			}
			if f("above") == "" && f("below") == "" {
				errs["above"] = "Set a price to alert above or below."
			}
		case "portfolio_move":
			if n, err := strconv.ParseFloat(f("pct"), 64); err != nil || n <= 0 {
				errs["pct"] = "Enter the move in percent, e.g. 10."
			}
		case "source_down":
			if n, err := strconv.Atoi(f("minutes")); err != nil || n <= 0 {
				errs["minutes"] = "Enter minutes, e.g. 60."
			}
		}
		sse := datastar.NewSSE(w, r)
		time.Sleep(400 * time.Millisecond)
		if len(errs) > 0 {
			errs[""] = "Nothing was saved. Fix the fields below and try again."
			_ = sse.PatchElementTempl(templates.FormErrors(errs))
			return
		}
		if id := chi.URLParam(r, "id"); id == "" {
			name := f("name")
			if name == "" {
				name = templates.AlertTypeLabel(f("type"))
			}
			alertState.Lock()
			alertState.added = append(alertState.added, templates.AlertRuleRow{
				ID: "new-" + strconv.Itoa(len(alertState.added)+1), Name: name, Type: f("type"),
				Summary: alertSummary(f("type"), f), Enabled: f("enabled") != "",
			})
			alertState.Unlock()
		}
		_ = sse.Redirect("/alerts")
	}
	r.Post("/alerts", save)
	r.Post("/alerts/{id}", save)
	r.Post("/alerts/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		alertState.Lock()
		alertState.deleted[id] = true
		var keep []templates.AlertRuleRow
		for _, a := range alertState.added {
			if a.ID != id {
				keep = append(keep, a)
			}
		}
		alertState.added = keep
		alertState.Unlock()
		sse := datastar.NewSSE(w, r)
		_ = sse.PatchElementTempl(templates.AlertRules(alertsPage(r)))
	})
}
