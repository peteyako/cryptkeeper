package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"cryptkeeper/internal/alerts"
	"cryptkeeper/internal/sources/manual"
	"cryptkeeper/web/templates"
)

var ruleTypeLabels = map[string]string{
	alerts.TypeBalanceDrop:   "Balance drop",
	alerts.TypePrice:         "Price",
	alerts.TypePortfolioMove: "Portfolio move",
	alerts.TypeSourceDown:    "Source down",
	alerts.TypeLargeActivity: "Large send",
}

// ruleSummary describes a rule in one line for the list.
func (s *Server) ruleSummary(r alerts.Rule) string {
	labels, _ := s.sourceLabels()
	switch r.Type {
	case alerts.TypeBalanceDrop:
		who := "every self-custody wallet"
		if len(r.Wallets) > 0 {
			var names []string
			for _, w := range r.Wallets {
				if l := labels[w]; l != "" {
					names = append(names, l)
				} else {
					names = append(names, w)
				}
			}
			who = strings.Join(names, ", ")
		}
		if r.MinPct > 0 {
			return fmt.Sprintf("Any asset in %s drops more than %s%%", who, trimFloat(r.MinPct))
		}
		return "Any decrease in " + who
	case alerts.TypePrice:
		var parts []string
		if r.Above > 0 {
			parts = append(parts, "above "+templates.Fiat(r.Above))
		}
		if r.Below > 0 {
			parts = append(parts, "below "+templates.Fiat(r.Below))
		}
		return r.Asset + " " + strings.Join(parts, " or ")
	case alerts.TypePortfolioMove:
		name := "All holdings"
		if r.Portfolio != "" && r.Portfolio != "all" && s.Portfolios != nil {
			if p, ok := s.Portfolios.Get(r.Portfolio); ok {
				name = p.Name
			} else {
				name = r.Portfolio
			}
		}
		return fmt.Sprintf("%s moves ±%s%% within %s", name, trimFloat(r.Pct), windowLabel(r.WindowDuration().String()))
	case alerts.TypeSourceDown:
		return fmt.Sprintf("A source is failing or stale for %d min", r.Minutes)
	case alerts.TypeLargeActivity:
		return fmt.Sprintf("A send of %s or more from a self-custody wallet", templates.Fiat(r.MinUsd))
	}
	return r.Type
}

func windowLabel(d string) string {
	switch d {
	case "1h0m0s":
		return "1 hour"
	case "24h0m0s":
		return "24 hours"
	case "168h0m0s":
		return "7 days"
	}
	return strings.TrimSuffix(strings.TrimSuffix(d, "0s"), "0m")
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (s *Server) alertsPage(r *http.Request) templates.AlertsPage {
	p := templates.AlertsPage{Shell: s.shell(r, "Alerts", "alerts", "all", ""), Redact: s.Cfg.Ntfy.RedactAmounts, Now: s.Now()}
	if u, err := url.Parse(s.Cfg.Ntfy.URL); err == nil && s.Cfg.Ntfy.URL != "" {
		p.NtfyHost = u.Host
	}
	if s.AlertRules != nil {
		for _, rule := range s.AlertRules.List() {
			name := rule.Name
			if name == "" {
				name = ruleTypeLabels[rule.Type]
			}
			p.Rules = append(p.Rules, templates.AlertRuleRow{ID: rule.ID, Name: name, Type: rule.Type, Summary: s.ruleSummary(rule), Enabled: rule.IsEnabled()})
		}
		if err := s.AlertRules.LoadError(); err != nil {
			p.LoadError = err.Error()
		}
	}
	if s.Alerts != nil {
		if recent, err := s.Alerts.Recent(r.Context(), 50); err == nil {
			for _, e := range recent {
				p.Recent = append(p.Recent, templates.AlertLogRow{Time: e.Time, RuleID: e.RuleID, Title: e.Title, Body: e.Body, Err: e.Err})
			}
		}
	}
	return p
}

func (s *Server) alertsFull(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, templates.AlertsFull(s.alertsPage(r)))
}

func (s *Server) alertForm(r *http.Request, editID string) (templates.AlertFormPage, bool) {
	p := templates.AlertFormPage{
		Values:          map[string]string{"type": alerts.TypeBalanceDrop, "cooldown": "6h", "enabled": "on", "window": "24h", "portfolio": "all"},
		SelectedWallets: map[string]bool{}, Errors: map[string]string{},
	}
	title := "New alert"
	if editID != "" {
		rule, ok := s.AlertRules.Get(editID)
		if !ok {
			return p, false
		}
		title = "Edit alert"
		p.EditID = rule.ID
		p.Values = ruleValues(rule)
		for _, w := range rule.Wallets {
			p.SelectedWallets[w] = true
		}
	}
	p.Shell = s.shell(r, title, "alerts", "all", "")
	p.WalletOptions = s.walletOptions(true)
	p.PortfolioOptions = s.portfolioNav()
	p.AssetOptions = s.assetOptions()
	return p, true
}

func ruleValues(r alerts.Rule) map[string]string {
	f := func(v float64) string {
		if v == 0 {
			return ""
		}
		return trimFloat(v)
	}
	v := map[string]string{
		"id": r.ID, "name": r.Name, "type": r.Type, "asset": r.Asset, "above": f(r.Above), "below": f(r.Below),
		"portfolio": r.Portfolio, "pct": f(r.Pct), "window": r.Window, "minUsd": f(r.MinUsd), "minPct": f(r.MinPct),
		"cooldown": r.Cooldown,
	}
	if r.Minutes > 0 {
		v["minutes"] = strconv.Itoa(r.Minutes)
	}
	if r.IsEnabled() {
		v["enabled"] = "on"
	}
	return v
}

func (s *Server) alertNew(w http.ResponseWriter, r *http.Request) {
	p, _ := s.alertForm(r, "")
	s.render(w, r, templates.AlertFormFull(p))
}

func (s *Server) alertEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.alertForm(r, chi.URLParam(r, "id"))
	if !ok {
		http.Redirect(w, r, "/alerts", http.StatusSeeOther)
		return
	}
	s.render(w, r, templates.AlertFormFull(p))
}

// parseRuleForm builds a rule from the posted form; only the chosen type's fields are kept
// (the rules parser rejects fields that belong to another type).
func parseRuleForm(r *http.Request, editID string) (alerts.Rule, map[string]string) {
	errs := map[string]string{}
	if err := parseBody(r); err != nil {
		errs[""] = "Could not read the form."
		return alerts.Rule{}, errs
	}
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	num := func(k string) float64 {
		v := strings.ReplaceAll(strings.TrimPrefix(f(k), "$"), ",", "")
		if v == "" {
			return 0
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			errs[k] = "Enter a positive number."
		}
		return n
	}
	rule := alerts.Rule{ID: editID, Name: f("name"), Type: f("type"), Cooldown: f("cooldown")}
	if f("enabled") == "" {
		disabled := false
		rule.Enabled = &disabled
	}
	switch rule.Type {
	case alerts.TypeBalanceDrop:
		rule.Wallets = r.PostForm["wallets"]
		rule.MinPct = num("minPct")
	case alerts.TypePrice:
		rule.Asset = manual.NormalizeAsset(f("asset"))
		rule.Above, rule.Below = num("above"), num("below")
		if rule.Asset == "" {
			errs["asset"] = "Choose an asset."
		}
		switch {
		case errs["above"] != "" || errs["below"] != "":
		case rule.Above == 0 && rule.Below == 0:
			errs["above"] = "Set a price to alert above or below."
		case rule.Above > 0 && rule.Below > 0:
			errs["above"] = "Set either above or below; create a second alert for the other direction."
		}
	case alerts.TypePortfolioMove:
		rule.Portfolio, rule.Window = f("portfolio"), f("window")
		rule.Pct = num("pct")
		if rule.Pct <= 0 && errs["pct"] == "" {
			errs["pct"] = "Enter the move in percent, e.g. 10."
		}
	case alerts.TypeSourceDown:
		m, err := strconv.Atoi(f("minutes"))
		if err != nil || m <= 0 {
			errs["minutes"] = "Enter minutes, e.g. 60."
		}
		rule.Minutes = m
	case alerts.TypeLargeActivity:
		rule.MinUsd = num("minUsd")
	default:
		errs["type"] = "Choose an alert type."
	}
	return rule, errs
}

func (s *Server) alertSave(w http.ResponseWriter, r *http.Request) {
	rule, errs := parseRuleForm(r, chi.URLParam(r, "id"))
	sse := datastar.NewSSE(w, r)
	if len(errs) == 0 {
		if _, err := s.AlertRules.Upsert(rule); err != nil {
			errs[""] = err.Error()
		} else {
			_ = sse.Redirect("/alerts")
			return
		}
	}
	_ = sse.PatchElementTempl(templates.FormErrors(errs))
}

func (s *Server) alertDelete(w http.ResponseWriter, r *http.Request) {
	_ = s.AlertRules.Delete(chi.URLParam(r, "id"))
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.AlertRules(s.alertsPage(r)))
}

func (s *Server) alertTest(w http.ResponseWriter, r *http.Request) {
	outcome := templates.ImportOutcome{OK: true, Message: "Test notification sent. Check your phone."}
	switch {
	case s.Alerts == nil || s.Cfg.Ntfy.URL == "":
		outcome = templates.ImportOutcome{Message: "ntfy isn't configured: add ntfy.url to config.yaml and restart."}
	default:
		if err := s.Alerts.Test(r.Context()); err != nil {
			outcome = templates.ImportOutcome{Message: "Sending failed: " + err.Error()}
		}
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.AlertTestResult(outcome))
	p := s.alertsPage(r)
	_ = sse.PatchElementTempl(templates.RecentAlerts(p))
}
