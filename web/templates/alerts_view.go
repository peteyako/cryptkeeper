package templates

import (
	"strings"
	"time"
)

// The display decisions of the Alerts screens.

// Alert rule types, as written in alerts.yaml.
const (
	alertBalanceDrop   = "balance_drop"
	alertPrice         = "price"
	alertPortfolioMove = "portfolio_move"
	alertSourceDown    = "source_down"
	alertLargeActivity = "large_activity"
)

// AlertTypeLabel is the short name of a rule type: "Balance drop", "Price", ...
func AlertTypeLabel(t string) string {
	switch t {
	case alertBalanceDrop:
		return "Balance drop"
	case alertPrice:
		return "Price"
	case alertPortfolioMove:
		return "Portfolio move"
	case alertSourceDown:
		return "Source down"
	case alertLargeActivity:
		return "Large send"
	}
	return t
}

// alertTypeOpts are the options of the Type select, in display order.
var alertTypeOpts = []selectOpt{
	{alertBalanceDrop, "Balance drop (cold-storage security)"},
	{alertPrice, "Price above or below"},
	{alertPortfolioMove, "Portfolio move"},
	{alertSourceDown, "Source down"},
	{alertLargeActivity, "Large send"},
}

var alertWindowOpts = []selectOpt{{"1h", "1 hour"}, {"24h", "24 hours"}, {"168h", "7 days"}}

var alertCooldownOpts = []selectOpt{{"1h", "1 hour"}, {"6h", "6 hours"}, {"24h", "24 hours"}}

func (p AlertFormPage) isEdit() bool { return p.EditID != "" }

func (p AlertFormPage) val(key string) string { return p.Values[key] }

// ruleType is the chosen type, balance drop for a new rule.
func (p AlertFormPage) ruleType() string {
	for _, o := range alertTypeOpts {
		if o.Value == p.Values["type"] {
			return o.Value
		}
	}
	return alertBalanceDrop
}

// enabled is the state of the Enabled checkbox: on for a new rule, else as stored. A rule
// that is switched off simply has no "enabled" value.
func (p AlertFormPage) enabled() bool {
	v := strings.ToLower(strings.TrimSpace(p.Values["enabled"]))
	if v == "" {
		return !p.isEdit()
	}
	switch v {
	case "false", "off", "0", "no":
		return false
	}
	return true
}

// cooldown is the stored cooldown, 6 hours for a new rule.
func (p AlertFormPage) cooldown() string {
	if v := p.Values["cooldown"]; v != "" {
		return v
	}
	return "6h"
}

// window is the stored portfolio-move window, 24 hours for a new rule.
func (p AlertFormPage) window() string {
	if v := p.Values["window"]; v != "" {
		return v
	}
	return "24h"
}

// portfolio is the stored portfolio-move target, all holdings for a new rule.
func (p AlertFormPage) portfolio() string {
	if v := p.Values["portfolio"]; v != "" {
		return v
	}
	return "all"
}

func (p AlertFormPage) portfolioOpts() []selectOpt {
	opts := make([]selectOpt, 0, len(p.PortfolioOptions)+1)
	has := false
	for _, o := range p.PortfolioOptions {
		opts = append(opts, selectOpt{o.ID, o.Name})
		has = has || o.ID == "all"
	}
	if !has {
		opts = append([]selectOpt{{"all", "All holdings"}}, opts...)
	}
	return withCurrentLabel(opts, p.portfolio(), p.portfolio())
}

func alertFormTitle(p AlertFormPage) string {
	if p.isEdit() {
		return "Edit alert"
	}
	return "New alert"
}

func alertFormLede(p AlertFormPage) string {
	if p.isEdit() {
		return "Change what this alert watches for. Vault only ever reads your balances, so alerts tell you; they never move funds."
	}
	return "Pick what to watch for and Vault sends you a notification when it happens."
}

func alertSaveLabel(p AlertFormPage) string {
	if p.isEdit() {
		return "Save alert"
	}
	return "Create alert"
}

// shows reports whether the chosen type is one of types, i.e. whether a group of fields is
// visible on first render (data-show takes over once the page is live).
func (p AlertFormPage) shows(types ...string) bool {
	for _, t := range types {
		if p.ruleType() == t {
			return true
		}
	}
	return false
}

// showExpr is the data-show expression for a group visible for any of the types.
func showExpr(types ...string) string {
	parts := make([]string, len(types))
	for i, t := range types {
		parts[i] = ruleTypeIs(t)
	}
	return strings.Join(parts, " || ")
}

// hideExpr is the data-attr:disabled expression of a group: the negation of showExpr.
func hideExpr(types ...string) string { return "!(" + showExpr(types...) + ")" }

// logWhen is "Oct 16, 11:42" for the recent alerts list.
func logWhen(t time.Time) string { return Local(t).Format("Jan 2, 15:04") }

// alertEnabledChip is the state chip of a rule.
func alertEnabledChip(on bool) (glyph, label, tone string) {
	if on {
		return "●", "On", "gain"
	}
	return "◌", "Paused", "neutral"
}

// ruleName is the name of a rule in the list: its name, else its type.
func ruleName(r AlertRuleRow) string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return AlertTypeLabel(r.Type)
}
