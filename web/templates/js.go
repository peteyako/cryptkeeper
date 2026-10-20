package templates

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Everything in this file builds Datastar expressions. Server values must always go
// through JSString: an unescaped quote, backslash or newline inside an attribute turns
// into a silent client-side syntax error (or worse, injected script).

// JSString returns s as a single-quoted JavaScript string literal that is safe to place
// inside a Datastar expression, an HTML attribute or even a <script> block: backslashes,
// quotes, control characters, U+2028/U+2029 and the HTML-significant characters < > & are
// escaped, so "</script>" can never close a script element.
func JSString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case ' ', ' ', '<', '>', '&', '`':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// Ranges are the chart ranges, in display order.
var Ranges = []string{"1D", "1W", "1M", "YTD", "1Y", "All"}

// liveFilter limits the signals sent to the server to the two it reads.
const liveFilter = `{filterSignals: {include: /^(pf|range)$/}}`

func normRange(r string) string {
	for _, k := range Ranges {
		if k == r {
			return r
		}
	}
	return "1W"
}

func normPF(id string) string {
	if id == "" {
		return "all"
	}
	return id
}

// bodySignals is the data-signals object on <body>. The body is never patched, so these
// signals live for the whole page.
func bodySignals(s Shell) string {
	return "{hide: false, theme: 'system', range: " + JSString(normRange(s.Range)) +
		", pf: " + JSString(normPF(s.PF)) + ", q: '', showHidden: false, showPnl: false, cashOpen: false}"
}

// bodyInit restores the persisted privacy and theme choice, falling back to the keys the
// app used as CryptKeep. Every storage access is guarded: localStorage can be missing or
// throw (private windows, blocked site data).
const bodyInit = `try { const s = window.localStorage; if (s) { $hide = (s.getItem('cryptkeeper.hide') || s.getItem('cryptkeep.hide')) === '1'; $theme = s.getItem('cryptkeeper.theme') || s.getItem('cryptkeep.theme') || 'system' } } catch (e) {}`

// bodyEffect persists the choice and applies the theme to <html data-theme>. Both
// signals are read up front so the effect always subscribes to both, even when storage
// is unavailable. It must come after data-init in the attribute order, or the defaults
// would be written over the stored values before they are restored.
const bodyEffect = `const h = $hide, t = $theme; try { const s = window.localStorage; if (s) { s.setItem('cryptkeeper.hide', h ? '1' : '0'); s.setItem('cryptkeeper.theme', t) } } catch (e) {} ; t === 'system' ? delete document.documentElement.dataset.theme : (document.documentElement.dataset.theme = t)`

// liveInit opens the persistent SSE stream. requestCancellation 'cleanup' aborts it when
// the #live element is removed — i.e. when navigating away from the overview — so a
// stream never outlives the page it patches.
const liveInit = `@get('/stream', {filterSignals: {include: /^(pf|range)$/}, requestCancellation: 'cleanup'})`

// syncStreamOpts send no signals and end the activity sync stream with its element.
const syncStreamOpts = `{filterSignals: {include: /^$/}, requestCancellation: 'cleanup'}`

// syncInit opens the activity sync stream (see SyncStream).
const syncInit = `@get('/activity/sync', ` + syncStreamOpts + `)`

// syncFeedInit is syncInit for the Activity page: total is the number of entries the page
// was rendered with, so the stream can offer the ones synced after it (ActivityNew).
func syncFeedInit(total int) string {
	return "@get('/activity/sync?feed=" + strconv.Itoa(total) + "', " + syncStreamOpts + ")"
}

const backfillClick = `@post('/chart/backfill', ` + liveFilter + `)`

const refreshClick = `@post('/refresh', ` + liveFilter + `)`

// refreshPricesClick is the prices chip: new prices on the balances already fetched.
const refreshPricesClick = `@post('/refresh/prices', ` + liveFilter + `)`

// findVaultClick posts the add-source form to the vault's Find button, which reads the
// wallet's address from it.
const findVaultClick = `@post('/sources/find-vault', {contentType: 'form', selector: '#source-form'})`

// testClick posts the add-source form to the connection test. It names the form by
// selector, which works whether or not the button sits inside the form.
const testClick = `@post('/sources/test', {contentType: 'form', selector: '#source-form'})`

const removeConfirm = `Remove this wallet? Its settings are deleted; on-chain funds are not affected.`

func rangeIs(r string) string { return "$range === " + JSString(r) }

func rangeClick(r string) string {
	return "$range = " + JSString(r) + "; @get('/chart', " + liveFilter + ")"
}

func rangeClass(r string) string { return "{'seg--on': " + rangeIs(r) + "}" }

func rangePressed(r string) string { return rangeIs(r) + " ? 'true' : 'false'" }

// pfHref is the real URL of a portfolio: "/" for all holdings, "/p/{id}" otherwise.
func pfHref(id string) string {
	if id == "" || id == "all" {
		return "/"
	}
	return "/p/" + url.PathEscape(id)
}

func pfClick(id string) string {
	return "$pf = " + JSString(id) + "; history.replaceState(null, '', " + JSString(pfHref(id)) + "); @get('/view', " + liveFilter + ")"
}

func pfCurrent(id string) string { return "$pf === " + JSString(id) + " ? 'page' : false" }

func pfClass(id string) string { return "{'nav-item--on': $pf === " + JSString(id) + "}" }

func walletHref(id string) string { return "/w/" + url.PathEscape(id) }

// coinHref is the coin page of an asset key; "spl:<mint>" keys are path-escaped.
func coinHref(key string) string { return "/a/" + url.PathEscape(key) }

// coinRangeClick loads a coin chart range on demand. The range goes in the query string so
// it never touches the overview's own $range signal; $coinRange marks the clicked button.
func coinRangeClick(key, rng string) string {
	return "$coinRange = " + JSString(rng) + "; @post(" + JSString(coinHref(key)+"/chart?range="+url.QueryEscape(rng)) + ")"
}

// coinChartSignals is the data-signals object on #coin-chart: the range shown and whether
// another one is loading. Every patch of the chart resets both.
func coinChartSignals(rng string) string {
	return "{coinRange: " + JSString(rng) + ", coinCharting: false}"
}

func coinRangeClass(r string) string { return "{'seg--on': $coinRange === " + JSString(r) + "}" }

func coinRangePressed(r string) string {
	return "$coinRange === " + JSString(r) + " ? 'true' : 'false'"
}

func tabHref(id, tab string) string { return walletHref(id) + "?tab=" + url.QueryEscape(tab) }

func tabClick(id, tab string) string {
	return "history.replaceState(null, '', " + JSString(tabHref(id, tab)) + "); @get(" + JSString(walletHref(id)+"/tab/"+url.PathEscape(tab)) + ")"
}

func copyClick(addr string) string {
	return "navigator.clipboard && navigator.clipboard.writeText(" + JSString(addr) + ")"
}

func removeClick(id string) string {
	return "confirm(" + JSString(removeConfirm) + ") && @post(" + JSString("/sources/"+url.PathEscape(id)+"/delete") + ")"
}

func editHref(id string) string { return "/sources/" + url.PathEscape(id) + "/edit" }

func formAction(editID string) string {
	if editID == "" {
		return "/sources"
	}
	return "/sources/" + url.PathEscape(editID)
}

func submitClick(editID string) string {
	return "@post(" + JSString(formAction(editID)) + ", {contentType: 'form'})"
}

func themeClick(v string) string { return "$theme = " + JSString(v) }

func themeOn(v string) string { return "$theme === " + JSString(v) }

func themePressed(v string) string { return themeOn(v) + " ? 'true' : 'false'" }

func themeClass(v string) string { return "{'seg--on': " + themeOn(v) + "}" }

func hideSetClick(v bool) string {
	if v {
		return "$hide = true"
	}
	return "$hide = false"
}

func hideSetOn(v bool) string {
	if v {
		return "$hide"
	}
	return "!$hide"
}

func hideSetPressed(v bool) string { return hideSetOn(v) + " ? 'true' : 'false'" }

func hideSetClass(v bool) string { return "{'seg--on': " + hideSetOn(v) + "}" }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---- portfolios ---------------------------------------------------------------------------

func pfEditHref(id string) string { return "/portfolios/" + url.PathEscape(id) + "/edit" }

func pfExportHref(id string) string { return "/portfolios/" + url.PathEscape(id) + "/export" }

// pfDeleteClick asks first, then posts the delete. The portfolio name goes through
// JSString inside the confirm text, so a quote in a name cannot break the expression.
func pfDeleteClick(id, name string) string {
	return "confirm(" + JSString("Delete the portfolio "+name+"? Your wallets are not affected.") + ") && @post(" + JSString("/portfolios/"+url.PathEscape(id)+"/delete") + ")"
}

const restoreConfirm = `Restore this backup? The current list is backed up first.`

func restoreClick(backup string) string {
	return "confirm(" + JSString(restoreConfirm) + ") && @post(" + JSString("/portfolios/restore/"+url.PathEscape(backup)) + ")"
}

// importSubmit posts the import form (multipart, so the file rides along).
const importSubmit = `@post('/portfolios/import', {contentType: 'form'})`

func pfFormAction(editID string) string {
	if editID == "" {
		return "/portfolios"
	}
	return "/portfolios/" + url.PathEscape(editID)
}

func pfSubmitClick(editID string) string {
	return "@post(" + JSString(pfFormAction(editID)) + ", {contentType: 'form'})"
}

func modeValue(all bool) string {
	if all {
		return "all"
	}
	return "selected"
}

// pfFormSignals is the data-signals object of the editor's wrapper. The wrapper is never
// patched (only #form-errors is), so these survive a failed save.
func pfFormSignals(p PortfolioFormPage) string {
	return "{walletsMode: " + JSString(modeValue(p.AllWallets)) + ", assetsMode: " + JSString(modeValue(p.AllAssets)) + ", assetQ: ''}"
}

// pfModeSelected is true while the "Only selected ..." radio of a section is chosen.
func pfModeSelected(signal string) string { return "$" + signal + " === 'selected'" }

// assetShow filters the asset picker rows against the search box.
const assetShow = `!$assetQ || el.dataset.search.includes($assetQ.toLowerCase())`

// noEnterSubmit stops Enter in the asset search from submitting the whole form.
const noEnterSubmit = `evt.key === 'Enter' && evt.preventDefault()`

// ---- activity ---------------------------------------------------------------------------

// actFilter limits the signals sent to the activity endpoints to the three filters.
const actFilter = `{filterSignals: {include: /^act/}}`

// actFilterGet reloads the list from the current filters.
const actFilterGet = `@get('/activity/list', ` + actFilter + `)`

// actSignals is the data-signals object of the Activity page. The wrapper that carries it
// is never patched (only #activity-list is), so the filters survive every patch.
func actSignals(p ActivityPage) string {
	return "{actSource: " + JSString(p.SourceID) + ", actAsset: " + JSString(p.AssetKey) +
		", actKind: " + JSString(p.Kind) + ", actRange: " + JSString(p.Period) +
		", actFrom: " + JSString(p.From) + ", actTo: " + JSString(p.To) + "}"
}

// actExportHref keeps the CSV link in step with the filters, so the download is exactly
// what the feed shows.
const actExportHref = `'/export/activity.csv?' + new URLSearchParams({source: $actSource, asset: $actAsset, kind: $actKind, range: $actRange, from: $actFrom, to: $actTo})`

// actMoreGet loads the page of older entries that starts at the opaque cursor.
func actMoreGet(cursor string) string {
	return "@get(" + JSString("/activity/list?before="+url.QueryEscape(cursor)) + ", " + actFilter + ")"
}

// ---- reports ----------------------------------------------------------------------------

// csvSubmit posts the CSV import form (multipart, so the file rides along).
const csvSubmit = `@post('/reports/import', {contentType: 'form'})`

// ---- Koinly import -----------------------------------------------------------------------

// koinlyUploadSubmit posts the Koinly report files (multipart, so the files ride along).
const koinlyUploadSubmit = `@post('/reports/koinly/upload', {contentType: 'form'})`

// koinlyMapSubmit saves the wallet and asset mappings and the extra bases; the map form
// also posts it on every change, so the reconciliation always follows the selects.
const koinlyMapSubmit = `@post('/reports/koinly/map', {contentType: 'form'})`

// koinlyRematchClick asks first, then maps every Koinly wallet and asset again from scratch.
const koinlyRematchClick = `confirm('Match every wallet and asset automatically? Choices you made here are replaced.') && @post('/reports/koinly/rematch')`

// koinlyAcceptClick asks first, then closes the year from the draft.
func koinlyAcceptClick(year int) string {
	msg := "Close " + strconv.Itoa(year) + " with these opening lots? Koinly's figures for " + strconv.Itoa(year) +
		" are saved as a closed year, and CryptKeeper's cost basis starts from them."
	return "confirm(" + JSString(msg) + ") && @post('/reports/koinly/accept')"
}

// koinlyDiscardClick asks first, then forgets the uploaded reports and mappings.
const koinlyDiscardClick = `confirm('Discard this import? The uploaded reports and the mappings you chose are forgotten.') && @post('/reports/koinly/discard')`

// ---- tax review ---------------------------------------------------------------------------

// reviewFormSubmit posts a review form (tag, price, write-off); the reply patches the panel.
const reviewFormSubmit = `@post(el.action, {contentType: 'form'})`

// reviewPostClick posts to path (a delete button).
func reviewPostClick(path string) string { return "@post(" + JSString(path) + ")" }

// reviewBulkSubmit asks first, then tags the whole group.
func reviewBulkSubmit(count int, symbol string) string {
	msg := "Tag all " + strconv.Itoa(count) + " " + symbol + " transactions with this tag?"
	return "confirm(" + JSString(msg) + ") && @post('/reports/tax/tag-bulk', {contentType: 'form'})"
}

// reviewBulkSelected tags the group's ticked rows: their boxes belong to the bulk form, and
// the button's only=selected says to tag just those.
const reviewBulkSelected = `@post('/reports/tax/tag-bulk', {contentType: 'form'})`

// reviewSelectAll ticks or clears every row box of the bulk form with id form.
func reviewSelectAll(form string) string {
	return "document.querySelectorAll('input[form=" + form + "]').forEach(c => c.checked = el.checked)"
}

// ---- alerts -----------------------------------------------------------------------------

// alertTestClick sends a test notification.
const alertTestClick = `@post('/alerts/test')`

func alertEditHref(id string) string { return "/alerts/" + url.PathEscape(id) + "/edit" }

func alertDeleteClick(id, name string) string {
	return "confirm(" + JSString("Delete the alert "+name+"?") + ") && @post(" + JSString("/alerts/"+url.PathEscape(id)+"/delete") + ")"
}

func alertFormAction(editID string) string {
	if editID == "" {
		return "/alerts"
	}
	return "/alerts/" + url.PathEscape(editID)
}

func alertSubmitClick(editID string) string {
	return "@post(" + JSString(alertFormAction(editID)) + ", {contentType: 'form'})"
}

// alertSignals is the data-signals object of the alert editor's wrapper, which is never
// patched (only #form-errors is), so the chosen type survives a failed save.
func alertSignals(p AlertFormPage) string {
	return "{ruleType: " + JSString(p.ruleType()) + "}"
}

// ruleTypeIs is true while the given alert type is chosen.
func ruleTypeIs(t string) string { return "$ruleType === " + JSString(t) }

// logosChange saves the token-logos setting when its checkbox changes.
const logosChange = `@post('/settings/logos', {contentType: 'form'})`

// pricesSubmit saves or clears the CoinGecko key.
const pricesSubmit = `@post('/settings/prices', {contentType: 'form'})`

// pricesClear deletes the saved CoinGecko key (a submitter button's value isn't in the form body).
const pricesClear = `@post('/settings/prices?clear=1')`

// bitcoinKeySubmit checks and saves the Blockstream API key.
const bitcoinKeySubmit = `@post('/settings/bitcoin', {contentType: 'form'})`

// bitcoinKeyClear deletes the saved Blockstream API key.
const bitcoinKeyClear = `@post('/settings/bitcoin?clear=1')`

// backupImportSubmit posts the restore form (multipart: the file and the mode).
const backupImportSubmit = `@post('/backup/import', {contentType: 'form'})`

// ResetWord is what Settings asks the user to type before erasing everything.
const ResetWord = "RESET"

// resetSubmit posts the reset form (the typed confirmation rides along).
const resetSubmit = `@post('/backup/reset', {contentType: 'form'})`

// resetDisabled keeps the reset button off until the confirmation word is typed.
const resetDisabled = `$resetting || ($resetConfirm || '').trim().toUpperCase() !== '` + ResetWord + `'`

// navClick turns an in-app link into a Datastar navigation: push the URL, then @get the
// same URL; the server answers a Datastar GET with only the page regions (title, sidebar,
// #main, bottom bar) over SSE, so styles, fonts and scripts never reload. Modified clicks
// (cmd/ctrl/shift/alt, middle button) keep the browser's default, e.g. open in a new tab.
// pf, when non-empty, sets the portfolio signal first (portfolio links and Overview).
func navClick(href, pf string) string {
	h := JSString(href)
	setPF := ""
	if pf != "" {
		setPF = "$pf = " + JSString(pf) + ", "
	}
	return "(evt.metaKey || evt.ctrlKey || evt.shiftKey || evt.altKey || evt.button) || (evt.preventDefault(), " +
		setPF + "$q = '', history.pushState(null, '', " + h + "), @get(" + h + ", " + liveFilter + "), window.scrollTo(0, 0))"
}

// navPop handles the browser's back/forward buttons the same way. Pages outside the app
// shell (the add-source flow) are reloaded instead, since they use a different layout.
const navPop = `location.pathname.startsWith('/sources/') ? location.reload() : ($pf = location.pathname.startsWith('/p/') ? decodeURIComponent(location.pathname.slice(3)) : 'all', $q = '', @get(location.pathname + location.search, ` + liveFilter + `))`
