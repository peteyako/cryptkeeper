package demo

import (
	"time"

	"crypto-portfolio/web/templates"
)

// Alert rule ids used by the fixtures.
const (
	AlertColdDrop  = "cold-drop"
	AlertBTCAbove  = "btc-70k"
	AlertDayMove   = "day-move"
	AlertKrakenOff = "kraken-down"
	AlertBigSend   = "big-send"
)

// alertValues are the form values of each fixture rule, as the handler fills them from
// the stored rule (empty numbers are left out).
var alertValues = map[string]map[string]string{
	AlertColdDrop: {
		"id": AlertColdDrop, "name": "Cold storage watch", "type": "balance_drop", "minPct": "", "enabled": "on",
	},
	AlertBTCAbove: {
		"id": AlertBTCAbove, "name": "BTC above $70,000", "type": "price", "asset": "BTC", "above": "70000",
		"cooldown": "24h", "enabled": "on",
	},
	AlertDayMove: {
		"id": AlertDayMove, "name": "Big day", "type": "portfolio_move", "portfolio": "all", "pct": "10", "window": "24h",
		"cooldown": "6h", "enabled": "on",
	},
	AlertKrakenOff: {
		"id": AlertKrakenOff, "name": "Kraken down", "type": "source_down", "minutes": "60",
	},
	AlertBigSend: {
		"id": AlertBigSend, "name": "Large send", "type": "large_activity", "minUsd": "5000", "cooldown": "1h", "enabled": "on",
	},
}

// Alerts returns the Alerts page: one rule of every type (one of them paused), the ntfy
// server with amounts hidden, and a log with a delivery error.
func Alerts(now time.Time) templates.AlertsPage {
	return templates.AlertsPage{
		Shell: Shell("Alerts", "alerts", "all", ""),
		Rules: []templates.AlertRuleRow{
			{ID: AlertColdDrop, Name: "Cold storage watch", Type: "balance_drop", Summary: "Any decrease in Cold storage", Enabled: true},
			{ID: AlertBTCAbove, Name: "BTC above $70,000", Type: "price", Summary: "BTC above $70,000.00", Enabled: true},
			{ID: AlertDayMove, Name: "Big day", Type: "portfolio_move", Summary: "All holdings moves ±10% within 24 hours", Enabled: true},
			{ID: AlertKrakenOff, Name: "Kraken down", Type: "source_down", Summary: "A source is failing or stale for 60 min", Enabled: false},
			{ID: AlertBigSend, Name: "Large send", Type: "large_activity", Summary: "A send of $5,000.00 or more from a self-custody wallet", Enabled: true},
		},
		Recent: []templates.AlertLogRow{
			{
				Time: now.Add(-5 * time.Hour), RuleID: AlertBigSend, Title: "Large send from Coinbase",
				Body: "A send worth about $3,206.00 left Coinbase for bc1q8f…a7x4.",
				Err:  "ntfy returned 503 Service Unavailable. CryptKeep will not retry this one.",
			},
			{
				Time: now.Add(-2*24*time.Hour - 3*time.Hour), RuleID: AlertColdDrop, Title: "Cold storage balance dropped",
				Body: "Cold storage went from 0.32006114 BTC to 0.30006114 BTC (−0.02000000 BTC).",
			},
			{
				Time: now.Add(-9 * 24 * time.Hour), RuleID: AlertBTCAbove, Title: "BTC is above $60,000",
				Body: "Bitcoin is now $60,412.00, above your $60,000 alert.",
			},
			{
				Time: now.Add(-21 * 24 * time.Hour), RuleID: AlertDayMove, Title: "Big day: All holdings moved −10.4%",
				Body: "Down from $53,812.10 to $48,214.77 in the last 24 hours.",
			},
		},
		NtfyHost: "ntfy.example.org",
		Redact:   true,
		Now:      now,
	}
}

// AlertsEmpty is the Alerts page with no rules and ntfy not configured.
func AlertsEmpty(now time.Time) templates.AlertsPage {
	return templates.AlertsPage{Shell: Shell("Alerts", "alerts", "all", ""), Now: now}
}

// AlertsLoadError is the Alerts page when alerts.yaml has a bad hand edit.
func AlertsLoadError(now time.Time) templates.AlertsPage {
	p := Alerts(now)
	p.Rules, p.Recent = p.Rules[:2], nil
	p.LoadError = `rule "day-move": window "1 day" is not a valid duration (use e.g. 24h, or 168h for a week)`
	return p
}

// AlertForm returns the create form (editID "") or the edit form of a fixture rule. An
// unknown id gives the create form.
func AlertForm(editID string, now time.Time) templates.AlertFormPage {
	p := templates.AlertFormPage{
		Shell:            Shell("New alert", "alerts", "all", ""),
		Values:           map[string]string{"type": "balance_drop", "cooldown": "6h", "enabled": "on", "window": "24h", "portfolio": "all"},
		SelectedWallets:  map[string]bool{},
		Errors:           map[string]string{},
		PortfolioOptions: append([]templates.PortfolioNav(nil), Shell("", "", "", "").Portfolios...),
		AssetOptions:     assetOptions(now),
	}
	for _, w := range Wallets(now) {
		p.WalletOptions = append(p.WalletOptions, templates.WalletOption{ID: w.ID, Label: w.Label, Type: w.Type, Kind: w.Kind})
	}
	if v, ok := alertValues[editID]; ok {
		p.EditID = editID
		p.Shell.Title = "Edit alert"
		p.Values = map[string]string{}
		for k, val := range v {
			p.Values[k] = val
		}
		if editID == AlertColdDrop {
			p.SelectedWallets[IDCold] = true
		}
	}
	return p
}

// AlertFormInvalid is the create form for a price alert after a failed save.
func AlertFormInvalid(now time.Time) templates.AlertFormPage {
	p := AlertForm("", now)
	p.Values = map[string]string{"type": "price", "asset": "", "above": "", "cooldown": "6h", "enabled": "on"}
	p.Errors = map[string]string{
		"":      "Nothing was saved. Fix the fields below and try again.",
		"asset": "Choose an asset.",
		"above": "Set a price to alert above or below.",
	}
	return p
}
