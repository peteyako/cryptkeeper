package templates

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// unquoteJS decodes the subset of JavaScript string-literal escapes JSString emits, so
// the tests can check that every value survives a round trip.
func unquoteJS(t *testing.T, lit string) string {
	t.Helper()
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		t.Fatalf("not a single-quoted literal: %q", lit)
	}
	body := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(body); {
		c := body[i]
		if c == '\'' {
			t.Fatalf("unescaped quote in %q", lit)
		}
		if c != '\\' {
			r, n := utf8.DecodeRuneInString(body[i:])
			b.WriteRune(r)
			i += n
			continue
		}
		i++
		if i >= len(body) {
			t.Fatalf("dangling backslash in %q", lit)
		}
		switch body[i] {
		case '\\', '\'', '"':
			b.WriteByte(body[i])
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			if i+5 > len(body) {
				t.Fatalf("short \\u escape in %q", lit)
			}
			n, err := strconv.ParseUint(body[i+1:i+5], 16, 32)
			if err != nil {
				t.Fatalf("bad \\u escape in %q: %v", lit, err)
			}
			b.WriteRune(rune(n))
			i += 4
		default:
			t.Fatalf("unexpected escape \\%c in %q", body[i], lit)
		}
		i++
	}
	return b.String()
}

func TestJSString(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"plain", "bitcoin"},
		{"empty", ""},
		{"single quote", "Mom's wallet"},
		{"double quote", `say "hi"`},
		{"backslash", `C:\vault\keys`},
		{"backslash then quote", `\'`},
		{"script close", "</script><script>alert(1)</script>"},
		{"html comment", "<!-- x -->"},
		{"ampersand", "a&b"},
		{"backtick and template", "`${evil}`"},
		{"newline", "line1\nline2"},
		{"carriage return and tab", "a\r\nb\tc"},
		{"line separator", "a\u2028b\u2029c"},
		{"nul and control", "a\x00b\x1fc\x7f"},
		{"unicode", "Cold storage ₿ ✕ 日本"},
		{"dollar and at", "$hide @get('/x')"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := JSString(tc.in)
			if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
				t.Fatalf("not single-quoted: %q", got)
			}
			for _, bad := range []string{"\n", "\r", "\u2028", "\u2029", "</", "<", ">", "&", "`", "\x00"} {
				if strings.Contains(got, bad) {
					t.Errorf("literal %q still contains %q", got, bad)
				}
			}
			// The only unescaped single quotes are the delimiters.
			if strings.Count(strings.ReplaceAll(strings.ReplaceAll(got, `\\`, ""), `\'`, ""), "'") != 2 {
				t.Errorf("stray quote in %q", got)
			}
			if back := unquoteJS(t, got); back != tc.in {
				t.Errorf("round trip: got %q, want %q (literal %q)", back, tc.in, got)
			}
		})
	}
	if got := JSString("</script>"); got != `'\u003c/script\u003e'` {
		t.Errorf("JSString(</script>) = %s", got)
	}
	if got := JSString("a'b"); got != `'a\'b'` {
		t.Errorf("JSString quote = %s", got)
	}
}

// ---- Datastar expression checks, run in Node when it is available -----------------------

var dsRewrite = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|\$([A-Za-z_]\w*)|@([A-Za-z_$][\w$]*)\(`)

// datastarBody mimics how the Datastar bundle turns an expression into a function body:
// strings are left alone, $name becomes $['name'] and @action( becomes __action("action",evt,.
func datastarBody(expr string, returnsValue bool) string {
	body := strings.TrimSpace(expr)
	if returnsValue {
		body = "return (" + body + ");"
	}
	return dsRewrite.ReplaceAllStringFunc(body, func(m string) string {
		switch {
		case m[0] == '"' || m[0] == '\'':
			return m
		case m[0] == '$':
			return "$['" + m[1:] + "']"
		default:
			return `__action("` + m[1:len(m)-1] + `",evt,`
		}
	})
}

func needNode(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JavaScript syntax checks")
	}
	return path
}

// checkSyntax compiles body as a function body the way Datastar does, without running it.
func checkSyntax(t *testing.T, node, body string) {
	t.Helper()
	out, err := exec.Command(node, "-e", `new Function("el", "$", "__action", "evt", process.argv[1])`, body).CombinedOutput()
	if err != nil {
		t.Errorf("invalid JavaScript:\n%s\n%s", body, out)
	}
}

func TestExpressionsAreValidJavaScript(t *testing.T) {
	node := needNode(t)
	hostile := "o'neil\\</script>\n\u2028"
	cases := []struct {
		name    string
		expr    string
		returns bool
	}{
		{"body signals", bodySignals(Shell{PF: hostile, Range: "1M"}), true},
		{"body init", bodyInit, false},
		{"body effect", bodyEffect, false},
		{"live init", liveInit, false},
		{"refresh", refreshClick, false},
		{"test connection", testClick, false},
		{"range click", rangeClick("1M"), false},
		{"range class", rangeClass("1M"), true},
		{"range pressed", rangePressed("1M"), true},
		{"portfolio click", pfClick(hostile), false},
		{"portfolio class", pfClass(hostile), true},
		{"portfolio current", pfCurrent(hostile), true},
		{"tab click", tabClick(hostile, "addresses"), false},
		{"copy", copyClick(hostile), false},
		{"remove", removeClick(hostile), false},
		{"submit", submitClick(hostile), false},
		{"theme click", themeClick("dark"), false},
		{"theme pressed", themePressed("dark"), true},
		{"theme class", themeClass("dark"), true},
		{"hide click", hideSetClick(true), false},
		{"hide pressed", hideSetPressed(false), true},
		{"hide class", hideSetClass(true), true},
		{"search show", searchShow, true},
		{"hide toggle", "$hide = !$hide", false},
		{"hide attr", "$hide ? 'true' : 'false'", true},
		{"show hidden toggle", "$showHidden = !$showHidden", false},
		{"disabled", "$testing || $saving", true},
		{"portfolio delete", pfDeleteClick(hostile, hostile), false},
		{"portfolio restore", restoreClick(hostile), false},
		{"portfolio import", importSubmit, false},
		{"portfolio submit", pfSubmitClick(hostile), false},
		{"portfolio signals", pfFormSignals(PortfolioFormPage{AllAssets: true}), true},
		{"portfolio mode", pfModeSelected("walletsMode"), true},
		{"asset search", assetShow, true},
		{"enter guard", noEnterSubmit, false},
		{"importing", "$importing", true},
		{"activity filter", actFilterGet, false},
		{"activity signals", actSignals(ActivityPage{SourceID: hostile, AssetKey: hostile, Kind: hostile}), true},
		{"activity more", actMoreGet(hostile), false},
		{"basis submit", basisSubmit, false},
		{"csv submit", csvSubmit, false},
		{"basis delete", basisDeleteClick(hostile), false},
		{"set cost", setCostClick(hostile, 0.1), false},
		{"alert test", alertTestClick, false},
		{"alert delete", alertDeleteClick(hostile, hostile), false},
		{"alert submit", alertSubmitClick(hostile), false},
		{"alert signals", alertSignals(AlertFormPage{Values: map[string]string{"type": "price"}}), true},
		{"alert show", showExpr("price", "source_down"), true},
		{"alert hide", hideExpr("price", "source_down"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkSyntax(t, node, datastarBody(tc.expr, tc.returns)) })
	}
}

// runEffect evaluates the body effect with stub signals and a stub browser, and returns
// what it left behind as JSON.
func runEffect(t *testing.T, node, signals, storage string) string {
	t.Helper()
	script := fmt.Sprintf(`
const body = process.argv[1];
const document = { documentElement: { dataset: {} } };
const window = { localStorage: %s };
const $ = %s;
new Function("window", "document", "$", body)(window, document, $);
console.log(JSON.stringify({ theme: document.documentElement.dataset.theme ?? null, store: window.localStorage && window.localStorage.data ? window.localStorage.data : null }));
`, storage, signals)
	out, err := exec.Command(node, "-e", script, datastarBody(bodyEffect, false)).CombinedOutput()
	if err != nil {
		t.Fatalf("effect failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBodyEffectBehaviour(t *testing.T) {
	node := needNode(t)
	okStore := `{ data: {}, setItem(k, v) { this.data[k] = v } }`
	if got := runEffect(t, node, `{hide: true, theme: 'dark'}`, okStore); got != `{"theme":"dark","store":{"vault.hide":"1","vault.theme":"dark"}}` {
		t.Errorf("dark: %s", got)
	}
	if got := runEffect(t, node, `{hide: false, theme: 'system'}`, `{ data: {}, setItem(k, v) { this.data[k] = v; } }`); got != `{"theme":null,"store":{"vault.hide":"0","vault.theme":"system"}}` {
		t.Errorf("system must clear data-theme: %s", got)
	}
	// Storage that throws (private window) must not stop the theme from being applied.
	if got := runEffect(t, node, `{hide: false, theme: 'light'}`, `{ setItem() { throw new Error("denied") } }`); got != `{"theme":"light","store":null}` {
		t.Errorf("throwing storage: %s", got)
	}
	// No storage at all.
	if got := runEffect(t, node, `{hide: false, theme: 'light'}`, `null`); got != `{"theme":"light","store":null}` {
		t.Errorf("missing storage: %s", got)
	}
}

func TestBodyInitBehaviour(t *testing.T) {
	node := needNode(t)
	run := func(storage string) string {
		script := fmt.Sprintf(`
const window = { localStorage: %s };
const $ = { hide: false, theme: 'system' };
new Function("window", "$", process.argv[1])(window, $);
console.log(JSON.stringify($));
`, storage)
		out, err := exec.Command(node, "-e", script, datastarBody(bodyInit, false)).CombinedOutput()
		if err != nil {
			t.Fatalf("init failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(`{ getItem(k) { return { 'vault.hide': '1', 'vault.theme': 'dark' }[k] ?? null } }`); got != `{"hide":true,"theme":"dark"}` {
		t.Errorf("restore: %s", got)
	}
	if got := run(`{ getItem() { return null } }`); got != `{"hide":false,"theme":"system"}` {
		t.Errorf("nothing stored: %s", got)
	}
	if got := run(`{ getItem() { throw new Error("denied") } }`); got != `{"hide":false,"theme":"system"}` {
		t.Errorf("throwing storage: %s", got)
	}
	if got := run(`null`); got != `{"hide":false,"theme":"system"}` {
		t.Errorf("missing storage: %s", got)
	}
}

func TestExpressionBuilders(t *testing.T) {
	if got := rangeClick("1M"); got != `$range = '1M'; @get('/chart', {filterSignals: {include: /^(pf|range)$/}})` {
		t.Errorf("rangeClick = %s", got)
	}
	if got := rangeClass("1W"); got != `{'seg--on': $range === '1W'}` {
		t.Errorf("rangeClass = %s", got)
	}
	if got := rangePressed("1W"); got != `$range === '1W' ? 'true' : 'false'` {
		t.Errorf("rangePressed = %s", got)
	}
	if got := pfClick("long-term"); got != `$pf = 'long-term'; history.replaceState(null, '', '/p/long-term'); @get('/view', {filterSignals: {include: /^(pf|range)$/}})` {
		t.Errorf("pfClick = %s", got)
	}
	if got := pfClick("all"); !strings.Contains(got, `history.replaceState(null, '', '/')`) {
		t.Errorf("all holdings should link to /: %s", got)
	}
	if got := tabClick("cold", "utxos"); got != `history.replaceState(null, '', '/w/cold?tab=utxos'); @get('/w/cold/tab/utxos')` {
		t.Errorf("tabClick = %s", got)
	}
	if got := copyClick("bc1qabc"); got != `navigator.clipboard && navigator.clipboard.writeText('bc1qabc')` {
		t.Errorf("copyClick = %s", got)
	}
	if got := removeClick("mom-safe"); !strings.HasSuffix(got, `@post('/sources/mom-safe/delete')`) || !strings.HasPrefix(got, "confirm('Remove this wallet?") {
		t.Errorf("removeClick = %s", got)
	}
	if got := submitClick(""); got != `@post('/sources', {contentType: 'form'})` {
		t.Errorf("submitClick add = %s", got)
	}
	if got := submitClick("a b"); got != `@post('/sources/a%20b', {contentType: 'form'})` {
		t.Errorf("submitClick edit = %s", got)
	}
	if got := pfDeleteClick("long-term", "Mom's coins"); got != `confirm('Delete the portfolio Mom\'s coins? Your wallets are not affected.') && @post('/portfolios/long-term/delete')` {
		t.Errorf("pfDeleteClick = %s", got)
	}
	if got := pfDeleteClick("a b/c", "x"); !strings.HasSuffix(got, `@post('/portfolios/a%20b%2Fc/delete')`) {
		t.Errorf("pfDeleteClick must path-escape the id: %s", got)
	}
	if got := restoreClick("20261016-114205.yaml"); got != `confirm('Restore this backup? The current list is backed up first.') && @post('/portfolios/restore/20261016-114205.yaml')` {
		t.Errorf("restoreClick = %s", got)
	}
	if got := pfSubmitClick(""); got != `@post('/portfolios', {contentType: 'form'})` {
		t.Errorf("pfSubmitClick create = %s", got)
	}
	if got := pfSubmitClick("a b"); got != `@post('/portfolios/a%20b', {contentType: 'form'})` {
		t.Errorf("pfSubmitClick edit = %s", got)
	}
	if got := pfFormSignals(PortfolioFormPage{AllWallets: true}); got != `{walletsMode: 'all', assetsMode: 'selected', assetQ: ''}` {
		t.Errorf("pfFormSignals = %s", got)
	}
	if got := pfModeSelected("assetsMode"); got != `$assetsMode === 'selected'` {
		t.Errorf("pfModeSelected = %s", got)
	}
	if pfEditHref("a b") != "/portfolios/a%20b/edit" || pfExportHref("a/b") != "/portfolios/a%2Fb/export" {
		t.Error("portfolio hrefs must path-escape the id")
	}
	if got := bodySignals(Shell{}); got != `{hide: false, theme: 'system', range: '1W', pf: 'all', q: '', showHidden: false, showPnl: false}` {
		t.Errorf("bodySignals defaults = %s", got)
	}
	if got := bodySignals(Shell{Range: "bogus", PF: "x"}); !strings.Contains(got, "range: '1W'") {
		t.Errorf("unknown range must fall back to 1W: %s", got)
	}
	if got := actFilterGet; got != `@get('/activity/list', {filterSignals: {include: /^act/}})` {
		t.Errorf("actFilterGet = %s", got)
	}
	if got := actSignals(ActivityPage{SourceID: "cold", Kind: "send"}); got != `{actSource: 'cold', actAsset: '', actKind: 'send'}` {
		t.Errorf("actSignals = %s", got)
	}
	// The cursor is URL-escaped for the query and JS-escaped for the expression.
	if got := actMoreGet("a+b/c=&d"); got != `@get('/activity/list?before=a%2Bb%2Fc%3D%26d', {filterSignals: {include: /^act/}})` {
		t.Errorf("actMoreGet = %s", got)
	}
	if got := actMoreGet("1760000000000000000"); got != `@get('/activity/list?before=1760000000000000000', {filterSignals: {include: /^act/}})` {
		t.Errorf("actMoreGet = %s", got)
	}
	if got := basisDeleteClick("BTC"); got != `confirm('Remove the manual cost for BTC?') && @post('/reports/basis/BTC/delete')` {
		t.Errorf("basisDeleteClick = %s", got)
	}
	if got := basisDeleteClick("spl:a b/c"); !strings.HasSuffix(got, `@post('/reports/basis/spl:a%20b%2Fc/delete')`) {
		t.Errorf("basisDeleteClick must path-escape the asset: %s", got)
	}
	if got := setCostClick("BTC", 0.05); got != `document.getElementById('f-asset').value = 'BTC'; document.getElementById('f-amount').value = '0.05'; document.getElementById('f-cost').focus()` {
		t.Errorf("setCostClick = %s", got)
	}
	if got := alertDeleteClick("cold-drop", "Mom's alert"); got != `confirm('Delete the alert Mom\'s alert?') && @post('/alerts/cold-drop/delete')` {
		t.Errorf("alertDeleteClick = %s", got)
	}
	if got := alertSubmitClick(""); got != `@post('/alerts', {contentType: 'form'})` {
		t.Errorf("alertSubmitClick create = %s", got)
	}
	if got := alertSubmitClick("a b"); got != `@post('/alerts/a%20b', {contentType: 'form'})` {
		t.Errorf("alertSubmitClick edit = %s", got)
	}
	if got := alertSignals(AlertFormPage{}); got != `{ruleType: 'balance_drop'}` {
		t.Errorf("alertSignals = %s", got)
	}
	if alertEditHref("a b/c") != "/alerts/a%20b%2Fc/edit" {
		t.Errorf("alertEditHref = %s", alertEditHref("a b/c"))
	}
	// An attacker-controlled portfolio id cannot break out of the literal.
	got := bodySignals(Shell{PF: `x'}; alert(1); ({`})
	if strings.Contains(got, `x'}`) {
		t.Errorf("portfolio id escaped its literal: %s", got)
	}
}
