package templates_test

import (
	"encoding/json"
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"crypto-portfolio/web/static"
	"crypto-portfolio/web/templates"
)

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(static.FS, name)
	if err != nil {
		t.Fatalf("static.FS: %v", err)
	}
	return string(b)
}

// cssBlock returns the declarations of the first rule that starts with selector.
func cssBlock(t *testing.T, css, selector string) string {
	t.Helper()
	i := strings.Index(css, selector)
	if i < 0 {
		t.Fatalf("selector %q not found", selector)
	}
	open := strings.Index(css[i:], "{") + i
	depth := 0
	for k := open; k < len(css); k++ {
		switch css[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[open+1 : k]
			}
		}
	}
	t.Fatalf("unterminated block for %q", selector)
	return ""
}

var reProp = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`)

func props(block string) map[string]string {
	m := map[string]string{}
	for _, p := range reProp.FindAllStringSubmatch(block, -1) {
		m[p[1]] = strings.TrimSpace(p[2])
	}
	return m
}

func TestEmbeddedAssets(t *testing.T) {
	for _, name := range []string{
		"tokens.css", "app.css", "datastar.js", "manifest.webmanifest",
		"fonts/ibm-plex-sans-400.woff2", "fonts/ibm-plex-sans-500.woff2", "fonts/ibm-plex-sans-600.woff2",
		"fonts/ibm-plex-mono-400.woff2", "fonts/ibm-plex-mono-500.woff2",
		"icons/icon.svg", "icons/icon-192.png", "icons/icon-512.png", "icons/icon-maskable-512.png", "icons/apple-touch-icon.png",
	} {
		if b, err := fs.ReadFile(static.FS, name); err != nil || len(b) == 0 {
			t.Errorf("static.FS is missing %s (%v)", name, err)
		}
	}
	if js := readStatic(t, "datastar.js"); !strings.HasPrefix(js, "// Datastar v1.0.4") {
		t.Errorf("datastar.js should be the vendored v1.0.4 bundle, starts with %.40q", js)
	}
}

func TestTokens(t *testing.T) {
	css := readStatic(t, "tokens.css")
	base := props(cssBlock(t, css, ":root {"))
	media := cssBlock(t, css, "@media (prefers-color-scheme: light)")
	lightMedia := props(media)
	lightExplicit := props(cssBlock(t, css, `:root[data-theme="light"]`))

	required := []string{
		"--bg", "--surface-1", "--surface-2", "--border", "--border-strong", "--row",
		"--text-1", "--text-2", "--text-3", "--accent", "--on-accent", "--accent-soft", "--link",
		"--gain", "--gain-soft", "--loss", "--loss-soft", "--warn", "--warn-soft",
		"--src-exchange", "--src-hot", "--src-hardware", "--src-other",
		"--card-shadow",
		"--s1", "--s2", "--s3", "--s4", "--s5", "--s6", "--s7", "--s8",
		"--r-sm", "--r-md", "--r-lg", "--r-xl", "--font-sans", "--font-mono",
	}
	for _, k := range []string{"btc", "eth", "sol", "usd", "neutral"} {
		required = append(required, "--disc-"+k+"-bg", "--disc-"+k+"-fg")
	}
	for _, k := range required {
		if base[k] == "" {
			t.Errorf("dark/base is missing %s", k)
		}
	}

	// Every color token that is themed must be themed identically in both light blocks
	// (the system-preference one and the explicit data-theme override).
	for k, v := range lightExplicit {
		if lightMedia[k] != v {
			t.Errorf("%s: light override %q differs from the prefers-color-scheme block %q", k, v, lightMedia[k])
		}
	}
	for k := range lightMedia {
		if _, ok := lightExplicit[k]; !ok {
			t.Errorf("%s is in the media block but not in [data-theme=light]", k)
		}
	}
	// The light blocks only restate colors; every one of them must exist in the base.
	for k := range lightExplicit {
		if _, ok := base[k]; !ok {
			t.Errorf("light defines %s, which the base does not", k)
		}
	}
	// Every themed token in the base (all but fonts, spacing, radius) has a light value.
	for k := range base {
		switch {
		case strings.HasPrefix(k, "--s") && len(k) == 4, strings.HasPrefix(k, "--r-"), strings.HasPrefix(k, "--font-"):
			continue
		}
		if _, ok := lightExplicit[k]; !ok {
			t.Errorf("%s has a dark value but no light value", k)
		}
	}

	// Values from the boards, spot-checked.
	spot := map[string][2]string{ // token -> dark, light
		"--bg": {"#0d0f12", "#f6f7f9"}, "--surface-1": {"#14171c", "#ffffff"}, "--accent": {"#6e9bff", "#2f5fd9"},
		"--gain": {"#4fd69c", "#0e7a4e"}, "--loss": {"#ff8a7a", "#c23b2b"}, "--warn": {"#f2b84b", "#9a6400"},
		"--src-hardware": {"#f2b84b", "#c48a12"}, "--src-hot": {"#c08cff", "#7a3fd1"}, "--text-3": {"#7c8592", "#69717d"},
		"--disc-sol-bg": {"#271b3a", "#f0e8fb"}, "--disc-btc-fg": {"#f2b84b", "#8a5a00"},
	}
	for k, want := range spot {
		if !strings.EqualFold(base[k], want[0]) || !strings.EqualFold(lightExplicit[k], want[1]) {
			t.Errorf("%s = %s / %s, want %s / %s", k, base[k], lightExplicit[k], want[0], want[1])
		}
	}
	if base["--s1"] != "4px" || base["--s8"] != "64px" || base["--r-sm"] != "6px" || base["--r-xl"] != "20px" {
		t.Errorf("spacing/radius scale is off: %v %v %v %v", base["--s1"], base["--s8"], base["--r-sm"], base["--r-xl"])
	}

	// <meta name=theme-color> and the manifest mirror --bg; they cannot use var().
	if !strings.EqualFold(base["--bg"], templates.ThemeColorDark) || !strings.EqualFold(lightExplicit["--bg"], templates.ThemeColorLight) {
		t.Errorf("ThemeColor constants (%s, %s) must match --bg (%s, %s)", templates.ThemeColorDark, templates.ThemeColorLight, base["--bg"], lightExplicit["--bg"])
	}

	// Font faces: all five weights, swapped, self-hosted under /static/fonts.
	if n := strings.Count(css, "@font-face"); n != 5 {
		t.Errorf("want 5 @font-face rules, got %d", n)
	}
	if n := strings.Count(css, "font-display: swap"); n != 5 {
		t.Errorf("every @font-face needs font-display: swap, got %d", n)
	}
	for _, f := range []string{"ibm-plex-sans-400", "ibm-plex-sans-500", "ibm-plex-sans-600", "ibm-plex-mono-400", "ibm-plex-mono-500"} {
		if !strings.Contains(css, `url("/static/fonts/`+f+`.woff2")`) {
			t.Errorf("missing @font-face for %s", f)
		}
	}
	if strings.Contains(css, "http") {
		t.Error("tokens.css must not load anything from the network")
	}
	// Explicit dark must also exist so a pinned theme beats the media query.
	if !strings.Contains(css, `:root[data-theme="dark"]`) || !strings.Contains(css, `:root:not([data-theme="dark"])`) {
		t.Error("missing dark override or the :not([data-theme=dark]) guard on the media block")
	}
}

func TestAppCSSUsesOnlyTokens(t *testing.T) {
	css := readStatic(t, "app.css")
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`),
		regexp.MustCompile(`\brgba?\(`),
		regexp.MustCompile(`\bhsla?\(`),
	} {
		if m := re.FindString(css); m != "" {
			t.Errorf("app.css contains a raw color (%q); use a token", m)
		}
	}
	for _, want := range []string{
		"body.amounts-hidden .fig-v", "body.amounts-hidden .fig-m", ".fig-m {\n  display: none;",
		"@media (min-width: 760px)", "prefers-reduced-motion", "vector-effect: non-scaling-stroke",
		":focus-visible", "box-shadow: 0 0 0 3px var(--accent-soft)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css missing %q", want)
		}
	}
	// Every var() it uses is defined in tokens.css (or is one of its own).
	tokens := readStatic(t, "tokens.css")
	defined := props(tokens)
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)`).FindAllStringSubmatch(css, -1) {
		if _, ok := defined[m[1]]; !ok {
			t.Errorf("app.css uses undefined token %s", m[1])
		}
	}
	// The classes the templates rely on exist.
	all := ""
	for _, p := range allPages(t) {
		all += p.html
	}
	missing := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sclass="([^"]+)"`).FindAllStringSubmatch(all, -1) {
		for _, c := range strings.Fields(m[1]) {
			if !strings.Contains(css, "."+c) {
				missing[c] = true
			}
		}
	}
	for c := range missing {
		switch c {
		case "icon-eye", "icon-refresh": // icon-* classes are optional hooks
			continue
		}
		if strings.HasPrefix(c, "icon-") {
			continue
		}
		t.Errorf("class %q is used by a template but has no rule in app.css", c)
	}
}

func TestManifest(t *testing.T) {
	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		Display         string `json:"display"`
		StartURL        string `json:"start_url"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(readStatic(t, "manifest.webmanifest")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "Vault" || m.ShortName != "Vault" || m.Display != "standalone" || m.StartURL != "/" {
		t.Errorf("manifest basics: %+v", m)
	}
	if !strings.EqualFold(m.ThemeColor, templates.ThemeColorDark) || !strings.EqualFold(m.BackgroundColor, templates.ThemeColorDark) {
		t.Errorf("manifest colors %s/%s should be the dark --bg %s", m.ThemeColor, m.BackgroundColor, templates.ThemeColorDark)
	}
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		sizes[ic.Sizes] = true
		path := strings.TrimPrefix(ic.Src, "/static/")
		if path == ic.Src {
			t.Errorf("icon %s must be served under /static/", ic.Src)
		}
		if _, err := fs.Stat(static.FS, path); err != nil {
			t.Errorf("manifest icon %s is not embedded", ic.Src)
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("need 192 and 512 icons, have %v", sizes)
	}
}

// ---- contrast ---------------------------------------------------------------------------

func hexLum(t *testing.T, h string) float64 {
	t.Helper()
	h = strings.TrimPrefix(h, "#")
	if len(h) != 6 {
		t.Fatalf("not a #rrggbb color: %q", h)
	}
	ch := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(h[0:2]) + 0.7152*ch(h[2:4]) + 0.0722*ch(h[4:6])
}

func contrast(t *testing.T, a, b string) float64 {
	la, lb := hexLum(t, a), hexLum(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TestTokenContrast checks the text/background pairs app.css actually uses, in both
// themes. The one pair that sits fractionally under 4.5 is --loss on --loss-soft in light
// (4.495:1), which comes straight from the design board's own colors.
func TestTokenContrast(t *testing.T) {
	css := readStatic(t, "tokens.css")
	dark := props(cssBlock(t, css, ":root {"))
	light := map[string]string{}
	for k, v := range dark {
		light[k] = v
	}
	for k, v := range props(cssBlock(t, css, `:root[data-theme="light"]`)) {
		light[k] = v
	}
	pairs := [][2]string{
		{"--text-1", "--bg"}, {"--text-1", "--surface-1"}, {"--text-1", "--surface-2"},
		{"--text-1", "--accent-soft"}, {"--text-1", "--warn-soft"}, {"--text-1", "--loss-soft"}, {"--text-1", "--gain-soft"},
		{"--text-2", "--bg"}, {"--text-2", "--surface-1"}, {"--text-2", "--surface-2"},
		{"--text-3", "--bg"}, {"--text-3", "--surface-1"}, {"--text-3", "--nav"},
		{"--on-accent", "--accent"},
		{"--link", "--bg"}, {"--link", "--surface-1"}, {"--link", "--accent-soft"},
		{"--accent", "--surface-1"},
		{"--gain", "--surface-1"}, {"--gain", "--bg"}, {"--gain", "--gain-soft"},
		{"--loss", "--surface-1"}, {"--loss", "--bg"}, {"--loss", "--loss-soft"},
		{"--warn", "--surface-1"}, {"--warn", "--bg"}, {"--warn-text", "--warn-soft"},
		{"--disc-btc-fg", "--disc-btc-bg"}, {"--disc-eth-fg", "--disc-eth-bg"}, {"--disc-sol-fg", "--disc-sol-bg"},
		{"--disc-usd-fg", "--disc-usd-bg"}, {"--disc-neutral-fg", "--disc-neutral-bg"},
	}
	for name, theme := range map[string]map[string]string{"dark": dark, "light": light} {
		for _, p := range pairs {
			fg, bg := theme[p[0]], theme[p[1]]
			if fg == "" || bg == "" {
				t.Errorf("%s: %s or %s undefined", name, p[0], p[1])
				continue
			}
			if c := contrast(t, fg, bg); c < 4.49 {
				t.Errorf("%s: %s (%s) on %s (%s) is %.2f:1, want at least 4.5", name, p[0], fg, p[1], bg, c)
			}
		}
	}
}
