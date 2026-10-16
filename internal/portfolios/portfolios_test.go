package portfolios

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

const jupMint = "JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN"

func mustParse(t *testing.T, raw string) File {
	t.Helper()
	f, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, raw)
	}
	return f
}

func TestParseExampleFile(t *testing.T) {
	raw, err := os.ReadFile("testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := File{Version: 1, Portfolios: []Portfolio{
		{ID: "long-term", Name: "Long-term", Wallets: AllSelector(),
			Assets: Selector{IDs: []string{"BTC", "SOL", "spl:J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn"}},
			Watch:  []string{"ETH", "LINK"}},
		{ID: "kids", Name: "Kids' wallets", Wallets: Selector{IDs: []string{"kids-phantom", "kids-cash"}}, Assets: AllSelector()},
		{ID: "memes", Name: "Memecoins", Wallets: AllSelector(), Assets: Selector{IDs: []string{"BONK", "WIF"}}},
		{ID: "watch", Name: "Watchlist", Wallets: Selector{}, Assets: AllSelector(), Watch: []string{"ETH", "AVAX"}},
	}}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got  %#v\nwant %#v", f, want)
	}
	if !f.Portfolios[3].Wallets.IsNone() {
		t.Fatal("explicit [] must be none")
	}
}

func TestSelectorDecoding(t *testing.T) {
	tests := []struct {
		name, yaml string
		wallets    Selector
		assets     Selector
		wantErr    string
	}{
		{name: "all scalar", yaml: "wallets: all\nassets: all", wallets: AllSelector(), assets: AllSelector()},
		{name: "all case-insensitive", yaml: "wallets: ALL\nassets: All", wallets: AllSelector(), assets: AllSelector()},
		{name: "quoted all", yaml: "wallets: \"all\"\nassets: 'all'", wallets: AllSelector(), assets: AllSelector()},
		{name: "list", yaml: "wallets: [a, b]\nassets: [btc]", wallets: Selector{IDs: []string{"a", "b"}}, assets: Selector{IDs: []string{"BTC"}}},
		{name: "block list", yaml: "wallets:\n  - a\n  - b\nassets: all", wallets: Selector{IDs: []string{"a", "b"}}, assets: AllSelector()},
		{name: "omitted is all", yaml: "", wallets: AllSelector(), assets: AllSelector()},
		{name: "omitted wallets only", yaml: "assets: [BTC]", wallets: AllSelector(), assets: Selector{IDs: []string{"BTC"}}},
		{name: "explicit empty is none", yaml: "wallets: []\nassets: []", wallets: Selector{}, assets: Selector{}},
		{name: "numeric-looking ids stay strings", yaml: "wallets: [123, true]\nassets: [1inch]", wallets: Selector{IDs: []string{"123", "true"}}, assets: Selector{IDs: []string{"1INCH"}}},
		{name: "null is an error", yaml: "wallets:", wantErr: "line"},
		{name: "other scalar is an error", yaml: "wallets: everything", wantErr: "all"},
		{name: "mapping is an error", yaml: "assets: {a: b}", wantErr: "all"},
		{name: "nested list is an error", yaml: "assets: [[BTC]]", wantErr: "line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := "portfolios:\n  - id: x\n    name: X\n"
			for line := range strings.SplitSeq(tc.yaml, "\n") {
				if line != "" {
					raw += "    " + line + "\n"
				}
			}
			f, err := Parse([]byte(raw))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := f.Portfolios[0]
			if !reflect.DeepEqual(p.Wallets, tc.wallets) || !reflect.DeepEqual(p.Assets, tc.assets) {
				t.Fatalf("wallets %#v assets %#v, want %#v %#v", p.Wallets, p.Assets, tc.wallets, tc.assets)
			}
		})
	}
}

func TestSelectorHas(t *testing.T) {
	all := AllSelector()
	none := Selector{}
	some := Selector{IDs: []string{"a", "b"}}
	if !all.Has("anything") || all.IsNone() {
		t.Fatal("all")
	}
	if none.Has("a") || !none.IsNone() {
		t.Fatal("none")
	}
	if !some.Has("a") || some.Has("c") || some.IsNone() {
		t.Fatal("some")
	}
}

func TestEncodeRoundTripAndStability(t *testing.T) {
	f := File{Version: 1, Portfolios: []Portfolio{
		{ID: "a", Name: "All of it", Wallets: AllSelector(), Assets: AllSelector()},
		{ID: "b", Name: "Kids' wallets: \"x\"", Wallets: Selector{IDs: []string{"kids-phantom", "kids-cash"}}, Assets: Selector{IDs: []string{"BTC", "spl:" + jupMint}}, Watch: []string{"ETH", "LINK"}},
		{ID: "c", Name: "Watch only", Wallets: Selector{}, Assets: AllSelector(), Watch: []string{"AVAX"}},
		{ID: "d", Name: "Tricky ids", Wallets: Selector{IDs: []string{"true", "123", "null"}}, Assets: Selector{}},
	}}
	raw, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip differs\ngot  %#v\nwant %#v\n%s", got, f, raw)
	}
	raw2, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(raw2) {
		t.Fatalf("re-encode is not stable:\n%s\nvs\n%s", raw, raw2)
	}
}

func TestEncodeShape(t *testing.T) {
	f := File{Portfolios: []Portfolio{
		{ID: "mix", Name: "Mix", Wallets: AllSelector(), Assets: Selector{IDs: []string{"BTC", "spl:" + jupMint}}, Watch: []string{"ETH"}},
		{ID: "none", Name: "None", Wallets: Selector{}, Assets: AllSelector()},
	}}
	raw, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `version: 1
portfolios:
  - id: mix
    name: Mix
    wallets: all
    assets: [BTC, 'spl:` + jupMint + `']
    watch: [ETH]
  - id: none
    name: None
    wallets: []
    assets: all
`
	if string(raw) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", raw, want)
	}
}

func TestEncodeEmptyFile(t *testing.T) {
	raw, err := File{}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "version: 1\nportfolios: []\n" {
		t.Fatalf("got %q", raw)
	}
	f, err := Parse(raw)
	if err != nil || f.Version != 1 || len(f.Portfolios) != 0 {
		t.Fatalf("%v %#v", err, f)
	}
}

func TestParseEmptyInputIsEmptyFile(t *testing.T) {
	for _, in := range []string{"", "\n", "# only a comment\n"} {
		f, err := Parse([]byte(in))
		if err != nil || f.Version != 1 || len(f.Portfolios) != 0 {
			t.Fatalf("%q: %v %#v", in, err, f)
		}
	}
}

func TestParseStrictRejectsTypos(t *testing.T) {
	raw, err := os.ReadFile("testdata/typo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(raw)
	if err == nil || !strings.Contains(err.Error(), "line 5") || !strings.Contains(err.Error(), "walets") {
		t.Fatalf("want a line-numbered error naming the typo, got %v", err)
	}
	if strings.Contains(err.Error(), "rawPortfolio") {
		t.Fatalf("internal type name leaked: %v", err)
	}
	// An unknown top-level key too.
	_, err = Parse([]byte("version: 1\nportfolio: []\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want line 2 error, got %v", err)
	}
	// Garbage is not a file.
	if _, err := Parse([]byte("portfolios: [unclosed")); err == nil {
		t.Fatal("want syntax error")
	}
}

func TestValidate(t *testing.T) {
	ok := func(id string) Portfolio {
		return Portfolio{ID: id, Name: "N", Wallets: AllSelector(), Assets: AllSelector()}
	}
	long := strings.Repeat("a", 41)
	tests := []struct {
		name    string
		f       File
		wantErr string
	}{
		{"valid", File{Version: 1, Portfolios: []Portfolio{ok("a"), ok("b-2"), ok("0x")}}, ""},
		{"max id length", File{Version: 1, Portfolios: []Portfolio{ok(strings.Repeat("a", 40))}}, ""},
		{"id too long", File{Version: 1, Portfolios: []Portfolio{ok(long)}}, "id"},
		{"empty id", File{Version: 1, Portfolios: []Portfolio{ok("")}}, "id"},
		{"uppercase id", File{Version: 1, Portfolios: []Portfolio{ok("Long")}}, "id"},
		{"space in id", File{Version: 1, Portfolios: []Portfolio{ok("a b")}}, "id"},
		{"leading dash", File{Version: 1, Portfolios: []Portfolio{ok("-a")}}, "id"},
		{"underscore", File{Version: 1, Portfolios: []Portfolio{ok("a_b")}}, "id"},
		{"duplicate id", File{Version: 1, Portfolios: []Portfolio{ok("a"), ok("b"), ok("a")}}, "duplicate"},
		{"reserved all", File{Version: 1, Portfolios: []Portfolio{ok("all")}}, "reserved"},
		{"empty name", File{Version: 1, Portfolios: []Portfolio{{ID: "a", Name: "  ", Wallets: AllSelector(), Assets: AllSelector()}}}, "name"},
		{"name too long", File{Version: 1, Portfolios: []Portfolio{{ID: "a", Name: strings.Repeat("x", 61)}}}, "name"},
		{"name exactly 60 runes", File{Version: 1, Portfolios: []Portfolio{{ID: "a", Name: strings.Repeat("é", 60)}}}, ""},
		{"bad version", File{Version: 2, Portfolios: []Portfolio{ok("a")}}, "version"},
		{"blank wallet entry", File{Version: 1, Portfolios: []Portfolio{{ID: "a", Name: "A", Wallets: Selector{IDs: []string{"x", ""}}}}}, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseValidates(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate": "portfolios:\n  - {id: a, name: A}\n  - {id: a, name: B}\n",
		"bad id":    "portfolios:\n  - {id: Bad_ID, name: A}\n",
		"reserved":  "portfolios:\n  - {id: all, name: A}\n",
		"no name":   "portfolios:\n  - {id: a}\n",
		"version":   "version: 7\nportfolios: []\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestParseNormalizes(t *testing.T) {
	raw := `version: 1
portfolios:
  - id: a
    name: "  Spacey  "
    wallets: [w1, " w2 ", w1, ""]
    assets: [btc, " eth ", BTC, "spl:` + jupMint + `", "SPL:` + jupMint + `", "spl:` + jupMint + `"]
    watch: [link, LINK, avax, ""]
`
	f := mustParse(t, raw)
	p := f.Portfolios[0]
	if p.Name != "Spacey" {
		t.Errorf("name %q", p.Name)
	}
	if want := []string{"w1", "w2"}; !reflect.DeepEqual(p.Wallets.IDs, want) {
		t.Errorf("wallets %v want %v", p.Wallets.IDs, want)
	}
	if want := []string{"BTC", "ETH", "spl:" + jupMint}; !reflect.DeepEqual(p.Assets.IDs, want) {
		t.Errorf("assets %v want %v", p.Assets.IDs, want)
	}
	if want := []string{"LINK", "AVAX"}; !reflect.DeepEqual(p.Watch, want) {
		t.Errorf("watch %v want %v", p.Watch, want)
	}
	if strings.Contains(strings.Join(p.Assets.IDs, ","), strings.ToUpper(jupMint)) {
		t.Error("mint case was not preserved")
	}
}

func TestSlug(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Kids' wallets", "kids-wallets"},
		{"Long-term", "long-term"},
		{"  Hello,   World!!  ", "hello-world"},
		{"BTC & ETH", "btc-eth"},
		{"already-slugged-1", "already-slugged-1"},
		{"", "portfolio"},
		{"!!!", "portfolio"},
		{"日本語", "portfolio"},
		{"all", "all-1"},
		{"All", "all-1"},
		{"Café Fund", "caf-fund"},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
		{strings.Repeat("ab ", 20), strings.TrimRight(strings.Repeat("ab-", 20)[:40], "-")},
	}
	for _, tc := range tests {
		got := Slug(tc.in)
		if got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
		f := File{Version: 1, Portfolios: []Portfolio{{ID: got, Name: "x"}}}
		if err := f.Validate(); err != nil {
			t.Errorf("Slug(%q) = %q is not a valid id: %v", tc.in, got, err)
		}
	}
}
