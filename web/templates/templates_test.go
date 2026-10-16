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

	full("settings", templates.SettingsFull(demo.Settings(now)))
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
	for _, name := range []string{"wallets", "wallet cold holdings"} {
		lacks(t, byName(t, pages, name), `id="live"`)
	}

	// Patch fragments start with their root id, so a patch replaces the right element.
	roots := map[string]string{
		"dashboard": "dashboard", "value card": "value-card", "sync chip synced": "sync-status",
		"wallet tab cold utxos": "wallet-tab", "test ok": "test-result", "test empty": "test-result",
		"errors none": "form-errors", "errors some": "form-errors",
	}
	for name, id := range roots {
		p := byName(t, pages, name)
		if !regexp.MustCompile(`^<[a-z]+ id="` + id + `"`).MatchString(p.html) {
			t.Errorf("%s: root element must be #%s, starts with %.60q", name, id, p.html)
		}
	}
}

var (
	reOnDash      = regexp.MustCompile(`data-on-[a-z]`)
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
				if m := g.re.FindString(p.html); m != "" {
					t.Errorf("%s (found %q)", g.what, m)
				}
			}

			// Scripts: exactly the Datastar module on whole pages, none in fragments.
			n := len(reScript.FindAllString(p.html, -1))
			switch {
			case p.full && (n != 1 || !strings.Contains(p.html, datastarScript)):
				t.Errorf("want exactly the Datastar module script, found %d <script", n)
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
	has(t, ov, `<span class="fig-v">$57,131.27</span>`, `$ ••••••`, `•••• BTC`, `$ ••••`)
	lacks(t, ov, `fig-v">$64,120.00`, `fig-v">+2.14%`)
}

func TestLayoutHead(t *testing.T) {
	p := byName(t, allPages(t), "overview")
	has(t, p,
		`<html lang="en">`,
		`<title>Overview · Vault</title>`,
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
		`data-signals="{hide: false, theme: 'system', range: '1W', pf: 'all', q: '', showHidden: false}"`,
		`data-class="{'amounts-hidden': $hide}"`,
		`$hide = s.getItem('vault.hide') === '1'`,
		`document.documentElement.dataset.theme`,
	)

	// The live stream element is hidden and uses data-init, never data-on:load.
	has(t, p, `<div id="live" hidden data-init="@get(&#39;/stream&#39;, {filterSignals: {include: /^(pf|range)$/}})"></div>`)
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
	if strings.Count(out, "<script") != 1 || strings.Contains(out, "</script><b>") || strings.Contains(out, "<b>") {
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
	if strings.Count(out, "<script") != 1 {
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
	has(t, e, "Nothing to show yet", `href="/sources/new"`, "Vault never needs trade or withdrawal permission.")
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

	// No cost basis yet: the tiles show a dash and the hint.
	ph := byName(t, pages, "wallet phantom holdings")
	has(t, ph, "Set cost basis", "Needs a cost basis", `stat__value stat__value--none`)
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
	has(t, act, "Coming soon")

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
	has(t, w, "5 sources", "Wallets", "Paper wallet at mom&#39;s", `href="/w/mom-safe"`, `class="acct__rail rail--hardware"`, `class="acct__rail rail--exchange"`, `class="acct__rail rail--hot"`, `class="acct__rail rail--manual"`, "Vault is read-only")
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
		"Grant <strong>view</strong> permissions only. Vault never needs trade or transfer access.",
		"API key name", "organizations/…/apiKeys/…", "Private key (PEM or Ed25519)", "<textarea", `autocomplete="off"`, `spellcheck="false"`,
		`value="Coinbase"`)
	kr := byName(t, pages, "form add kraken")
	has(t, kr, "Connect Kraken", "Enable <strong>Query Funds</strong> and <strong>Query Ledger Entries</strong> only.", `type="password"`, "API secret")
	btc := byName(t, pages, "form add bitcoin")
	has(t, btc, "Track a Bitcoin wallet", `enctype="multipart/form-data"`, `type="file"`, `name="walletFile"`, `accept=".json,.txt"`,
		"zpub6r… / wsh(sortedmulti(…)) / bc1q…", "Extended public key, output descriptor, or address",
		`<option value="hardware" selected>`, `<option value="hot">`, `<option value="p2tr">`, "Auto-detect",
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
	plain := map[string]bool{"on": true, "effect": true, "init": true}
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
	for _, k := range []string{"on", "attr", "class", "show", "signals", "init", "effect"} {
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
