package templates_test

import (
	"context"
	"encoding/json"
	"html"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/demo"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/web/static"
	"crypto-portfolio/web/templates"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type page struct {
	name string
	html string
	full bool // a whole document (has <html>) rather than a patch fragment
}

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func walletPage(t *testing.T, id, tab string) templates.WalletPage {
	t.Helper()
	p, ok := demo.Wallet(id, tab, now)
	if !ok {
		t.Fatalf("no demo wallet %q", id)
	}
	return p
}

// allPages renders every page and fragment with demo data in every interesting state.
func allPages(t *testing.T) []page {
	t.Helper()
	var out []page
	full := func(name string, c templ.Component) { out = append(out, page{name, render(t, c), true}) }
	frag := func(name string, c templ.Component) { out = append(out, page{name, render(t, c), false}) }

	ov := demo.Overview(now)
	full("overview", templates.OverviewFull(ov))
	full("overview empty", templates.OverviewFull(demo.OverviewEmpty(now)))
	lt, _ := demo.OverviewFor("long-term", "1M", now)
	full("overview long-term", templates.OverviewFull(lt))
	noHistory := ov
	noHistory.Chart, noHistory.Change = nil, portfolio.Change{}
	full("overview no history", templates.OverviewFull(noHistory))
	frag("dashboard", templates.Dashboard(ov))
	frag("value card", templates.ValueCard(ov))
	for _, st := range []portfolio.Status{portfolio.StatusSynced, portfolio.StatusStale, portfolio.StatusError, portfolio.StatusPending} {
		frag("sync chip "+string(st), templates.SyncChip(templates.SyncInfo{Status: st, Label: "label " + string(st)}))
	}

	wp := demo.WalletsPage(now)
	full("wallets", templates.WalletsFull(wp))
	empty := wp
	empty.Wallets = nil
	full("wallets empty", templates.WalletsFull(empty))

	for _, id := range []string{demo.IDCold, demo.IDCoinbase, demo.IDKraken, demo.IDPhantom, demo.IDMom} {
		for _, tab := range []string{"holdings", "activity", "addresses", "utxos"} {
			p := walletPage(t, id, tab)
			full("wallet "+id+" "+tab, templates.WalletFull(p))
			frag("wallet tab "+id+" "+tab, templates.WalletTab(p))
		}
	}

	full("source pick", templates.SourcePickFull(demo.SourcePick()))
	for _, typ := range []string{"coinbase", "kraken", "bitcoin", "solana", "manual"} {
		full("form add "+typ, templates.SourceFormFull(demo.SourceForm(typ)))
		full("form edit "+typ, templates.SourceFormFull(demo.SourceFormEdit(typ)))
		full("form invalid "+typ, templates.SourceFormFull(demo.SourceFormInvalid(typ)))
	}
	frag("test ok", templates.TestResultView(demo.TestOK(now)))
	frag("test failed", templates.TestResultView(demo.TestFailed()))
	frag("test empty", templates.TestResultView(templates.TestResult{}))
	frag("errors none", templates.FormErrors(nil))
	frag("errors some", templates.FormErrors(map[string]string{"": "Nothing was saved.", "label": "Give it a name.", "key": "Bad key."}))

	mj, _ := demo.OverviewFor(demo.PFMajors, "1W", now)
	full("overview majors", templates.OverviewFull(mj))

	pp := demo.Portfolios(now)
	full("portfolios", templates.PortfoliosFull(pp))
	pe := pp
	pe.Items, pe.Backups = nil, nil
	full("portfolios empty", templates.PortfoliosFull(pe))
	pw := pp
	pw.Items = append([]templates.PortfolioItem(nil), pp.Items...)
	pw.Items[1].Missing = []string{"old-ledger", "paper-2019"}
	pw.LoadError = "line 7: field walet not found in type portfolios.Portfolio"
	full("portfolios warnings", templates.PortfoliosFull(pw))
	pi := pp
	pi.Import = templates.ImportOutcome{OK: true, Message: "Imported: 2 added, 1 replaced, 3 unchanged."}
	frag("portfolio list", templates.PortfolioList(pp))
	frag("portfolio io", templates.PortfolioIO(pp))
	frag("portfolio io imported", templates.PortfolioIO(pi))
	frag("import ok", templates.ImportResultView(pi.Import))
	frag("import failed", templates.ImportResultView(templates.ImportOutcome{Message: "Couldn't read that file: line 3."}))
	frag("import empty", templates.ImportResultView(templates.ImportOutcome{}))

	full("portfolio form new", templates.PortfolioFormFull(demo.PortfolioForm("", now)))
	for _, id := range []string{demo.PFLongTerm, demo.PFTrading, demo.PFMajors} {
		full("portfolio form edit "+id, templates.PortfolioFormFull(demo.PortfolioForm(id, now)))
	}
	full("portfolio form invalid", templates.PortfolioFormFull(demo.PortfolioFormInvalid(now)))
	gone := demo.PortfolioForm(demo.PFLongTerm, now)
	gone.SelectedWallets["old-ledger"] = true
	full("portfolio form missing wallet", templates.PortfolioFormFull(gone))
	bare := demo.PortfolioForm("", now)
	bare.AllWallets, bare.AllAssets, bare.WalletOptions, bare.AssetOptions = false, false, nil, nil
	full("portfolio form no options", templates.PortfolioFormFull(bare))

	full("settings", templates.SettingsFull(demo.Settings(now)))

	// activity
	act := demo.Activity("", "", "", "", now)
	full("activity", templates.ActivityFull(act))
	older := demo.Activity("", "", "", act.More, now)
	frag("activity list", templates.ActivityList(act))
	frag("activity rows", templates.ActivityRows(older.Rows, now))
	frag("activity more", templates.ActivityMore(act))
	frag("activity more none", templates.ActivityMore(templates.ActivityPage{}))
	full("activity filtered", templates.ActivityFull(demo.Activity(demo.IDCoinbase, "BTC", "trade", "", now)))
	full("activity no match", templates.ActivityFull(demo.Activity(demo.IDCold, "ETH", "", "", now)))
	full("activity empty", templates.ActivityFull(demo.ActivityEmpty(now)))
	frag("sync card", templates.ActivitySyncCard(demo.SyncRows(now), now))
	frag("sync card empty", templates.ActivitySyncCard(nil, now))

	// reports
	rp := demo.Reports(now)
	full("reports", templates.ReportsFull(rp))
	full("reports empty", templates.ReportsFull(demo.ReportsEmpty(now)))
	rerr := rp
	rerr.TaxErr = "data/tax.yaml has an error and was not reloaded: line 3: bad indent"
	rerr.Import = templates.ImportOutcome{OK: true, Message: "Imported 148 transactions (112 new)."}
	full("reports messages", templates.ReportsFull(rerr))
	frag("pnl", templates.PnLTable(rp))
	frag("pnl empty", templates.PnLTable(demo.ReportsEmpty(now)))
	frag("csv import", templates.CSVImportPanel(rp))
	frag("csv ok", templates.CSVResultView(rerr.Import))
	frag("csv failed", templates.CSVResultView(templates.ImportOutcome{Message: "Nothing was imported: line 1: not a Kraken export."}))
	frag("csv empty", templates.CSVResultView(templates.ImportOutcome{}))

	// Koinly import
	kp := demo.Koinly(now)
	full("koinly", templates.KoinlyFull(kp))
	full("koinly empty", templates.KoinlyFull(demo.KoinlyEmpty(now)))
	full("koinly blocked", templates.KoinlyFull(demo.KoinlyBlocked(now)))
	frag("koinly wizard", templates.KoinlyWizardView(kp.Wizard))
	frag("koinly wizard empty", templates.KoinlyWizardView(templates.KoinlyWizard{}))
	rmb := rp
	rmb.MissingBasisSales = 3
	full("reports missing basis", templates.ReportsFull(rmb))

	// alerts
	ap := demo.Alerts(now)
	full("alerts", templates.AlertsFull(ap))
	full("alerts empty", templates.AlertsFull(demo.AlertsEmpty(now)))
	full("alerts load error", templates.AlertsFull(demo.AlertsLoadError(now)))
	aok := ap
	aok.Test = templates.ImportOutcome{OK: true, Message: "Test notification sent. Check your phone."}
	full("alerts tested", templates.AlertsFull(aok))
	frag("alert rules", templates.AlertRules(ap))
	frag("alert recent", templates.RecentAlerts(ap))
	frag("alert recent empty", templates.RecentAlerts(demo.AlertsEmpty(now)))
	frag("alert test ok", templates.AlertTestResult(aok.Test))
	frag("alert test failed", templates.AlertTestResult(templates.ImportOutcome{Message: "Sending failed: 503"}))
	frag("alert test empty", templates.AlertTestResult(templates.ImportOutcome{}))
	full("alert form new", templates.AlertFormFull(demo.AlertForm("", now)))
	for _, id := range []string{demo.AlertColdDrop, demo.AlertBTCAbove, demo.AlertDayMove, demo.AlertKrakenOff, demo.AlertBigSend} {
		full("alert form edit "+id, templates.AlertFormFull(demo.AlertForm(id, now)))
	}
	full("alert form invalid", templates.AlertFormFull(demo.AlertFormInvalid(now)))

	full("error", templates.ErrorFull(demo.Shell("Error", "overview", "all", ""), "That wallet doesn't exist."))
	return out
}

func has(t *testing.T, p page, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(p.html, s) {
			t.Errorf("%s: missing %q", p.name, s)
		}
	}
}

func lacks(t *testing.T, p page, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(p.html, s) {
			t.Errorf("%s: should not contain %q", p.name, s)
		}
	}
}

func byName(t *testing.T, pages []page, name string) page {
	t.Helper()
	for _, p := range pages {
		if p.name == name {
			return p
		}
	}
	t.Fatalf("no page %q", name)
	return page{}
}

func TestRequiredIDs(t *testing.T) {
	pages := allPages(t)
	want := map[string][]string{
		"overview":                {"dashboard", "value-card", "sync-status", "live"},
		"overview long-term":      {"dashboard", "value-card", "sync-status", "live"},
		"overview empty":          {"dashboard", "sync-status", "live"},
		"wallets":                 {"sync-status"},
		"wallet cold holdings":    {"wallet-tab"},
		"wallet cold utxos":       {"wallet-tab"},
		"wallet phantom activity": {"wallet-tab"},
		"form add coinbase":       {"source-form", "test-result", "form-errors"},
		"form edit bitcoin":       {"source-form", "test-result", "form-errors"},
		"form invalid manual":     {"source-form", "test-result", "form-errors"},

		"portfolios":                    {"portfolio-list", "portfolio-io", "import-form", "import-result"},
		"portfolios empty":              {"portfolio-list", "portfolio-io", "import-form", "import-result"},
		"portfolios warnings":           {"portfolio-list", "portfolio-io", "import-form", "import-result"},
		"portfolio form new":            {"portfolio-form", "form-errors", "f-name"},
		"portfolio form edit long-term": {"portfolio-form", "form-errors", "f-name"},
		"portfolio form invalid":        {"portfolio-form", "form-errors", "f-name-err", "f-wallets-err"},

		"activity":           {"activity-list", "activity-rows", "activity-more", "activity-sync"},
		"activity empty":     {"activity-list", "activity-sync"},
		"activity no match":  {"activity-list"},
		"reports":            {"pnl", "csv-import", "csv-form", "csv-result"},
		"reports empty":      {"pnl", "csv-import", "csv-form", "csv-result"},
		"alerts":             {"alert-rules", "alert-recent", "alert-test"},
		"alerts empty":       {"alert-rules", "alert-recent", "alert-test"},
		"alert form new":     {"alert-form", "form-errors", "f-type", "f-name"},
		"alert form invalid": {"alert-form", "form-errors", "f-asset-err", "f-above-err"},
		"settings":           {"activity-sync"},
		"koinly":             {"koinly-wizard", "koinly-upload", "f-koinly-files", "koinly-map", "koinly-recon", "koinly-accept", "kw-0", "ka-0", "kx-1"},
		"koinly empty":       {"koinly-wizard", "koinly-upload", "f-koinly-files"},
		"koinly blocked":     {"koinly-wizard", "koinly-map", "koinly-accept", "koinly-blocked"},
	}
	for name, ids := range want {
		p := byName(t, pages, name)
		for _, id := range ids {
			if !strings.Contains(p.html, `id="`+id+`"`) {
				t.Errorf("%s: missing id %q", name, id)
			}
		}
	}
	// /stream patches #dashboard, so only pages that have one may open it.
	for _, name := range []string{"wallets", "wallet cold holdings", "portfolios", "portfolio form new", "activity", "reports", "alerts", "alert form new", "koinly"} {
		lacks(t, byName(t, pages, name), `id="live"`)
	}

	// Patch fragments start with their root id, so a patch replaces the right element.
	roots := map[string]string{
		"dashboard": "dashboard", "value card": "value-card", "sync chip synced": "sync-status",
		"wallet tab cold utxos": "wallet-tab", "test ok": "test-result", "test empty": "test-result",
		"errors none": "form-errors", "errors some": "form-errors",
		"portfolio list": "portfolio-list", "portfolio io": "portfolio-io", "portfolio io imported": "portfolio-io",
		"import ok": "import-result", "import failed": "import-result", "import empty": "import-result",
		"activity list": "activity-list", "activity more": "activity-more", "activity more none": "activity-more",
		"sync card": "activity-sync", "sync card empty": "activity-sync",
		"pnl": "pnl", "pnl empty": "pnl",
		"csv import": "csv-import", "csv ok": "csv-result", "csv failed": "csv-result", "csv empty": "csv-result",
		"alert rules": "alert-rules", "alert recent": "alert-recent", "alert recent empty": "alert-recent",
		"alert test ok": "alert-test", "alert test failed": "alert-test", "alert test empty": "alert-test",
		"koinly wizard": "koinly-wizard", "koinly wizard empty": "koinly-wizard",
	}
	for name, id := range roots {
		p := byName(t, pages, name)
		if !regexp.MustCompile(`^<[a-z]+ id="` + id + `"`).MatchString(p.html) {
			t.Errorf("%s: root element must be #%s, starts with %.60q", name, id, p.html)
		}
	}
}

var (
	reOnDash      = regexp.MustCompile(`data-on-[a-z-]+`)
	reOnModDot    = regexp.MustCompile(`data-on:[a-z]+\.`)
	reIndicator   = regexp.MustCompile(`data-indicator="\$`)
	reAttrHidden  = regexp.MustCompile(`data-attr:hidden`)
	reComment     = regexp.MustCompile(`<!--`)
	reInlineOn    = regexp.MustCompile(`\son[a-z]+=`)
	reClassKey    = regexp.MustCompile(`data-class:[^=\s]*(--|__)`)
	reScript      = regexp.MustCompile(`<script`)
	reJSURL       = regexp.MustCompile(`(?i)javascript:`)
	reButton      = regexp.MustCompile(`(?s)<button\b([^>]*)>(.*?)</button>`)
	reAnchor      = regexp.MustCompile(`(?s)<a\b([^>]*)>(.*?)</a>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]*>`)
	reID          = regexp.MustCompile(`\sid="([^"]+)"`)
	reDescribedBy = regexp.MustCompile(`aria-describedby="([^"]+)"`)
	reInput       = regexp.MustCompile(`<(input|textarea|select)\b([^>]*)>`)
)

var datastarScript = `<script type="module" src="/static/datastar.js?v=` + static.Version + `"></script>`

// chartScript is the chart hover script, the only other script a page may carry.
var chartScript = `<script defer src="/static/chart.js?v=` + static.Version + `"></script>`

// TestSilentFailureGuards catches the Datastar mistakes that compile and render but do
// nothing in the browser, and a few other cheap structural rules.
func TestSilentFailureGuards(t *testing.T) {
	for _, p := range allPages(t) {
		t.Run(p.name, func(t *testing.T) {
			guards := []struct {
				re   *regexp.Regexp
				what string
			}{
				{reOnDash, "data-on-<event> (dash) never fires; use data-on:<event>"},
				{reOnModDot, "data-on:<event>.modifier; modifiers use a double underscore"},
				{reIndicator, `data-indicator="$name"; the name has no $`},
				{reAttrHidden, "data-attr:hidden; use data-show"},
				{reComment, "HTML comment; use templ // comments"},
				{reInlineOn, "inline on*= handler"},
				{reClassKey, "data-class:key with -- or __; use the object syntax"},
				{reJSURL, "javascript: URL"},
			}
			for _, g := range guards {
				for _, m := range g.re.FindAllString(p.html, -1) {
					// data-on-intersect is a Datastar plugin of its own (not an event
					// handler), as are the interval and signal-patch ones.
					if g.re == reOnDash && (m == "data-on-intersect" || m == "data-on-interval" || m == "data-on-signal-patch") {
						continue
					}
					t.Errorf("%s (found %q)", g.what, m)
					break
				}
			}

			// Scripts: exactly the Datastar module and the chart script on whole pages, none
			// in fragments.
			n := len(reScript.FindAllString(p.html, -1))
			switch {
			case p.full && (n != 2 || !strings.Contains(p.html, datastarScript) || !strings.Contains(p.html, chartScript)):
				t.Errorf("want exactly the Datastar module and chart scripts, found %d <script", n)
			case !p.full && n != 0:
				t.Errorf("fragment contains %d <script", n)
			}

			// Icon-only controls need an accessible name.
			for _, re := range []*regexp.Regexp{reButton, reAnchor} {
				for _, m := range re.FindAllStringSubmatch(p.html, -1) {
					text := strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(m[2], "")))
					if text == "" && !strings.Contains(m[1], "aria-label=") {
						t.Errorf("control without text or aria-label: %.120s", m[0])
					}
				}
			}

			// Ids are unique (patch targets), and every aria-describedby points at one.
			ids := map[string]bool{}
			for _, m := range reID.FindAllStringSubmatch(p.html, -1) {
				if ids[m[1]] {
					t.Errorf("duplicate id %q", m[1])
				}
				ids[m[1]] = true
			}
			if p.full {
				for _, m := range reDescribedBy.FindAllStringSubmatch(p.html, -1) {
					for _, id := range strings.Fields(m[1]) {
						if !ids[id] {
							t.Errorf("aria-describedby points at missing id %q", id)
						}
					}
				}
			}

			// Every visible form control has a <label for>.
			for _, m := range reInput.FindAllStringSubmatch(p.html, -1) {
				if strings.Contains(m[2], `type="hidden"`) {
					continue
				}
				idm := reID.FindStringSubmatch(m[2])
				if idm == nil {
					// A search box labelled by its sr-only label has an id too; anything
					// without one is unlabelled.
					t.Errorf("form control without id (so without a label): %.100s", m[0])
					continue
				}
				if !strings.Contains(p.html, `for="`+idm[1]+`"`) {
					t.Errorf("form control #%s has no <label for>", idm[1])
				}
			}
		})
	}
}

// TestPrivacyFigures: every figure carries both its value and its mask, so
// body.amounts-hidden can swap them in CSS.
func TestPrivacyFigures(t *testing.T) {
	good := regexp.MustCompile(`<span class="fig"><span class="fig-v">[^<]+</span><span class="fig-m" role="img" aria-label="Hidden amount">[^<]+</span></span>`)
	total := 0
	for _, p := range allPages(t) {
		figs := strings.Count(p.html, `class="fig"`)
		total += figs
		if v, m := strings.Count(p.html, `class="fig-v"`), strings.Count(p.html, `class="fig-m"`); v != figs || m != figs {
			t.Errorf("%s: %d .fig, %d .fig-v, %d .fig-m", p.name, figs, v, m)
		}
		if ok := len(good.FindAllString(p.html, -1)); ok != figs {
			t.Errorf("%s: %d of %d figures are not well formed", p.name, figs-ok, figs)
		}
	}
	if total < 100 {
		t.Errorf("suspiciously few figures rendered (%d)", total)
	}

	ov := byName(t, allPages(t), "overview")
	// Amounts are masked, prices and percentages are not.
	has(t, ov, `<span class="fig-v">$113,231.17</span>`, `$ ••••••`, `•••• BTC`, `$ ••••`)
	lacks(t, ov, `fig-v">$64,120.00`, `fig-v">+2.14%`)
}

func TestLayoutHead(t *testing.T) {
	p := byName(t, allPages(t), "overview")
	has(t, p,
		`<html lang="en">`,
		`<title id="page-title">Overview · CryptKeeper</title>`,
		`name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"`,
		`name="theme-color" content="`+templates.ThemeColorDark+`" media="(prefers-color-scheme: dark)"`,
		`name="theme-color" content="`+templates.ThemeColorLight+`" media="(prefers-color-scheme: light)"`,
		`href="/static/tokens.css?v=`+static.Version+`"`, `href="/static/app.css?v=`+static.Version+`"`,
		`rel="manifest" href="/manifest.webmanifest"`,
		`rel="apple-touch-icon" href="/static/icons/apple-touch-icon.png"`,
		`rel="icon" href="/static/icons/icon.svg"`,
		datastarScript,
	)
	if !regexp.MustCompile(`<meta charset="utf-8"`).MatchString(p.html) {
		t.Error("missing charset")
	}

	// The body carries the signals and the persistence wiring, in an order that works:
	// signals first, then the restore (data-init), then the effect that writes back.
	body := p.html[strings.Index(p.html, "<body"):]
	body = body[:strings.Index(body, ">")+1]
	idx := func(s string) int { return strings.Index(body, s) }
	if !(idx("data-signals=") > 0 && idx("data-signals=") < idx("data-class=") && idx("data-class=") < idx("data-init=") && idx("data-init=") < idx("data-effect=")) {
		t.Errorf("body attribute order must be signals, class, init, effect: %s", body)
	}
	un := html.UnescapeString(body)
	has(t, page{"body", un, false},
		`data-signals="{hide: false, theme: 'system', range: '1W', pf: 'all', q: '', showHidden: false, showPnl: false, cashOpen: false}"`,
		`data-class="{'amounts-hidden': $hide}"`,
		`$hide = (s.getItem('cryptkeeper.hide') || s.getItem('cryptkeep.hide')) === '1'`,
		`document.documentElement.dataset.theme`,
	)

	// The live stream element is hidden and uses data-init, never data-on:load.
	has(t, p, `<div id="live" hidden data-init="@get(&#39;/stream&#39;, {filterSignals: {include: /^(pf|range)$/}, requestCancellation: &#39;cleanup&#39;})"></div>`)
	lacks(t, p, "data-on:load")

	// Focus layout (the add-source flow) has no navigation.
	f := byName(t, allPages(t), "source pick")
	lacks(t, f, `class="sidebar"`, `class="bottomnav"`)
	has(t, f, `class="focus"`)
}

func TestSignalsEscapeServerValues(t *testing.T) {
	sh := demo.Shell("x", "overview", "a'b</script><b>", "1W")
	ov, _ := demo.OverviewFor("all", "1W", now)
	ov.Shell = sh
	out := render(t, templates.OverviewFull(ov))
	if strings.Count(out, "<script") != 2 || strings.Contains(out, "</script><b>") || strings.Contains(out, "<b>") {
		t.Errorf("hostile portfolio id broke out of the page")
	}
	if !strings.Contains(html.UnescapeString(out), `pf: 'a\'b\u003c/script\u003e\u003cb\u003e'`) {
		t.Errorf("portfolio id not escaped as a JS string literal")
	}
}

func TestHostileDataIsEscaped(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>'"&`
	p := walletPage(t, demo.IDCold, "holdings")
	p.W.ID = "x'y</script><i>"
	p.W.Label = evil
	p.W.Warnings = []string{evil}
	p.W.Err = evil
	p.Receive = "bc1q'" + evil
	p.W.Meta = append(p.W.Meta, core.KV{K: evil, V: evil})
	out := render(t, templates.WalletFull(p))
	for _, bad := range []string{"<img", "<i>", "</script><i>", "</script><b>", "onerror=alert(1)>"} {
		if strings.Contains(out, bad) {
			t.Errorf("unescaped %q in output", bad)
		}
	}
	if strings.Count(out, "<script") != 2 {
		t.Errorf("script injected")
	}
	un := html.UnescapeString(out)
	// The copy and remove expressions carry the hostile values as safe JS literals.
	if !strings.Contains(un, `navigator.clipboard.writeText('bc1q\'\u003cimg src=x onerror=alert(1)\u003e\'\"\u0026')`) {
		t.Errorf("copy expression not escaped")
	}
	if !strings.Contains(un, `@post('/sources/x%27y%3C%2Fscript%3E%3Ci%3E/delete')`) {
		t.Errorf("remove expression should use the path-escaped id; got:\n%s", grep(un, "/delete"))
	}
}

func grep(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, ">") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestOverviewContent(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "overview")
	has(t, p,
		"All holdings", "Total value", "past week", "Includes stale data",
		`aria-label="Chart range"`, `aria-label="Hide balances"`, `aria-label="Refresh balances"`,
		`href="/sources/new"`, "Add source",
		"Allocation", "by asset", "Sources", "Manage", "Holdings", "Watching",
		"Cold storage", "Hardware · Bitcoin", "Exchange · API", "Stale 3h", "1 asset · watch-only",
		`data-bind="q"`,
		`data-show="!$q || el.dataset.search.includes($q.toLowerCase())"`,
		`data-search="btc bitcoin cold storage coinbase kraken paper wallet at mom&#39;s"`,
		// A stale price renders with the tilde and the warn class.
		`<span class="stale" title="Price is stale (3h ago)">~$6.41</span>`,
		"Held in", "▲ +2.14%", "▼ −0.86%", "– 0.00%",
	)
	// Range buttons: Datastar wiring and the server-rendered selected state.
	un := html.UnescapeString(p.html)
	has(t, page{"overview (unescaped)", un, false},
		`data-on:click="$range = '1M'; @get('/chart', {filterSignals: {include: /^(pf|range)$/}})"`,
		`data-attr:aria-pressed="$range === '1M' ? 'true' : 'false'"`,
		`data-class="{'seg--on': $range === '1M'}"`,
		`data-on:click="@post('/refresh', {filterSignals: {include: /^(pf|range)$/}})"`,
		`data-indicator="refreshing"`, `data-attr:disabled="$refreshing"`,
		`data-on:click="$hide = !$hide"`, `data-class="{'is-pressed': $hide}"`,
		// Portfolio switching.
		`data-on:click__prevent="$pf = 'long-term'; history.replaceState(null, '', '/p/long-term'); @get('/view', {filterSignals: {include: /^(pf|range)$/}})"`,
		`data-class="{'nav-item--on': $pf === 'long-term'}"`,
		`href="/p/long-term"`,
	)
	if !regexp.MustCompile(`class="seg__btn seg--on" aria-pressed="true"`).MatchString(p.html) {
		t.Error("the selected range (1W) must be rendered pressed")
	}
	if got := strings.Count(p.html, `class="seg__btn seg--on"`); got != 1 {
		t.Errorf("want exactly one selected range, got %d", got)
	}
	lacks(t, p, "of all holdings") // net worth share only for a sub-portfolio

	lt := byName(t, pages, "overview long-term")
	has(t, lt, "Long-term", "of all holdings")

	e := byName(t, pages, "overview empty")
	has(t, e, "Nothing to show yet", `href="/sources/new"`, "CryptKeeper never needs trade or withdrawal permission.")
	lacks(t, e, `id="value-card"`, "Holdings</h2>")

	nh := byName(t, pages, "overview no history")
	has(t, nh, "Not enough history yet", "History fills in")

	// On pages other than the overview, portfolio links are plain links (nothing to patch).
	w := byName(t, pages, "wallets")
	lacks(t, w, "@get('/view'", "@get(&#39;/view&#39;")
	has(t, w, `href="/p/long-term"`)
}

func TestWalletContent(t *testing.T) {
	pages := allPages(t)
	for _, p := range pages {
		lacks(t, p, "Send with device")
	}

	cold := byName(t, pages, "wallet cold holdings")
	un := page{"cold (unescaped)", html.UnescapeString(cold.html), false}
	has(t, cold,
		"Cold storage", "Bitcoin · hardware", "Watch-only", "Synced · block 866,412",
		`href="/sources/cold/edit"`, "Edit", "Balance", "Cost basis", "Unrealized P/L",
		"0.30006114 BTC", "avg", "Receive address", `id="receive"`, `<div class="qr"><svg`, "bc1q8f…a7x4",
		"Verify this address on your device before sharing it.",
		"Connection", "Imported via", "xpub / zpub", "Addresses used", "7 of 27", "Last sync", "2m ago",
		"Remove wallet", `class="btn btn--danger btn--block"`,
		`class="tab tab--on"`, ">Addresses</a>", ">UTXOs</a>",
	)
	has(t, un,
		`navigator.clipboard && navigator.clipboard.writeText('bc1q8f3k7z2m9w5x6p4n8d0r3t7y2u6h4j9a7x4')`,
		`confirm('Remove this wallet? Its settings are deleted; on-chain funds are not affected.') && @post('/sources/cold/delete')`,
		`href="/w/cold?tab=addresses"`,
		`data-on:click__prevent="history.replaceState(null, '', '/w/cold?tab=addresses'); @get('/w/cold/tab/addresses')"`,
	)
	if !strings.Contains(cold.html, `aria-current="page"`) {
		t.Error("active tab should carry aria-current")
	}

	// No cost basis yet: the tiles show a dash and a link to the Koinly import.
	ph := byName(t, pages, "wallet phantom holdings")
	has(t, ph, "Import cost basis", `href="/reports/koinly"`, "Needs a cost basis", `stat__value stat__value--none`)
	lacks(t, ph, "Set cost basis")
	lacks(t, ph, "Receive address", `id="receive"`, ">Addresses</a>", ">UTXOs</a>", `href="#receive"`)
	has(t, ph,
		"Solana · hot wallet", "Staked", "Native stake",
		"Orca · SOL/USDC whirlpool", "Liquidity pool",
		"3 hidden tokens · 12 NFTs", `data-on:click="$showHidden = !$showHidden"`, `data-show="$showHidden" style="display:none"`,
		`data-attr:aria-expanded="$showHidden ? 'true' : 'false'"`,
		"12 NFTs are counted but not valued.",
	)

	kr := byName(t, pages, "wallet kraken holdings")
	has(t, kr, "Kraken last synced 3 hours ago. Values may be out of date.", "Stale 3h", "Read-only", "notice--warn")
	lacks(t, kr, "Verify this address")

	mom := byName(t, pages, "wallet mom-safe holdings")
	has(t, mom, "Hand-entered", "owed by Sam")

	act := byName(t, pages, "wallet phantom activity")
	lacks(t, act, "Coming soon")
	has(t, act, `class="tx-list" role="list"`, "Staking reward", "Swapped SOL → USDC")

	// Bitcoin-only tabs.
	addr := byName(t, pages, "wallet cold addresses")
	has(t, addr, "Transactions", "Unused", "Used", "Change", "bc1q8f…a7x4")
	utxo := byName(t, pages, "wallet cold utxos")
	has(t, utxo, "Pending", "Confirmed · 866,201", "3f9a…e5d6:0")

	// A wallet's error renders as a loss notice.
	p := walletPage(t, demo.IDKraken, "holdings")
	p.W.Err = "401 invalid key"
	out := render(t, templates.WalletFull(p))
	if !strings.Contains(out, `<div role="alert"><div class="notice notice--loss">`) || !strings.Contains(out, "401 invalid key") {
		t.Error("wallet error should render as a loss notice")
	}
	// In plain language, with the raw error behind Details.
	has(t, page{"wallet error", out, true}, `<p class="notice__line">Kraken rejected the API key.</p>`, "Query Funds and Query Ledger Entries",
		`<summary>Details</summary> <code>401 invalid key</code>`)

	// A filled-in cost basis shows the sign, arrow and percentage.
	cb := walletPage(t, demo.IDCold, "holdings")
	cost, pl := 20000.0, -760.08
	cb.CostBasis, cb.Unrealized = &cost, &pl
	out = render(t, templates.WalletFull(cb))
	for _, want := range []string{"$20,000.00", "▼ ", "−$760.08", "−3.80%"} {
		if !strings.Contains(out, want) {
			t.Errorf("unrealized P/L missing %q", want)
		}
	}
}

func TestWalletsAndSettings(t *testing.T) {
	pages := allPages(t)
	w := byName(t, pages, "wallets")
	has(t, w, "5 sources", "Wallets", "Paper wallet at mom&#39;s", `href="/w/mom-safe"`, `class="acct__rail rail--hardware"`, `class="acct__rail rail--exchange"`, `class="acct__rail rail--hot"`, `class="acct__rail rail--manual"`, "CryptKeeper is read-only")
	we := byName(t, pages, "wallets empty")
	has(t, we, "Nothing to show yet")

	s := byName(t, pages, "settings")
	un := page{"settings (unescaped)", html.UnescapeString(s.html), false}
	has(t, un,
		"Appearance", "System", "Light", "Dark", "Privacy", "Shown", "Hidden",
		`data-on:click="$theme = 'light'"`, `data-on:click="$theme = 'dark'"`, `data-on:click="$theme = 'system'"`,
		`data-attr:aria-pressed="$theme === 'dark' ? 'true' : 'false'"`,
		`data-on:click="$hide = true"`,
		"/data", "v0.1.0-dev", "Password required", `href="/sources/coinbase/edit"`, `aria-label="Edit Coinbase"`,
	)

	e := byName(t, pages, "error")
	has(t, e, "Something went wrong", "That wallet doesn&#39;t exist.", "Back to overview")
}

func TestSourceFlow(t *testing.T) {
	pages := allPages(t)

	pick := byName(t, pages, "source pick")
	has(t, pick, "Add a source", "Exchanges", "Self-custody",
		"Coinbase", "Kraken", "Read-only API key", "Bitcoin wallet", "xpub, descriptor or address",
		"Solana wallet", "Public address", "Manual holdings", "Coins held anywhere else",
		`href="/sources/new/coinbase"`, `href="/sources/new/kraken"`, `href="/sources/new/bitcoin"`,
		`href="/sources/new/solana"`, `href="/sources/new/manual"`,
		`aria-label="Step 1 of 2"`, "1/2", `aria-label="Back"`)

	for _, typ := range []string{"coinbase", "kraken", "bitcoin", "solana", "manual"} {
		add := byName(t, pages, "form add "+typ)
		un := page{"add " + typ, html.UnescapeString(add.html), false}
		has(t, un,
			`<form id="source-form" class="form" method="post" action="/sources"`,
			`data-indicator="saving"`,
			`data-on:submit="@post('/sources', {contentType: 'form'})"`,
			`<input type="hidden" name="type" value="`+typ+`">`,
			`data-indicator="testing"`, `data-attr:disabled="$testing || $saving"`,
			`data-on:click="@post('/sources/test', {contentType: 'form', selector: '#source-form'})"`,
			`type="button"`, "Test connection", "Save source", "Cancel", `aria-label="Step 2 of 2"`, "2/2",
			`id="test-result"`, `id="form-errors"`,
		)
		lacks(t, un, `name="editId"`)
		if typ == "bitcoin" != strings.Contains(add.html, `enctype="multipart/form-data"`) {
			t.Errorf("%s: multipart enctype only belongs on the bitcoin form", typ)
		}

		edit := byName(t, pages, "form edit "+typ)
		eun := page{"edit " + typ, html.UnescapeString(edit.html), false}
		has(t, eun, "Save changes", `name="editId"`, `data-on:submit="@post('/sources/`)
		lacks(t, eun, "Save source", "Step 2 of 2")

		inv := byName(t, pages, "form invalid "+typ)
		has(t, inv, `aria-invalid="true"`, `class="field__error"`, "Nothing was saved. Fix the fields below and try again.", "Give it a name")
	}

	cb := byName(t, pages, "form add coinbase")
	has(t, cb, "Connect Coinbase", "Create an API key in your Coinbase settings and paste it below.",
		"Grant <strong>view</strong> permissions only. CryptKeeper never needs trade or transfer access.",
		"API key name", "organizations/…/apiKeys/…", "Private key (PEM or Ed25519)", "<textarea", `autocomplete="off"`, `spellcheck="false"`,
		`value="Coinbase"`)
	kr := byName(t, pages, "form add kraken")
	has(t, kr, "Connect Kraken", "Enable <strong>Query Funds</strong> and <strong>Query Ledger Entries</strong> only.", `type="password"`, "API secret")
	btc := byName(t, pages, "form add bitcoin")
	has(t, btc, "Track a Bitcoin wallet", `enctype="multipart/form-data"`, `type="file"`, `name="walletFile"`, `accept=".json,.txt"`,
		"zpub6r… / wsh(sortedmulti(…)) / bc1q…", "Extended public key, output descriptor, or address",
		`<option value="hardware" selected>`, `<option value="hot">`, "Auto-detect",
		`<option value="p2wpkh">Native SegWit`, `<option value="p2sh-p2wpkh">Nested SegWit`, `<option value="p2tr">Taproot`, `<option value="p2pkh">Legacy`,
		"Watch-only. Never enter a seed phrase or private key here.")
	sol := byName(t, pages, "form add solana")
	has(t, sol, "Track a Solana wallet", `<option value="hot" selected>`, `name="address"`)
	man := byName(t, pages, "form add manual")
	has(t, man, "Add manual holdings", "One holding per line", "BTC 0.25 owed by Sam", `name="holdings"`)

	// Form-level and field errors reach the summary patch.
	fe := byName(t, pages, "errors some")
	has(t, fe, `role="alert"`, "Nothing was saved.", "<strong>Label:</strong> Give it a name.", "<strong>Public key:</strong> Bad key.")
	ne := byName(t, pages, "errors none")
	if strings.TrimSpace(ne.html) != `<div id="form-errors" class="form-errors"></div>` {
		t.Errorf("empty errors should render an empty container, got %q", ne.html)
	}

	ok := byName(t, pages, "test ok")
	has(t, ok, "notice--gain", "Connected. Found 4 balances.", "test-row", "Bitcoin")
	bad := byName(t, pages, "test failed")
	has(t, bad, "notice--loss", "Coinbase rejected the key (401).")
	zero := byName(t, pages, "test empty")
	if strings.TrimSpace(zero.html) != `<div id="test-result" class="test-result" aria-live="polite"></div>` {
		t.Errorf("empty test result should be an empty container, got %q", zero.html)
	}
}

// TestSecretsNeverPrefilled: secret inputs stay empty whatever the page is given, and an
// edit shows the stored mask in the placeholder instead.
func TestSecretsNeverPrefilled(t *testing.T) {
	const marker = "SECRETMARKER"
	for _, typ := range []string{"coinbase", "kraken"} {
		for _, edit := range []bool{false, true} {
			p := demo.SourceForm(typ)
			if edit {
				p = demo.SourceFormEdit(typ)
			}
			for _, k := range []string{"privateKey", "apiKey", "apiSecret"} {
				p.Values[k] = marker + "-" + k
			}
			p.Values["secret"] = marker + "-secret"
			p.Errors = map[string]string{"apiKey": "bad", "privateKey": "bad", "apiSecret": "bad"}
			out := render(t, templates.SourceFormFull(p))
			if strings.Contains(out, marker) {
				t.Errorf("%s edit=%v: secret value leaked into the page", typ, edit)
			}
			for _, m := range regexp.MustCompile(`value="([^"]*)"`).FindAllStringSubmatch(out, -1) {
				if strings.Contains(m[1], marker) {
					t.Errorf("%s: value attribute %q contains a secret", typ, m[1])
				}
			}
			// Secret inputs have no value attribute at all and the textarea has no content.
			for _, name := range []string{"privateKey", "apiKey", "apiSecret"} {
				re := regexp.MustCompile(`<(input|textarea)\b[^>]*name="` + name + `"[^>]*>`)
				tag := re.FindString(out)
				if tag == "" {
					continue
				}
				if strings.Contains(tag, "value=") {
					t.Errorf("%s: secret field %s has a value attribute: %s", typ, name, tag)
				}
			}
			if strings.Contains(out, `name="privateKey"`) && !regexp.MustCompile(`name="privateKey"[^>]*>\s*</textarea>`).MatchString(out) {
				t.Errorf("%s: privateKey textarea must be empty", typ)
			}
		}
	}

	co := demo.SourceFormEdit("coinbase")
	out := render(t, templates.SourceFormFull(co))
	if !strings.Contains(out, `placeholder="Leave blank to keep ••••3f9a"`) {
		t.Error("coinbase edit: placeholder should show the mask")
	}
	if regexp.MustCompile(`name="privateKey"[^>]*required`).MatchString(out) {
		t.Error("coinbase edit: a stored secret must not be required again")
	}
	add := render(t, templates.SourceFormFull(demo.SourceForm("coinbase")))
	if !regexp.MustCompile(`name="privateKey"[^>]*required`).MatchString(add) {
		t.Error("coinbase add: private key is required")
	}
	kr := render(t, templates.SourceFormFull(demo.SourceFormEdit("kraken")))
	for _, want := range []string{`placeholder="Leave blank to keep ••••b71c"`, `placeholder="Leave blank to keep ••••9d02"`} {
		if !strings.Contains(kr, want) {
			t.Errorf("kraken edit missing %q", want)
		}
	}
	// Non-secret fields are prefilled on edit.
	has(t, page{"coinbase edit", out, false}, `value="Coinbase"`, `value="organizations/9c1e…/apiKeys/3f9a"`)
	btc := render(t, templates.SourceFormFull(demo.SourceFormEdit("bitcoin")))
	if !strings.Contains(btc, "zpub6rFR7y4Q") || !strings.Contains(btc, `name="editId" value="cold"`) {
		t.Error("bitcoin edit: public key and edit id should be present")
	}
}

// TestDatastarExpressionsCompile pulls every Datastar attribute out of every rendered page
// and compiles it the way the bundle does (in Node, when installed).
func TestDatastarExpressionsCompile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	attr := regexp.MustCompile(`\s(data-[^\s=]+)="([^"]*)"`)
	returns := map[string]bool{"attr": true, "class": true, "show": true, "signals": true, "text": true, "style": true, "computed": true}
	plain := map[string]bool{"on": true, "effect": true, "init": true, "on-intersect": true}
	seen := map[string]bool{}
	var bodies []string
	kinds := map[string]int{}
	for _, p := range allPages(t) {
		for _, m := range attr.FindAllStringSubmatch(p.html, -1) {
			name := strings.TrimPrefix(m[1], "data-")
			kind := strings.FieldsFunc(name, func(r rune) bool { return r == ':' || r == '_' })[0]
			if !returns[kind] && !plain[kind] {
				continue
			}
			kinds[kind]++
			body := templates.DatastarBody(html.UnescapeString(m[2]), returns[kind])
			if !seen[body] {
				seen[body] = true
				bodies = append(bodies, body)
			}
		}
	}
	for _, k := range []string{"on", "attr", "class", "show", "signals", "init", "effect", "on-intersect"} {
		if kinds[k] == 0 {
			t.Errorf("no data-%s attributes found; the extraction is broken", k)
		}
	}
	js, _ := json.Marshal(bodies)
	script := `for (const b of JSON.parse(process.argv[1])) { try { new Function("el", "$", "__action", "evt", b) } catch (e) { console.log(e.message + "\n  " + b) } }`
	out, err := exec.Command(node, "-e", script, string(js)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("expressions that do not compile:\n%s", s)
	}
	if len(bodies) < 40 {
		t.Errorf("only %d distinct expressions checked", len(bodies))
	}
	sorted := append([]string(nil), bodies...)
	sort.Strings(sorted)
	t.Logf("compiled %d distinct Datastar expressions (%v)", len(sorted), kinds)
}

func TestLogosAreOptInAndHTTPSOnly(t *testing.T) {
	ov := demo.Overview(now)
	ov.Snap.Assets[0].LogoURL = "https://img.example/btc.png"
	ov.Snap.Assets[1].LogoURL = "http://insecure.example/eth.png"
	ov.Snap.Assets[2].LogoURL = "javascript:alert(1)"

	if off := render(t, templates.OverviewFull(ov)); strings.Contains(off, "<img") {
		t.Error("logos must be off unless Shell.ShowLogos is set")
	}
	ov.ShowLogos = true
	on := render(t, templates.OverviewFull(ov))
	if n := strings.Count(on, "<img"); n != 1 {
		t.Errorf("want exactly the https logo, got %d <img", n)
	}
	if !strings.Contains(on, `<img class="disc disc--logo" src="https://img.example/btc.png" alt=""`) || !strings.Contains(on, `referrerpolicy="no-referrer"`) {
		t.Error("https logo missing or without a no-referrer policy")
	}
	if strings.Contains(on, "insecure.example") || strings.Contains(on, "javascript:") {
		t.Error("non-https logo URLs must be dropped")
	}
}

// ---- portfolios -------------------------------------------------------------------------

// tagOf returns the opening tag of the first element whose attributes contain marker.
func tagOf(t *testing.T, html, marker string) string {
	t.Helper()
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("no element with %q", marker)
	}
	start := strings.LastIndex(html[:i], "<")
	end := strings.Index(html[i:], ">") + i
	return html[start : end+1]
}

func TestPortfolioManager(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "portfolios")
	un := page{"portfolios (unescaped)", html.UnescapeString(p.html), false}
	has(t, p,
		`<title id="page-title">Portfolios · CryptKeeper</title>`, `<h1 class="h-title">Portfolios</h1>`,
		`href="/portfolios/new"`, "New portfolio", `aria-label="Hide balances"`,
		"Long-term", "Trading", "Majors", "5 wallets", "2 wallets", "All wallets", "All assets", "BTC, ETH, SOL +2",
		"2 assets", "3 assets", "None", "76% of net worth", "24% of net worth",
		`href="/p/long-term"`, `href="/portfolios/long-term/edit"`, `href="/portfolios/long-term/export" download`,
		`aria-label="Open Long-term"`, `aria-label="Edit Long-term"`, `aria-label="Export Long-term"`, `aria-label="Delete Long-term"`,
	)
	has(t, un,
		`data-on:click="confirm('Delete the portfolio Long-term? Your wallets are not affected.') && @post('/portfolios/long-term/delete')"`,
		`data-on:click="confirm('Restore this backup? The current list is backed up first.') && @post('/portfolios/restore/20261016-114205.yaml')"`,
	)
	// Export is a plain download link, never a Datastar action.
	for _, m := range regexp.MustCompile(`<a [^>]*download[^>]*>`).FindAllString(p.html, -1) {
		if strings.Contains(m, "data-on") {
			t.Errorf("export link must be a plain anchor: %s", m)
		}
	}
	if n := strings.Count(p.html, " download"); n != 4 {
		t.Errorf("want 3 per-portfolio export links and Export all, got %d download attributes", n)
	}
	// Values are private figures; shares are not.
	has(t, p, `<span class="fig-v">$86,338.23</span>`, `$ ••••••`)

	// The sidebar reads "Portfolios" as the current section, and no main item is current.
	has(t, p, `class="nav-head nav-head--on"`, `class="nav-manage" href="/portfolios" data-on:click=`)
	if n := strings.Count(p.html, `aria-current="page"`); n != 1 {
		t.Errorf("only the Manage link should be current on /portfolios, got %d aria-current", n)
	}
	lacks(t, p, "nav-item--on", "bn-item--on", "@get(&#39;/view", "@get(&#39;/stream")

	// Save and restore.
	has(t, p,
		"Save &amp; restore", "Export all", `href="/portfolios/export" download`,
		`<form id="import-form" class="form" method="post" action="/portfolios/import" enctype="multipart/form-data"`,
		`data-indicator="importing"`, `data-attr:disabled="$importing"`,
		`name="file" type="file" accept=".yaml,.yml,text/yaml" required`,
		`name="mode" value="merge" checked`, `name="mode" value="replace"`,
		"Add new portfolios and update ones with the same id", "Your file becomes the whole list",
		"Backups", "4 backups", "Oct 16, 11:35", "25m ago · 1.3 KB", "3h ago · 1.2 KB", "980 B", `datetime="2026-10-16T11:35:00Z"`,
		`aria-label="Restore the backup from Oct 16, 11:35"`,
	)
	has(t, un, `data-on:submit="@post('/portfolios/import', {contentType: 'form'})"`)
	lacks(t, p, "No backups yet", "No portfolios yet")
	if strings.Contains(tagOf(t, p.html, `value="replace"`), "checked") {
		t.Error("Merge is the default import mode, not Replace")
	}
	if !strings.Contains(tagOf(t, p.html, `id="import-form"`), "data-on:submit") {
		t.Error("import form must post through Datastar")
	}

	// Empty: friendly copy and a primary action; the save and restore panel is still there.
	e := byName(t, pages, "portfolios empty")
	has(t, e, "No portfolios yet", `<a class="btn btn--primary" href="/portfolios/new">`, "No backups yet — one is made before every change.", `id="import-form"`)
	lacks(t, e, "pf-item", `class="backup-row"`)

	// Warnings: a hand-edit error and wallets that no longer exist.
	w := byName(t, pages, "portfolios warnings")
	has(t, w, `role="alert"`, "notice--warn", "CryptKeeper can't read portfolios.yaml.", "line 7: field walet not found in type portfolios.Portfolio",
		"Fix the file, or restore a backup.", "2 wallets in this portfolio no longer exist: ", `<span class="mono">old-ledger, paper-2019</span>`, "Edit the portfolio to clear it.")
	lacks(t, e, "notice--warn")
	// With an unreadable file and nothing parsed, the empty state would be misleading.
	bad := demo.Portfolios(now)
	bad.Items, bad.LoadError = nil, "line 2: boom"
	out := render(t, templates.PortfolioList(bad))
	if strings.Contains(out, "No portfolios yet") || !strings.Contains(out, "line 2: boom") {
		t.Errorf("a load error replaces the empty state: %s", out)
	}
	one := demo.Portfolios(now)
	one.Items = append([]templates.PortfolioItem(nil), one.Items[:1]...)
	one.Items[0].Missing = []string{"gone"}
	if out := render(t, templates.PortfolioList(one)); !strings.Contains(out, "1 wallet in this portfolio no longer exists: ") {
		t.Error("singular missing-wallet copy")
	}

	// Fragments.
	ok := byName(t, pages, "import ok")
	has(t, ok, "notice--gain", "Imported: 2 added, 1 replaced, 3 unchanged.", `aria-live="polite"`)
	fail := byName(t, pages, "import failed")
	has(t, fail, "notice--loss", "Couldn&#39;t read that file: line 3.")
	if got := strings.TrimSpace(byName(t, pages, "import empty").html); got != `<div id="import-result" class="import-result" aria-live="polite"></div>` {
		t.Errorf("empty import result should be an empty container, got %q", got)
	}
	// The IO panel can carry the result itself, so one patch refreshes backups and message.
	has(t, byName(t, pages, "portfolio io imported"), "Imported: 2 added, 1 replaced, 3 unchanged.")
	lacks(t, byName(t, pages, "portfolio io"), "notice--gain", "notice--loss")
}

func TestPortfolioFormPage(t *testing.T) {
	pages := allPages(t)

	n := byName(t, pages, "portfolio form new")
	un := page{"new (unescaped)", html.UnescapeString(n.html), false}
	has(t, un,
		`<title id="page-title">New portfolio · CryptKeeper</title>`, "New portfolio", "Group some of your holdings under a name.",
		`<form id="portfolio-form" class="form card pf-form" method="post" action="/portfolios"`,
		`data-indicator="saving"`, `data-on:submit="@post('/portfolios', {contentType: 'form'})"`,
		`data-attr:disabled="$saving"`, "Create portfolio", `<a class="btn btn--ghost" href="/portfolios">Cancel</a>`,
		`data-signals="{walletsMode: 'all', assetsMode: 'all', assetQ: ''}"`,
		`name="name"`, `maxlength="60"`, "required", `placeholder="e.g. Long-term"`,
		"All wallets", "Only selected wallets", "All assets", "Only selected assets",
		"Watch only (shown with live price, not counted)", `name="watch"`, `placeholder="ETH AVAX spl:<mint>"`,
		`id="form-errors"`,
	)
	lacks(t, un, `name="editId"`, "Save changes")
	// Both pickers start hidden when "All" is chosen, and are toggled by the mode signals.
	if got := strings.Count(n.html, `class="pf-pick" data-show="$walletsMode === &#39;selected&#39;" style="display:none"`) +
		strings.Count(n.html, `class="pf-pick" data-show="$assetsMode === &#39;selected&#39;" style="display:none"`); got != 2 {
		t.Errorf("both pickers must start hidden in All mode, got %d", got)
	}
	for _, id := range []string{"f-walletsMode-all", "f-assetsMode-all"} {
		tag := tagOf(t, n.html, `id="`+id+`"`)
		if !strings.Contains(tag, "checked") || !strings.Contains(tag, `type="radio"`) {
			t.Errorf("%s should be a checked radio: %s", id, tag)
		}
	}
	for id, bind := range map[string]string{"f-walletsMode-all": "walletsMode", "f-walletsMode-selected": "walletsMode", "f-assetsMode-all": "assetsMode", "f-assetsMode-selected": "assetsMode"} {
		if tag := tagOf(t, n.html, `id="`+id+`"`); !strings.Contains(tag, `data-bind="`+bind+`"`) || !strings.Contains(tag, `name="`+bind+`"`) {
			t.Errorf("%s must be bound to %s: %s", id, bind, tag)
		}
	}

	// Editing Long-term: only selected wallets, the signal and the ticks follow the data.
	lt := byName(t, pages, "portfolio form edit long-term")
	lun := page{"edit (unescaped)", html.UnescapeString(lt.html), false}
	has(t, lun,
		"Edit Long-term", "Save changes", `name="editId" value="long-term"`,
		`action="/portfolios/long-term"`, `data-on:submit="@post('/portfolios/long-term', {contentType: 'form'})"`,
		`data-signals="{walletsMode: 'selected', assetsMode: 'all', assetQ: ''}"`,
		"Exchanges", "Bitcoin", "Solana", "Manual", "Coinbase", "Kraken", "Cold storage", "Phantom", "Paper wallet at mom's",
		"Exchange", "Hardware wallet", "Hot wallet", "Only holdings in these wallets count.",
		`class="src-dot src-dot--exchange"`, `class="src-dot src-dot--hot"`, `class="src-dot src-dot--hardware"`, `class="src-dot src-dot--manual"`,
		`value="ETH LINK"`,
	)
	lacks(t, lun, "Create portfolio")
	ticked := map[string]bool{}
	for _, m := range regexp.MustCompile(`<input [^>]*name="wallets"[^>]*>`).FindAllString(lt.html, -1) {
		v := regexp.MustCompile(`value="([^"]*)"`).FindStringSubmatch(m)[1]
		ticked[v] = strings.Contains(m, "checked")
	}
	want := map[string]bool{"cold": true, "coinbase": false, "kraken": false, "phantom": true, "mom-safe": true, "fidelity": true, "roth-ira": true}
	if len(ticked) != len(want) {
		t.Errorf("wallet checkboxes = %v", ticked)
	}
	for id, on := range want {
		if ticked[id] != on {
			t.Errorf("wallet %s ticked=%v, want %v", id, ticked[id], on)
		}
	}
	if strings.Contains(lt.html, `class="pf-pick" data-show="$walletsMode === &#39;selected&#39;" style="display:none"`) {
		t.Error("the wallet picker is visible when only selected wallets are chosen")
	}
	if !strings.Contains(lt.html, `class="pf-pick" data-show="$assetsMode === &#39;selected&#39;" style="display:none"`) {
		t.Error("the asset picker must be hidden for All assets")
	}

	// Editing Majors: the asset picker with search, ticks, extra and watch text.
	mj := byName(t, pages, "portfolio form edit majors")
	mun := page{"majors (unescaped)", html.UnescapeString(mj.html), false}
	has(t, mun,
		`data-signals="{walletsMode: 'all', assetsMode: 'selected', assetQ: ''}"`,
		`data-bind="assetQ"`, `<span class="sr-only">Search your assets</span>`,
		`data-on:keydown="evt.key === 'Enter' && evt.preventDefault()"`,
		`data-search="btc bitcoin btc"`, `data-search="usdc usd coin usdc"`,
		`data-show="!$assetQ || el.dataset.search.includes($assetQ.toLowerCase())"`,
		`name="assets" value="BTC" checked`, `name="assets" value="USDC">`,
		"Bitcoin", "USD Coin", "Add assets you don't hold yet", `placeholder="ETH, spl:<mint>"`,
		`value="AVAX spl:J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn"`, `value="DOGE ARB"`,
		"Only these assets count. If you also picked wallets, a holding must match both.",
	)
	if n := len(regexp.MustCompile(`<label[^>]*data-search=`).FindAllString(mj.html, -1)); n != 12 {
		t.Errorf("want one filterable row per held asset (12), got %d", n)
	}
	// Asset values are private figures too.
	has(t, mj, `<span class="fig-v">$30,209.57</span>`)
	// The search box has no name, so it is never posted.
	if tag := tagOf(t, mj.html, `id="pf-asset-q"`); strings.Contains(tag, "name=") {
		t.Errorf("the asset search must not be submitted: %s", tag)
	}
	if !strings.Contains(tagOf(t, mj.html, `class="pf-pick" data-show="$walletsMode`), `style="display:none"`) {
		t.Error("wallet picker hidden for All wallets")
	}

	// Validation errors: inline, described, and in the summary.
	inv := byName(t, pages, "portfolio form invalid")
	has(t, inv,
		`role="alert"`, "Nothing was saved. Fix the fields below and try again.",
		`<p id="f-name-err" class="field__error">`, "Give the portfolio a name.",
		`<p id="f-wallets-err" class="field__error">`, "Pick at least one wallet, or choose All wallets.",
		"<strong>Name:</strong> Give the portfolio a name.", "<strong>Wallets:</strong> Pick at least one wallet",
		`aria-invalid="true"`, `aria-describedby="f-name-err"`, `aria-describedby="f-wallets-err f-wallets-help"`,
	)

	// A wallet the portfolio lists but that no longer exists is called out; saving drops it.
	gone := byName(t, pages, "portfolio form missing wallet")
	has(t, gone, "1 wallet in this portfolio no longer exists: ", `<span class="mono">old-ledger</span>`, "Saving removes it.")
	lacks(t, byName(t, pages, "portfolio form edit long-term"), "no longer exists")
	// Nothing to pick from: say so instead of rendering empty boxes.
	none := byName(t, pages, "portfolio form no options")
	has(t, none, "No wallets yet.", `href="/sources/new"`, "Nothing is held yet. Add assets by symbol below.")
	lacks(t, none, `class="pick-box`, `id="pf-asset-q"`)

	// Every choice is a labelled input: the row is the label, and it is at least 48px tall.
	for _, name := range []string{"portfolio form new", "portfolio form edit long-term", "portfolio form edit majors"} {
		p := byName(t, pages, name)
		inputs := regexp.MustCompile(`<input [^>]*type="(radio|checkbox)"[^>]*>`).FindAllString(p.html, -1)
		if len(inputs) < 4 {
			t.Errorf("%s: only %d radios/checkboxes", name, len(inputs))
		}
		for _, in := range inputs {
			id := regexp.MustCompile(`id="([^"]+)"`).FindStringSubmatch(in)[1]
			if !regexp.MustCompile(`<label class="choice[^"]*" for="` + id + `"`).MatchString(p.html) {
				t.Errorf("%s: %s is not inside a .choice label for it", name, id)
			}
		}
	}
	css := readStatic(t, "app.css")
	if b := cssBlock(t, css, ".choice {"); !strings.Contains(b, "min-height: 48px") {
		t.Errorf(".choice must be at least 44px tall: %s", b)
	}
}

func TestPortfolioPagesEscapeHostileValues(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>'"&`
	pp := demo.Portfolios(now)
	pp.Items = append([]templates.PortfolioItem(nil), pp.Items...)
	pp.Items[0].ID = "x'y</script><i>"
	pp.Items[0].Name = evil
	pp.Items[1].Missing = []string{evil}
	pp.LoadError = evil
	pp.Backups = append([]templates.BackupRow(nil), pp.Backups...)
	pp.Backups[0].Name = "b'</script>.yaml"
	pp.Import = templates.ImportOutcome{Message: evil}
	out := render(t, templates.PortfoliosFull(pp))

	pf := demo.PortfolioForm(demo.PFMajors, now)
	pf.EditID = "x'y</script><i>"
	pf.Name = evil
	pf.ExtraAssets, pf.Watch = evil, evil
	pf.AssetOptions = append([]templates.AssetOption(nil), pf.AssetOptions...)
	pf.AssetOptions[0].Key, pf.AssetOptions[0].Name = evil, evil
	pf.WalletOptions = append([]templates.WalletOption(nil), pf.WalletOptions...)
	pf.WalletOptions[0].Label = evil
	pf.Errors = map[string]string{"name": evil, "": evil}
	form := render(t, templates.PortfolioFormFull(pf))

	for name, o := range map[string]string{"manager": out, "form": form} {
		for _, bad := range []string{"<img", "<i>", "</script><i>", "onerror=alert(1)>"} {
			if strings.Contains(o, bad) {
				t.Errorf("%s: unescaped %q in output", name, bad)
			}
		}
		if strings.Count(o, "<script") != 2 {
			t.Errorf("%s: script injected", name)
		}
	}
	un := html.UnescapeString(out)
	for _, want := range []string{
		`@post('/portfolios/x%27y%3C%2Fscript%3E%3Ci%3E/delete')`,
		`confirm('Delete the portfolio \u003cimg src=x onerror=alert(1)\u003e\'\"\u0026? Your wallets are not affected.')`,
		`@post('/portfolios/restore/b%27%3C%2Fscript%3E.yaml')`,
		`href="/portfolios/x%27y%3C%2Fscript%3E%3Ci%3E/edit"`,
	} {
		if !strings.Contains(un, want) {
			t.Errorf("manager is missing %q; delete/restore expressions:\n%s", want, grep(un, "/delete"))
		}
	}
	fun := html.UnescapeString(form)
	if !strings.Contains(fun, `data-on:submit="@post('/portfolios/x%27y%3C%2Fscript%3E%3Ci%3E', {contentType: 'form'})"`) {
		t.Errorf("form action not escaped:\n%s", grep(fun, "data-on:submit"))
	}
}

// TestOverviewPortfolioChanges: the 7d column, the Edit link and the Manage links.
func TestOverviewPortfolioChanges(t *testing.T) {
	pages := allPages(t)
	ov := byName(t, pages, "overview")
	has(t, ov,
		`<th class="col-chg num" scope="col">24h</th><th class="col-chg7 num" scope="col">7d</th>`,
		// BTC +5.84% and SOL +12.40% over 7 days, ETH -3.12%, USDC flat.
		`<td class="col-chg7 num"><span class="chg-cell"><span class="chg chg--up">▲ +5.84%</span> <span class="chg chg--usd chg--up"><span class="fig"><span class="fig-v">+$1,666.89</span>`,
		// The 24h column carries the dollar change too: BTC +2.14% on ~$30,210 held.
		`<td class="col-chg num"><span class="chg-cell"><span class="chg chg--up">▲ +2.14%</span> <span class="chg chg--usd chg--up"><span class="fig"><span class="fig-v">+$632.94</span><span class="fig-m" role="img" aria-label="Hidden amount">$ ••••</span>`,
		`<span class="chg chg--up">▲ +12.40%</span>`, `<span class="chg chg--down">▼ −3.12%</span>`,
		`<span class="chg chg--flat">– 0.00%</span>`,
		// Assets without history show the dash with an accessible name.
		`<span class="chg chg--none" aria-label="No 7 day change available">—</span>`,
		// The sidebar and the phone menu lead to the manager.
		`class="nav-manage" href="/portfolios" data-on:click=`, `class="nav-item pf-menu__manage" href="/portfolios" data-on:click=`, `>Manage portfolios</a>`,
		`href="/portfolios/new"`,
	)
	lacks(t, ov, `nav-head--on`, "pf-edit", "/portfolios/all/edit")
	if n := strings.Count(ov.html, `<td class="col-chg7 num">`); n != 13 {
		t.Errorf("want a 7d cell on every holdings row (12 assets and the cash group), got %d", n)
	}
	// The watch list has no 7d data, so it keeps just 24h.
	if strings.Contains(byName(t, pages, "overview empty").html, "col-chg7") {
		t.Error("the empty overview has no table")
	}

	lt := byName(t, pages, "overview long-term")
	has(t, lt, `<a class="btn btn--secondary btn--sm pf-edit" href="/portfolios/long-term/edit" aria-label="Edit Long-term">Edit</a>`, "76% of all holdings", "Manage portfolios")
	mj := byName(t, pages, "overview majors")
	has(t, mj, "Majors", `href="/portfolios/majors/edit"`)

	// The portfolio menu is there even when there is only "All holdings" (it holds Manage).
	solo := demo.Overview(now)
	solo.Portfolios = solo.Portfolios[:1]
	if out := render(t, templates.OverviewFull(solo)); !strings.Contains(out, "Manage portfolios") {
		t.Error("the phone menu should always offer Manage portfolios")
	}
	// A portfolio with a hostile id still gets a safe Edit href.
	bad, _ := demo.OverviewFor(demo.PFLongTerm, "1W", now)
	bad.PF = "a b/c"
	if out := render(t, templates.OverviewFull(bad)); !strings.Contains(out, `href="/portfolios/a%20b%2Fc/edit"`) {
		t.Error("Edit href must path-escape the portfolio id")
	}
}

// ---- activity, reports, alerts ------------------------------------------------------------

func unescaped(p page) page {
	return page{p.name + " (unescaped)", html.UnescapeString(p.html), p.full}
}

func TestActivityPage(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "activity")
	un := unescaped(p)
	has(t, p,
		`<title id="page-title">Activity · CryptKeeper</title>`, `<h1 class="h-title">Activity</h1>`, "Synced from your wallets and exchanges",
		`aria-label="Hide balances"`, `<a class="nav-item nav-item--on" href="/activity" data-on:click=`,
		`<div class="activity" data-signals="{actSource: &#39;&#39;, actAsset: &#39;&#39;, actKind: &#39;&#39;, actRange: &#39;&#39;, actFrom: &#39;&#39;, actTo: &#39;&#39;}">`,
		`role="group" aria-label="Filter activity"`, `data-indicator="filtering"`,
		`<label class="field__label" for="f-act-source">Source</label>`, `<label class="field__label" for="f-act-asset">Asset</label>`, `<label class="field__label" for="f-act-kind">Kind</label>`,
		`data-bind="actSource"`, `data-bind="actAsset"`, `data-bind="actKind"`,
		"All sources", "All assets", "All kinds", "Received", "Sent", "Transfers", "Trades", "Swaps", "Rewards", "Staking", "Fees",
		`<option value="coinbase">Coinbase</option>`, `<option value="BTC">BTC · Bitcoin</option>`, `<option value="trade">Trades</option>`,
		`role="list" aria-label="Activity"`, `role="listitem"`,
	)
	has(t, un,
		`data-on:change="@get('/activity/list', {filterSignals: {include: /^act/}})"`,
		`data-signals="{actSource: '', actAsset: '', actKind: '', actRange: '', actFrom: '', actTo: ''}"`,
	)
	if n := strings.Count(p.html, `class="tx"`) + strings.Count(p.html, `class="tx tx--failed"`); n != demo.ActivityPageSize {
		t.Errorf("a page holds %d rows, got %d", demo.ActivityPageSize, n)
	}
	// Titles and detail lines of the fixtures.
	has(t, p,
		"Transfer · Coinbase → Cold storage", "Swapped SOL → USDC", "Staking reward", "Bought BTC", "Sold ETH",
		`<span class="tx__disc tx__disc--accent" aria-hidden="true">⇄</span>`, `<span class="tx__disc tx__disc--gain" aria-hidden="true">★</span>`,
		`<span class="chip chip--sm chip--warn tx__status" title="Pending, 2 confirmations so far"><span aria-hidden="true">◐</span> <span>2 conf</span></span>`,
		`<span class="chip chip--sm chip--neutral tx__status" title="Confirmed"><span>Confirmed</span></span>`,
		`<span class="tx__hash"><a href="https://mempool.space/tx/`, `target="_blank" rel="noopener noreferrer"`, `>3f9a…c21e</a>`,
		`class="src-dot src-dot--hardware"`, `class="src-dot src-dot--exchange"`, `class="src-dot src-dot--hot"`,
		`<time class="tx__when" datetime="`, `<span class="tx__clock">, `,
	)
	// Amounts and values are private figures; direction and status are not.
	has(t, p,
		`<span class="tx__amt tx__amt--in"><span class="fig"><span class="fig-v">+0.0500 BTC</span><span class="fig-m" role="img" aria-label="Hidden amount">+•••• BTC</span></span></span>`,
		`<span class="tx__amt"><span class="fig"><span class="fig-v">−0.0500 BTC</span>`,
		`<span class="fig-v">$3,206.00</span>`, `$ ••••••`, `<span class="fig-v">+296.12 USDC</span>`,
	)
	// The sync card.
	has(t, p, `id="activity-sync"`, "Activity sync", "4 sources", "1,204 entries", "38 entries", "Synced 2m ago", "Synced 3h ago",
		"Up to date", "Backfilling history…", `class="sync-row__err"`)
	// A failed sync reads as plain language, with the raw error kept behind Details (which
	// stays open across the stream's re-renders), and a running sync shows as such.
	has(t, p, "Rate limited: Kraken asked CryptKeeper to slow down. CryptKeeper tries again every 15 min.",
		`<details class="sync-row__raw" data-preserve-attr="open"><summary>Details</summary> <code>kraken: rate limited: EAPI:Rate limit exceeded</code></details>`,
		`<span class="chip chip--info">`, "Syncing…")
	// The card keeps itself current over SSE until navigation removes the stream element. On
	// this page the stream also offers new entries: it is told the 1,550 the page has.
	has(t, unescaped(p), `<div hidden data-init="@get('/activity/sync?feed=1550', {filterSignals: {include: /^$/}, requestCancellation: 'cleanup'})"></div>`,
		`<div id="activity-new" class="act-new" aria-live="polite"></div>`)
	// The prompt reloads the list with the current filters.
	has(t, unescaped(page{"new", render(t, templates.ActivityNew(true)), false}),
		`<button type="button" class="btn btn--secondary act-new__btn" data-on:click="@get('/activity/list', {filterSignals: {include: /^act/}})">`, "New activity synced · Show")

	// Load older: one element carries the click, the intersection and the indicator.
	if m := byName(t, pages, "activity more"); true {
		mun := unescaped(m)
		cursor := demo.Activity("", "", "", "", now).More
		want := `@get('/activity/list?before=` + cursor + `', {filterSignals: {include: /^act/}})`
		has(t, mun, `data-on:click="`+want+`"`, `data-on-intersect__once="`+want+`"`, `data-indicator="loadingMore"`,
			`data-attr:disabled="$loadingMore"`, `data-class="{'is-busy': $loadingMore}"`, ">Load older activity</button>", `type="button"`)
	}
	if got := strings.TrimSpace(byName(t, pages, "activity more none").html); got != `<div id="activity-more" class="act-more"></div>` {
		t.Errorf("no older entries renders an empty container, got %q", got)
	}
	last := demo.Activity("", "", "", "", now)
	for last.More != "" {
		last = demo.Activity("", "", "", last.More, now)
	}
	if got := strings.TrimSpace(render(t, templates.ActivityMore(last))); got != `<div id="activity-more" class="act-more"></div>` {
		t.Errorf("the last page has no load-more control: %s", got)
	}

	// Rows only: no wrapper, no ids, so a page can be appended to #activity-rows.
	rows := byName(t, pages, "activity rows")
	if !strings.HasPrefix(rows.html, `<div class="tx`) || strings.Contains(rows.html, ` id="`) {
		t.Errorf("rows fragment must be bare rows, starts with %.60q", rows.html)
	}

	// Filters: the current values are selected and ride in the signals.
	f := byName(t, pages, "activity filtered")
	fun := unescaped(f)
	has(t, f, `<option value="coinbase" selected>Coinbase</option>`, `<option value="BTC" selected>BTC · Bitcoin</option>`, `<option value="trade" selected>Trades</option>`)
	has(t, fun, `data-signals="{actSource: 'coinbase', actAsset: 'BTC', actKind: 'trade', actRange: '', actFrom: '', actTo: ''}"`)
	lacks(t, f, "Swapped SOL")

	// Empty states.
	nm := byName(t, pages, "activity no match")
	has(t, nm, "No matching activity", "Nothing matches these filters.", `href="/activity"`, "Clear filters")
	lacks(t, nm, `id="activity-rows"`, "No activity yet")
	em := byName(t, pages, "activity empty")
	has(t, em, "No activity yet", "It appears as CryptKeeper syncs your sources; history is backfilled gradually.", "Not synced yet", "0 entries")
	lacks(t, em, `id="activity-rows"`, "Clear filters", "Load older")

	// Failed and pending rows.
	var failed, pending page
	for _, pg := range pages {
		if strings.Contains(pg.html, "tx--failed") && failed.name == "" {
			failed = pg
		}
		if strings.Contains(pg.html, "2 conf") && pending.name == "" {
			pending = pg
		}
	}
	if failed.name == "" || pending.name == "" {
		t.Fatal("no page shows a failed and a pending row")
	}
	// Confirmations show only while pending.
	all := ""
	for _, pg := range pages {
		all += pg.html
	}
	if !regexp.MustCompile(`class="chip chip--sm chip--loss tx__status" title="Failed, no funds moved"><span aria-hidden="true">✕</span> <span>Failed</span>`).MatchString(all) {
		t.Error("a failed entry shows the loss chip")
	}
	if strings.Contains(all, "40 conf") || strings.Contains(all, "6 conf") {
		t.Error("confirmations are only shown while pending")
	}
}

func TestActivityRowsEdgeCases(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	row := func(tx core.Tx, label string) templates.ActivityRow {
		tx.Time = at
		return templates.ActivityRow{Tx: tx, SourceLabel: label, SourceKind: core.KindHardware}
	}
	// No legs: a dash with a name. No fiat value: a dash with a name. More than two legs.
	out := render(t, templates.ActivityRows([]templates.ActivityRow{
		row(core.Tx{Kind: core.TxOther, Note: "Contract call"}, "Phantom"),
		row(core.Tx{Kind: core.TxReceive, Legs: []core.Leg{{Symbol: "BTC", Amount: 1}}}, "Cold storage"),
		row(core.Tx{Kind: core.TxSwap, Legs: []core.Leg{
			{Symbol: "SOL", Amount: -1, USD: usdPtr(-150)}, {Symbol: "JUP", Amount: -2}, {Symbol: "USDC", Amount: 150, USD: usdPtr(150)}, {Symbol: "BONK", Amount: 5},
		}}, "Phantom"),
	}, now))
	if !strings.Contains(out, `<span class="tx__amt tx__amt--none" aria-label="No amount">—</span>`) {
		t.Error("an entry without legs shows a dash")
	}
	if !strings.Contains(out, `<span class="tx__fiat" aria-label="Value at the time is not known">—</span>`) {
		t.Error("an entry without a value shows a dash")
	}
	if !strings.Contains(out, `<span class="tx__more">+2 more</span>`) || len(regexp.MustCompile(`class="tx__amt[ "]`).FindAllString(out, -1)) != 1+1+2 {
		t.Errorf("only two legs show, then +N more: %s", out)
	}
	if strings.Contains(out, `class="tx__hash`) || strings.Contains(out, `class="tx__fee`) {
		t.Error("no hash and no fee, nothing rendered")
	}
}

func usdPtr(v float64) *float64 { return &v }

func TestActivityHostileValues(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>'"&`
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rows := demo.ActivityRows(now)[:3]
	rows[0].SourceLabel, rows[0].PeerLabel = evil, evil
	rows[0].Tx.Counterparty = evil
	rows[0].Tx.Kind = core.TxReceive
	rows[1].Tx.ExplorerURL = "javascript:alert(1)"
	rows[1].Tx.TxHash = "abc123abc123abc123"
	rows[2].Tx.ExplorerURL = "http://insecure.example/tx/1"
	rows[2].Tx.TxHash = "def456def456def456"
	pg := demo.Activity("", "", "", "", now)
	pg.Rows = rows
	pg.More = `1'+"</script><b>`
	pg.SourceID, pg.AssetKey, pg.Kind = `x'y</script><i>`, evil, evil
	pg.Sources = append(pg.Sources, templates.WalletOption{ID: `a'b`, Label: evil})
	pg.Assets = append(pg.Assets, templates.AssetOption{Key: evil, Symbol: evil, Name: evil})
	pg.Sync = append(pg.Sync, templates.SyncRow{ID: "x", Label: evil, LastError: evil})
	out := render(t, templates.ActivityFull(pg))
	for _, bad := range []string{"<img", "<i>", "</script><i>", "</script><b>", "onerror=alert(1)>", "javascript:", "insecure.example"} {
		if strings.Contains(out, bad) {
			t.Errorf("unescaped or unsafe %q in output", bad)
		}
	}
	if strings.Count(out, "<script") != 2 {
		t.Error("script injected")
	}
	un := html.UnescapeString(out)
	if !strings.Contains(un, `{actSource: 'x\'y\u003c/script\u003e\u003ci\u003e', actAsset: '\u003cimg src=x onerror=alert(1)\u003e\'\"\u0026', actKind:`) {
		t.Errorf("filter signals not escaped:\n%s", grep(un, "actSource"))
	}
	if !strings.Contains(un, `@get('/activity/list?before=1%27%2B%22%3C%2Fscript%3E%3Cb%3E', {filterSignals: {include: /^act/}})`) {
		t.Errorf("cursor not escaped:\n%s", grep(un, "before="))
	}
	// Only the one untouched https explorer link opens a tab.
	if n := strings.Count(out, `target="_blank"`); n != 1 {
		t.Errorf("only https explorer links may open: %d links", n)
	}
}

func TestReportsPage(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "reports")
	un := unescaped(p)
	has(t, p,
		`<title id="page-title">Reports · CryptKeeper</title>`, `<h1 class="h-title">Reports</h1>`, "Profit and loss, cost basis and exports",
		`<a class="nav-item nav-item--on" href="/reports" data-on:click=`,
		// Tiles.
		"Cost basis", "Value", "Unrealized P/L", "Basis known for 92% of value", `class="cover"`, `style="width:91.8%;"`,
		`<span class="stat__value chg--up">▲ <span class="fig">`, `<span class="stat__sub chg--up">+23.32%</span>`, "Crypto holdings, at today's prices",
		// Table.
		`id="pnl"`, "FIFO tax lots, all wallets", `<th class="col-asset" scope="col">Asset</th>`, `>Amount</th>`, `>Avg cost</th>`, `>Cost basis</th>`, `>Value</th>`, `>Unrealized</th>`,
		"Bitcoin", "Polkadot", "Bonk", "0.0500 BTC with unknown cost", "185.2000 DOT with unknown cost",
		`<span class="chg chg--up">▲ <span class="fig"><span class="fig-v">+$9,023.17</span>`, `<span class="chg pl__pct chg--up">+50.18%</span>`,
		`<span class="chg chg--down">▼ <span class="fig"><span class="fig-v">−$553.50</span>`, `<span class="chg pl__pct chg--down">−4.44%</span>`,
		`<span class="chg chg--none" aria-label="No profit or loss available">—</span>`,
		`<span class="fig-v">$42,694.48</span>`, `$ ••••`,
		// Realized.
		"Realized P/L", "by year", ">2026<", ">2025<", ">2024<", "+$1,204.12", "−$212.40", "+$3,880.00",
		// Exports and the disclaimer.
		`href="/export/holdings.csv" download`, `href="/export/history.csv?range=All" download`, `href="/export/pnl.csv" download`, `href="/export/activity.csv" download`,
		`aria-label="Download holdings CSV"`, "First in, first out, per wallet, from your tax lots. Not tax advice.",
	)
	// The manual cost panel and its "Set cost" links are gone: the tax lots are the only
	// source of cost basis.
	lacks(t, p, `id="basis-panel"`, `id="basis-form"`, "/reports/basis", ">Set cost</a>", `<span class="badge">manual</span>`,
		"Manual cost basis", "Average cost", "notice--loss", `role="alert"`)
	for _, m := range regexp.MustCompile(`<a [^>]*download[^>]*>`).FindAllString(p.html, -1) {
		if strings.Contains(m, "data-on") {
			t.Errorf("export link must be a plain anchor: %s", m)
		}
	}
	if n := strings.Count(p.html, " download"); n != 4 {
		t.Errorf("want 4 export links, got %d", n)
	}

	// CSV import.
	has(t, p,
		`id="csv-import"`, "Import from a CSV", `<form id="csv-form" class="form" method="post" action="/reports/import" enctype="multipart/form-data"`,
		`data-indicator="importingCsv"`, `name="source" value="coinbase" checked`, `name="source" value="kraken"`,
		"Coinbase transaction history CSV", "Kraken ledgers CSV", `name="file" type="file" accept=".csv,text/csv" required`,
		`<div id="csv-result" class="import-result" aria-live="polite"></div>`, ">Import CSV</button>",
	)
	has(t, un, `data-on:submit="@post('/reports/import', {contentType: 'form'})"`)
	if strings.Contains(tagOf(t, p.html, `value="kraken"`), "checked") {
		t.Error("Coinbase is the default export")
	}

	// Messages.
	m := byName(t, pages, "reports messages")
	has(t, m, `<div role="alert"><div class="notice notice--loss">`, "data/tax.yaml has an error and was not reloaded: line 3: bad indent", "notice--gain", "Imported 148 transactions (112 new).")
	ok := byName(t, pages, "csv ok")
	has(t, ok, "notice--gain", "Imported 148 transactions (112 new).")
	bad := byName(t, pages, "csv failed")
	has(t, bad, "notice--loss", "Nothing was imported: line 1: not a Kraken export.")
	if got := strings.TrimSpace(byName(t, pages, "csv empty").html); got != `<div id="csv-result" class="import-result" aria-live="polite"></div>` {
		t.Errorf("empty csv result should be an empty container, got %q", got)
	}

	// Nothing held yet.
	e := byName(t, pages, "reports empty")
	has(t, e, "Nothing is held yet.", "No sales on record yet.", `<span class="stat__value stat__value--none">—</span>`, "Needs a cost basis", "No cost basis known yet")
	lacks(t, e, "<table", `class="year-list"`)
}

func TestReportsKoinlyLinks(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "reports")
	has(t, p, `id="koinly-card-h"`, "Import from Koinly", `<a class="btn btn--secondary btn--block" href="/reports/koinly" data-on:click=`)
	lacks(t, p, "no cost basis", `class="realized__warn"`)
	m := byName(t, pages, "reports missing basis")
	card := m.html[strings.Index(m.html, `id="realized-h"`):]
	card = card[:strings.Index(card, "</section>")]
	text := strings.Join(strings.Fields(html.UnescapeString(reTag.ReplaceAllString(card, ""))), " ")
	if !strings.Contains(text, "3 sales have no cost basis — import your opening basis from Koinly") || !strings.Contains(card, `href="/reports/koinly"`) {
		t.Errorf("realized card: %q", text)
	}
}

func TestKoinlyPage(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "koinly")
	un := unescaped(p)
	has(t, p,
		`<title id="page-title">Import from Koinly · CryptKeeper</title>`, `<h1 class="h-title">Import from Koinly</h1>`,
		`<a class="nav-item nav-item--on" href="/reports" data-on:click=`, `<a class="back" href="/reports"`,
		// Step 1: upload, and what is loaded.
		`<form id="koinly-upload" class="form" method="post" action="/reports/koinly/upload" enctype="multipart/form-data"`,
		`name="file" type="file" accept=".csv,text/csv" multiple required`, "Loaded for 2025",
		"Holdings as at Dec 31, 2025: 4 assets", "Transaction history: 212 transactions", "Capital gains: 37 rows", "Income: not uploaded",
		"koinly_transaction_history.csv", "notice--gain",
		// Step 2: one select per Koinly wallet and asset, the chosen option selected.
		`<label for="kw-0">Ledger</label>`, `<select class="input" id="kw-0" name="w.Ledger">`,
		`<option value="cold" selected>Cold storage (Bitcoin)</option>`, `<option value="untracked" selected>Untracked (no longer synced)</option>`,
		`<label for="ka-2">JUP (Jupiter)</label>`, "Several assets use the symbol JUP: choose the right one.",
		`<input class="koinly-map__skip-box" type="checkbox" id="ks-2" name="s.JUP (Jupiter)" value="1" checked>`, `<label class="koinly-map__skip" for="ks-2">`,
		`<option value="" selected>Skip</option>`, `<option value="BTC" selected>BTC · Bitcoin</option>`,
		// Step 3: the reconciliation, the problems above Accept, the extra basis input.
		`id="koinly-recon"`, ">Dated in 2025</th>", ">Before 2025</th>", ">Untracked</th>", ">CryptKeeper extra</th>",
		`name="x.SOL"`, `value="212.5"`, `<label class="sr-only" for="kx-1">Your cost for the 1.2500 SOL Koinly has no cost for, in US dollars</label>`,
		"Check these before you accept:", "<li>BTC: Koinly 0.35 vs CryptKeeper 0.30006114</li>",
		"Accept and close 2025", "Discard import", "Update reconciliation",
	)
	has(t, un,
		`data-on:submit="@post('/reports/koinly/upload', {contentType: 'form'})"`,
		`data-on:change="@post('/reports/koinly/map', {contentType: 'form'})"`,
		`data-on:click="confirm('Close 2025 with these opening lots?`, `&& @post('/reports/koinly/accept')"`,
		`data-attr:disabled="$koinlySaving || $koinlyAccepting"`,
		`@post('/reports/koinly/discard')`,
	)
	if strings.Index(p.html, "Check these before you accept:") > strings.Index(p.html, `id="koinly-accept"`) {
		t.Error("the problems must show above Accept")
	}
	if n := strings.Count(p.html, `name="x.`); n != 1 {
		t.Errorf("only the asset with units missing a basis takes one, got %d inputs", n)
	}

	e := byName(t, pages, "koinly empty")
	has(t, e, `id="koinly-upload"`)
	lacks(t, e, `id="koinly-map"`, `id="koinly-recon"`, "Loaded for")

	b := byName(t, pages, "koinly blocked")
	has(t, b, "Holdings: not uploaded (needed to accept)", "Nothing was loaded: coinbase.csv: not a Koinly report.",
		`<button type="button" id="koinly-accept" class="btn btn--primary" disabled aria-describedby="koinly-blocked">`,
		`<p id="koinly-blocked" class="field__help">Upload the end of year holdings report first`,
		"Koinly's assets appear here once you upload the end of year holdings report.", "Nothing to reconcile yet")
	lacks(t, b, "koinlyAccepting", "Check these before you accept:")
}

func TestKoinlyHostileNamesAreEscaped(t *testing.T) {
	const evil = `x"><img src=x onerror=alert(1)>'`
	p := demo.Koinly(now)
	p.Wizard.Wallets[0].Name = evil
	p.Wizard.Assets[0].Name = evil
	p.Wizard.Recon[1].AssetKey = evil
	p.Wizard.Problems = append(p.Wizard.Problems, evil)
	p.Wizard.Files = append(p.Wizard.Files, evil)
	out := render(t, templates.KoinlyFull(p))
	if strings.Contains(out, "<img") || strings.Count(out, "<script") != 2 {
		t.Errorf("hostile Koinly names broke out of the page")
	}
	if !strings.Contains(out, `name="w.x&#34;&gt;&lt;img src=x onerror=alert(1)&gt;&#39;"`) {
		t.Errorf("wallet field name not escaped:\n%s", grep(out, "name=\"w."))
	}
}

func TestOverviewPnLColumn(t *testing.T) {
	pages := allPages(t)
	ov := byName(t, pages, "overview")
	un := unescaped(ov)
	has(t, ov,
		`<th class="col-pnl num" scope="col" data-show="$showPnl" style="display:none">P/L</th>`,
		`<td class="col-pnl num" data-show="$showPnl" style="display:none"><span class="chg-cell"><span class="chg chg--up">▲ +50.18%</span> <span class="chg chg--usd chg--up"><span class="fig"><span class="fig-v">+$9,023.17</span>`,
		`<td class="col-pnl num" data-show="$showPnl" style="display:none"><span class="chg-cell"><span class="chg chg--down">▼ −4.44%</span> <span class="chg chg--usd chg--down"><span class="fig"><span class="fig-v">−$553.50</span>`,
		`<span class="chg chg--none" aria-label="No profit or loss available">—</span>`,
		`class="btn btn--secondary btn--sm pnl-toggle" aria-pressed="false"`,
	)
	has(t, un,
		`data-on:click="$showPnl = !$showPnl"`, `data-attr:aria-pressed="$showPnl ? 'true' : 'false'"`, `data-class="{'is-pressed': $showPnl}"`,
		`data-signals="{hide: false, theme: 'system', range: '1W', pf: 'all', q: '', showHidden: false, showPnl: false, cashOpen: false}"`,
	)
	if n := strings.Count(ov.html, `<td class="col-pnl num"`); n != 13 {
		t.Errorf("want a P/L cell on every holdings row (12 assets and the cash group), got %d", n)
	}
	// Without P/L data there is no toggle and no column.
	bare := demo.Overview(now)
	bare.PnL = nil
	out := render(t, templates.OverviewFull(bare))
	if strings.Contains(out, "pnl-toggle") || strings.Contains(out, "col-pnl") {
		t.Error("no P/L data, no P/L column")
	}
	// The P/L toggle is the only addition to the 24h and 7d columns.
	has(t, ov, `<th class="col-chg num" scope="col">24h</th><th class="col-chg7 num" scope="col">7d</th>`)
	// The watch list holds nothing, so its 24h column is the percent alone.
	if i := strings.Index(ov.html, `class="tbl tbl--watch"`); i < 0 || strings.Contains(ov.html[i:], "chg--usd") {
		t.Error("watch list rows must not show a dollar change")
	}
}

func TestSettingsActivitySync(t *testing.T) {
	s := byName(t, allPages(t), "settings")
	has(t, s,
		`id="activity-sync"`, "Activity sync", "1,204 entries", "Backfilling history…", "Up to date", "Rate limited: Kraken asked CryptKeeper to slow down.",
		"@get(&#39;/activity/sync&#39;",
		`id="set-pf-h"`, "Portfolios", `<a class="btn btn--secondary" href="/portfolios">Manage portfolios</a>`,
		"Appearance", "Privacy", "About this install",
	)
	// Without a Now the card still renders relative times from the wall clock.
	p := demo.Settings(now)
	p.Now = time.Time{}
	p.Sync = []templates.SyncRow{{ID: "x", Label: "X", Kind: core.KindExchange, Count: 1, LastSync: time.Now().Add(-time.Minute)}}
	out := render(t, templates.SettingsFull(p))
	if !strings.Contains(out, "1 entry") || !strings.Contains(out, "Synced 1m ago") {
		t.Errorf("zero Now should fall back to the wall clock: %s", grep(out, "entry"))
	}
}

func TestWalletActivityTab(t *testing.T) {
	pages := allPages(t)
	cb := byName(t, pages, "wallet coinbase activity")
	has(t, cb, `class="tab tab--on"`, `class="tx-list" role="list"`, "Transfer · Coinbase → Cold storage", "Sold ETH", `class="btn btn--ghost tx-all" href="/activity?source=coinbase"`, "View all activity")
	if n := strings.Count(cb.html, `role="listitem"`); n != 25 {
		t.Errorf("the tab shows the 25 most recent entries, got %d", n)
	}
	cold := byName(t, pages, "wallet cold activity")
	has(t, cold, "Received BTC", "Transfer · Kraken → Cold storage", "2 conf")
	lacks(t, cold, "View all activity", "Coming soon")
	mom := byName(t, pages, "wallet mom-safe activity")
	has(t, mom, "No activity synced for this wallet yet.")
	lacks(t, mom, `role="list"`, "View all activity")
	// The Datastar tab link still patches the tab.
	has(t, unescaped(cb), `data-on:click__prevent="history.replaceState(null, '', '/w/coinbase?tab=activity'); @get('/w/coinbase/tab/activity')"`)
}

func TestAlertsPage(t *testing.T) {
	pages := allPages(t)
	p := byName(t, pages, "alerts")
	un := unescaped(p)
	has(t, p,
		`<title id="page-title">Alerts · CryptKeeper</title>`, `<h1 class="h-title">Alerts</h1>`, "Get a notification when something changes",
		`<a class="nav-item nav-item--on" href="/alerts" data-on:click=`, `<a class="btn btn--primary btn--add" href="/alerts/new">`,
		// Rules.
		`id="alert-rules"`, "5 rules", "Cold storage watch", "Any decrease in Cold storage", "BTC above $70,000", "Big day", "Kraken down", "Large send",
		`<span class="chip chip--sm chip--info">Balance drop</span>`, `<span class="chip chip--sm chip--info">Price</span>`, `<span class="chip chip--sm chip--info">Portfolio move</span>`,
		`<span class="chip chip--sm chip--info">Source down</span>`, `<span class="chip chip--sm chip--info">Large send</span>`,
		`<span class="chip chip--gain"><span aria-hidden="true">●</span> <span>On</span></span>`, `<span class="chip chip--neutral"><span aria-hidden="true">◌</span> <span>Paused</span></span>`,
		`href="/alerts/cold-drop/edit" aria-label="Edit Cold storage watch"`, `aria-label="Delete Cold storage watch"`, `class="btn btn--danger btn--sm"`,
		// Notifications.
		"ntfy.example.org", "Amounts hidden in notifications", `data-indicator="testing"`, `data-attr:disabled="$testing"`, ">Send test notification</button>",
		`<div id="alert-test" class="import-result" aria-live="polite"></div>`,
		// Recent.
		`id="alert-recent"`, "Large send from Coinbase", "5h ago", "2d ago", "Couldn't send: ntfy returned 503 Service Unavailable.", `class="log-row__err"`,
		"Cold storage went from 0.32006114 BTC",
	)
	has(t, un,
		`data-on:click="@post('/alerts/test')"`,
		`data-on:click="confirm('Delete the alert Cold storage watch?') && @post('/alerts/cold-drop/delete')"`,
	)
	if n := strings.Count(p.html, `class="log-row"`); n != 4 {
		t.Errorf("want 4 recent alerts, got %d", n)
	}
	lacks(t, p, "ntfy.url", "No alerts yet")

	e := byName(t, pages, "alerts empty")
	has(t, e, "No alerts yet", "An alert tells you when something changes", "A source stops syncing", `<a class="btn btn--primary" href="/alerts/new">`,
		"Notifications aren't set up.", `<span class="mono">ntfy.url</span>`, `<span class="mono">config.yaml</span>`, "notice--warn",
		"Nothing has fired yet.", `id="alert-test"`)
	if tag := tagOf(t, e.html, `disabled>Send test`); !strings.Contains(tag, "disabled") {
		t.Errorf("without ntfy the test button is disabled: %s", tag)
	}
	lacks(t, e, `@post(&#39;/alerts/test&#39;)`, `data-indicator="testing"`, `class="rule"`)

	le := byName(t, pages, "alerts load error")
	has(t, le, "CryptKeeper can't read alerts.yaml.", "window &#34;1 day&#34; is not a valid duration", "Fix the file and reload the page.", "notice--warn", `class="rule"`)
	lacks(t, le, "No alerts yet")

	ok := byName(t, pages, "alerts tested")
	has(t, ok, "notice--gain", "Test notification sent. Check your phone.")
	has(t, byName(t, pages, "alert test failed"), "notice--loss", "Sending failed: 503")
	if got := strings.TrimSpace(byName(t, pages, "alert test empty").html); got != `<div id="alert-test" class="import-result" aria-live="polite"></div>` {
		t.Errorf("empty test result should be an empty container, got %q", got)
	}
	// Without redaction the notice says amounts are included.
	plain := demo.Alerts(now)
	plain.Redact = false
	if out := render(t, templates.AlertsFull(plain)); strings.Contains(out, "Amounts hidden") || !strings.Contains(out, "Notifications include amounts.") {
		t.Error("redaction off: amounts are included")
	}
}

// activeGroups returns the legends of the field groups of the alert form that are neither
// hidden nor disabled on first render.
func activeGroups(t *testing.T, html string) []string {
	t.Helper()
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<fieldset class="alert-fields"([^>]*)>\s*<legend[^>]*>([^<]*)</legend>`).FindAllStringSubmatch(html, -1) {
		hidden := strings.Contains(m[1], `style="display:none"`)
		disabled := regexp.MustCompile(`\sdisabled(\s|>|$)`).MatchString(m[1])
		if hidden != disabled {
			t.Errorf("group %q must be hidden and disabled together: %s", m[2], m[1])
		}
		if !hidden {
			out = append(out, m[2])
		}
	}
	return out
}

func TestAlertForm(t *testing.T) {
	pages := allPages(t)
	n := byName(t, pages, "alert form new")
	un := unescaped(n)
	has(t, n,
		`<title id="page-title">New alert · CryptKeeper</title>`, `<h1 class="h-title">New alert</h1>`, "Pick what to watch for", `<a class="back" href="/alerts" data-on:click=`,
		`<form id="alert-form" class="form card pf-form" method="post" action="/alerts"`, `data-indicator="saving"`, `data-attr:disabled="$saving"`,
		`<label class="field__label" for="f-type">Alert type</label>`, `name="type"`, `data-bind="ruleType"`,
		`<option value="balance_drop" selected>Balance drop (cold-storage security)</option>`, `<option value="price">Price above or below</option>`,
		`<option value="portfolio_move">Portfolio move</option>`, `<option value="source_down">Source down</option>`, `<option value="large_activity">Large send</option>`,
		"Leave all unchecked to watch every non-exchange wallet.", "Minimum drop (%)", `name="wallets" value="cold"`, `name="wallets" value="coinbase"`,
		`for="f-enabled"`, `name="enabled" value="on" checked`, ">Create alert</button>", `<a class="btn btn--ghost" href="/alerts">Cancel</a>`, `id="form-errors"`,
		`name="minPct"`, `name="asset"`, `list="al-assets"`, `name="above"`, `name="below"`, `name="portfolio"`, `name="pct"`, `name="window"`, `name="minutes"`, `name="minUsd"`, `name="cooldown"`,
		`<option value="1h">1 hour</option>`, `<option value="6h" selected>6 hours</option>`, `<option value="24h">24 hours</option>`, `<option value="168h">7 days</option>`,
		`<option value="all" selected>All holdings</option>`, `<option value="long-term">Long-term</option>`,
	)
	has(t, un,
		`data-signals="{ruleType: 'balance_drop'}"`, `data-on:submit="@post('/alerts', {contentType: 'form'})"`,
		`data-show="$ruleType === 'balance_drop'"`, `data-attr:disabled="!($ruleType === 'balance_drop')"`,
		`data-show="$ruleType === 'price' || $ruleType === 'portfolio_move' || $ruleType === 'large_activity'"`,
	)
	lacks(t, un, `name="editId"`, "Save alert")
	if got := activeGroups(t, n.html); len(got) != 1 || got[0] != "Wallets to watch" {
		t.Errorf("a new alert shows the balance-drop fields only, got %v", got)
	}
	if strings.Contains(tagOf(t, n.html, `name="wallets" value="cold"`), "checked") {
		t.Error("no wallet is ticked by default (every non-exchange wallet is watched)")
	}

	// Editing: the signal, the visible group and the values follow the rule.
	price := byName(t, pages, "alert form edit "+demo.AlertBTCAbove)
	pun := unescaped(price)
	has(t, pun,
		`<title id="page-title">Edit alert · CryptKeeper</title>`, `action="/alerts/btc-70k"`, `name="editId" value="btc-70k"`, `data-signals="{ruleType: 'price'}"`,
		`data-on:submit="@post('/alerts/btc-70k', {contentType: 'form'})"`, `<option value="price" selected>Price above or below</option>`,
		`value="BTC"`, `value="70000"`, `value="BTC above $70,000"`, `<option value="24h" selected>24 hours</option>`, ">Save alert</button>",
		`<datalist id="al-assets">`, `<option value="BTC">Bitcoin</option>`,
	)
	lacks(t, pun, "Create alert")
	if got := activeGroups(t, price.html); len(got) != 2 || got[0] != "Price target" || got[1] != "Repeat notifications" {
		t.Errorf("price rule groups = %v", got)
	}
	if !strings.Contains(tagOf(t, price.html, `name="enabled"`), "checked") {
		t.Error("an enabled rule has the box ticked")
	}

	cold := byName(t, pages, "alert form edit "+demo.AlertColdDrop)
	if !strings.Contains(tagOf(t, cold.html, `name="wallets" value="cold"`), "checked") || strings.Contains(tagOf(t, cold.html, `name="wallets" value="phantom"`), "checked") {
		t.Error("only the watched wallet is ticked")
	}

	move := byName(t, pages, "alert form edit "+demo.AlertDayMove)
	mun := unescaped(move)
	has(t, mun, `data-signals="{ruleType: 'portfolio_move'}"`, `value="10"`, `<option value="24h" selected>24 hours</option>`, `<option value="all" selected>All holdings</option>`)
	if got := activeGroups(t, move.html); len(got) != 2 || got[0] != "Portfolio move" || got[1] != "Repeat notifications" {
		t.Errorf("portfolio move groups = %v", got)
	}

	down := byName(t, pages, "alert form edit "+demo.AlertKrakenOff)
	has(t, down, `data-signals="{ruleType: &#39;source_down&#39;}"`, `value="60"`)
	if strings.Contains(tagOf(t, down.html, `name="enabled"`), "checked") {
		t.Error("a paused rule has the box cleared")
	}
	if got := activeGroups(t, down.html); len(got) != 1 || got[0] != "Source down" {
		t.Errorf("source down groups = %v (the cooldown does not apply)", got)
	}

	big := byName(t, pages, "alert form edit "+demo.AlertBigSend)
	has(t, big, `value="5000"`, `<option value="1h" selected>1 hour</option>`)
	if got := activeGroups(t, big.html); len(got) != 2 || got[0] != "Large send" || got[1] != "Repeat notifications" {
		t.Errorf("large send groups = %v", got)
	}
	// The cooldown group is the one without a legend of its own: it shows for three types.
	if !regexp.MustCompile(`(?s)<fieldset class="alert-fields" data-show="[^"]*large_activity[^"]*" data-attr:disabled="[^"]*"><legend class="sr-only">Repeat notifications`).MatchString(strings.ReplaceAll(big.html, "&#39;", "'")) {
		t.Error("the cooldown group shows for a large send")
	}

	// Invalid: inline errors and the summary, with the aria wiring.
	inv := byName(t, pages, "alert form invalid")
	has(t, inv,
		`role="alert"`, "Nothing was saved. Fix the fields below and try again.", `<p id="f-asset-err" class="field__error">`, "Choose an asset.",
		`<p id="f-above-err" class="field__error">`, "Set a price to alert above or below.", "<strong>Asset:</strong> Choose an asset.", "<strong>Rises above:</strong>",
		`aria-invalid="true"`, `aria-describedby="f-asset-err f-asset-help"`,
	)

	// A rule edited by hand can carry values the form does not offer: they stay selectable,
	// so saving does not silently change them.
	odd := demo.AlertForm(demo.AlertDayMove, now)
	odd.Values["window"], odd.Values["cooldown"], odd.Values["portfolio"] = "72h", "90m", "retired"
	out := render(t, templates.AlertFormFull(odd))
	has(t, page{"odd", out, true}, `<option value="72h" selected>72h (custom)</option>`, `<option value="90m" selected>90m (custom)</option>`, `<option value="retired" selected>retired</option>`)
}

func TestAlertAndReportPagesEscapeHostileValues(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>'"&`
	ap := demo.Alerts(now)
	ap.Rules = append([]templates.AlertRuleRow(nil), ap.Rules...)
	ap.Rules[0].ID, ap.Rules[0].Name, ap.Rules[0].Summary = "x'y</script><i>", evil, evil
	ap.Recent = append([]templates.AlertLogRow(nil), ap.Recent...)
	ap.Recent[0].Title, ap.Recent[0].Body, ap.Recent[0].Err = evil, evil, evil
	ap.NtfyHost, ap.LoadError = evil, evil
	ap.Test = templates.ImportOutcome{Message: evil}
	alerts := render(t, templates.AlertsFull(ap))

	af := demo.AlertForm(demo.AlertBTCAbove, now)
	af.EditID = "x'y</script><i>"
	for k := range af.Values {
		af.Values[k] = evil
	}
	af.Values["type"] = "price"
	af.WalletOptions = append([]templates.WalletOption(nil), af.WalletOptions...)
	af.WalletOptions[0].Label, af.WalletOptions[0].ID = evil, evil
	af.AssetOptions = append([]templates.AssetOption(nil), af.AssetOptions...)
	af.AssetOptions[0].Key, af.AssetOptions[0].Name = evil, evil
	af.PortfolioOptions = append([]templates.PortfolioNav(nil), af.PortfolioOptions...)
	af.PortfolioOptions[0].Name = evil
	af.Errors = map[string]string{"": evil, "asset": evil}
	form := render(t, templates.AlertFormFull(af))

	rp := demo.Reports(now)
	rp.Rows = append([]templates.PnLRow(nil), rp.Rows...)
	rp.Rows[0].AssetKey, rp.Rows[0].Symbol, rp.Rows[0].Name = "x'y</script><i>", evil, evil
	rp.Rows[4].AssetKey = "a'b\\"
	rp.TaxErr = evil
	rp.Import = templates.ImportOutcome{Message: evil}
	reports := render(t, templates.ReportsFull(rp))

	for name, o := range map[string]string{"alerts": alerts, "alert form": form, "reports": reports} {
		for _, bad := range []string{"<img", "<i>", "</script><i>", "onerror=alert(1)>"} {
			if strings.Contains(o, bad) {
				t.Errorf("%s: unescaped %q in output", name, bad)
			}
		}
		if strings.Count(o, "<script") != 2 {
			t.Errorf("%s: script injected", name)
		}
	}
	aun, fun, run := html.UnescapeString(alerts), html.UnescapeString(form), html.UnescapeString(reports)
	for _, want := range []string{
		`confirm('Delete the alert \u003cimg src=x onerror=alert(1)\u003e\'\"\u0026?') && @post('/alerts/x%27y%3C%2Fscript%3E%3Ci%3E/delete')`,
		`href="/alerts/x%27y%3C%2Fscript%3E%3Ci%3E/edit"`,
	} {
		if !strings.Contains(aun, want) {
			t.Errorf("alerts is missing %q:\n%s", want, grep(aun, "/delete"))
		}
	}
	if !strings.Contains(fun, `data-on:submit="@post('/alerts/x%27y%3C%2Fscript%3E%3Ci%3E', {contentType: 'form'})"`) {
		t.Errorf("alert form action not escaped:\n%s", grep(fun, "data-on:submit"))
	}
	if strings.Contains(run, "/reports/basis") || strings.Contains(run, "f-asset") {
		t.Errorf("reports must not carry the retired manual cost scripts:\n%s", grep(run, "basis"))
	}
}
