package templates

import (
	"fmt"
	"net/url"
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
var Ranges = []string{"1D", "1W", "1M", "1Y", "All"}

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
		", pf: " + JSString(normPF(s.PF)) + ", q: '', showHidden: false}"
}

// bodyInit restores the persisted privacy and theme choice. Every storage access is
// guarded: localStorage can be missing or throw (private windows, blocked site data).
const bodyInit = `try { const s = window.localStorage; if (s) { $hide = s.getItem('vault.hide') === '1'; $theme = s.getItem('vault.theme') || 'system' } } catch (e) {}`

// bodyEffect persists the choice and applies the theme to <html data-theme>. Both
// signals are read up front so the effect always subscribes to both, even when storage
// is unavailable. It must come after data-init in the attribute order, or the defaults
// would be written over the stored values before they are restored.
const bodyEffect = `const h = $hide, t = $theme; try { const s = window.localStorage; if (s) { s.setItem('vault.hide', h ? '1' : '0'); s.setItem('vault.theme', t) } } catch (e) {} ; t === 'system' ? delete document.documentElement.dataset.theme : (document.documentElement.dataset.theme = t)`

// liveInit opens the persistent SSE stream.
const liveInit = `@get('/stream', ` + liveFilter + `)`

const refreshClick = `@post('/refresh', ` + liveFilter + `)`

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
