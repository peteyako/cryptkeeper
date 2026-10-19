package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"cryptkeeper/internal/httpx"
)

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		amount   string
		decimals int
		want     string
	}{
		{"0", 6, "0"},
		{"1", 9, "0.000000001"},
		{"1500000", 6, "1.5"},
		{"1000000", 6, "1"},
		{"42", 0, "42"},
		{"123456789012345678901", 18, "123.456789012345678901"},
		{"18446744073709551615", 9, "18446744073.709551615"},
		{"100", 2, "1"},
		{"5", 2, "0.05"},
		{"3750000000", 9, "3.75"},
	}
	for _, c := range cases {
		n, ok := new(big.Int).SetString(c.amount, 10)
		if !ok {
			t.Fatal(c.amount)
		}
		if got := formatAmount(n, c.decimals); got != c.want {
			t.Errorf("formatAmount(%s, %d) = %q, want %q", c.amount, c.decimals, got, c.want)
		}
	}
}

func TestShortMint(t *testing.T) {
	if got := shortKey("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"); got != "EPjF…Dt1v" {
		t.Errorf("shortKey = %q", got)
	}
	if got := shortKey("abc"); got != "abc" {
		t.Errorf("shortKey(short) = %q", got)
	}
}

func TestCleanText(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"  USD Coin\x00\x00\x00 ", 64, "USD Coin"},
		{"bad\x01\x07name\n", 64, "badname"},
		{"abcdefghij", 4, "abcd"},
		{"\xff\xfeok", 64, "ok"},
		{"日本語トークン", 3, "日本語"},
	}
	for _, c := range cases {
		if got := cleanText(c.in, c.max); got != c.want {
			t.Errorf("cleanText(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// serveFiles returns a server answering GET /jup and POST (JSON-RPC) with canned files,
// keyed by the RPC address being asked about.
func fixtureResolver(t *testing.T, jupFile string, rpcFiles map[string]string) *resolver {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jup" {
			w.Write(read(jupFile))
			return
		}
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var addr string
		json.Unmarshal(req.Params[0], &addr)
		if f, ok := rpcFiles[addr]; ok {
			w.Write(read(f))
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","result":{"context":{"slot":1},"value":null},"id":1}`))
	}))
	t.Cleanup(srv.Close)
	hc := fastClient(srv)
	return &resolver{rpc: newRPCClient(srv.URL, hc), http: hc, jupURL: srv.URL + "/jup", cache: NewMemoryMetaCache()}
}

func TestJupiterLiveFixture(t *testing.T) {
	const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	r := fixtureResolver(t, "jupiter_usdc.json", nil)
	got, err := r.jupiter(context.Background(), []string{usdc})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got[usdc]
	if !ok {
		t.Fatalf("USDC missing from %v", got)
	}
	if m.Symbol != "USDC" || m.Name != "USD Coin" || m.Decimals != 6 || !m.Verified || m.Source != "jupiter" || m.Mint != usdc {
		t.Errorf("meta = %+v", m)
	}
	if !strings.HasSuffix(m.LogoURL, "/logo.png") {
		t.Errorf("LogoURL = %q", m.LogoURL)
	}
}

func TestJupiterLiveNotFound(t *testing.T) {
	r := fixtureResolver(t, "jupiter_notfound.json", nil)
	got, err := r.jupiter(context.Background(), []string{"Fa1se1111111111111111111111111111111111111Z"})
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestJupiterIgnoresEntriesForOtherMints(t *testing.T) {
	// A search endpoint may return fuzzy matches; only the exact id counts.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"OtherMint","symbol":"USDC","name":"Imposter","decimals":6,"isVerified":true}]`))
	}))
	defer srv.Close()
	hc := fastClient(srv)
	r := &resolver{http: hc, jupURL: srv.URL, cache: NewMemoryMetaCache()}
	got, err := r.jupiter(context.Background(), []string{"WantedMint"})
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestToken2022LiveFixture(t *testing.T) {
	const pyusd = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	r := fixtureResolver(t, "", map[string]string{pyusd: "token2022_pyusd_mint.json"})
	m, ok, err := r.token2022(context.Background(), pyusd)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if m.Name != "PayPal USD" || m.Symbol != "PYUSD" || m.Source != "token2022" || m.Decimals != 6 {
		t.Errorf("meta = %+v", m)
	}
}

func TestToken2022MintWithoutMetadataExtension(t *testing.T) {
	// The live USDC mint is a plain (non-2022) mint: no extensions at all.
	r := fixtureResolver(t, "", nil)
	if _, ok, err := r.token2022(context.Background(), "SomeMint"); ok || err != nil {
		t.Errorf("ok=%v err=%v (account missing should be a clean not-found)", ok, err)
	}
}

func TestMetaplexLiveFixture(t *testing.T) {
	const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	r := fixtureResolver(t, "", map[string]string{"5x38Kp4hvdomTCnCrAny4UtMUt5rQBdB6px2K1Ui45Wq": "metaplex_usdc_account.json"})
	m, ok, err := r.metaplex(context.Background(), usdc)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if m.Name != "USD Coin" || m.Symbol != "USDC" || m.Source != "metaplex" || m.Mint != usdc {
		t.Errorf("meta = %+v", m)
	}
}

func TestMetaplexAccountForDifferentMintIsRejected(t *testing.T) {
	// Defence in depth: the decoded mint field must match the mint we asked about.
	// Ask for PYUSD but serve the USDC metadata account at PYUSD's PDA.
	const pyusd = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	pda, err := metadataPDA(pyusd)
	if err != nil {
		t.Fatal(err)
	}
	r := fixtureResolver(t, "", map[string]string{pda: "metaplex_usdc_account.json"})
	if _, ok, _ := r.metaplex(context.Background(), pyusd); ok {
		t.Error("metadata for a different mint must not be accepted")
	}
}

func TestResolverBoundsWorkForSpamWallets(t *testing.T) {
	var jupCalls, rpcCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tag"):
			// Verified list knows exactly one of the mints.
			fmt.Fprint(w, `[{"id":"Verified1111","symbol":"VER","name":"Verified","decimals":6,"isVerified":true}]`)
		case strings.HasSuffix(r.URL.Path, "/search"):
			jupCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests) // rate limited
		default:
			rpcCalls.Add(1)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"value":null}}`)
		}
	}))
	defer srv.Close()
	hc := httpx.New()
	hc.MaxRetries = 0
	r := &resolver{rpc: newRPCClient(srv.URL, hc), http: hc, jupURL: srv.URL + "/tokens/v2/search", cache: NewMemoryMetaCache()}
	var mbs []*mintBalance
	mbs = append(mbs, &mintBalance{Mint: "Verified1111", Amount: big.NewInt(1), Decimals: 6})
	for i := range 500 {
		mbs = append(mbs, &mintBalance{Mint: fmt.Sprintf("Spam%04dxxxxxxxxxxxxxxxxxxxxxxxxxxx", i), Amount: big.NewInt(1)})
	}
	out := r.resolveAll(context.Background(), mbs)
	if got := out["Verified1111"]; got.meta.Symbol != "VER" || got.fallback {
		t.Fatalf("verified list not used: %+v", got)
	}
	if jupCalls.Load() != 1 {
		t.Fatalf("Jupiter search should stop after the first 429, made %d calls", jupCalls.Load())
	}
	if n := rpcCalls.Load(); n > maxOnChainLookups {
		t.Fatalf("on-chain lookups %d exceed the cap %d", n, maxOnChainLookups)
	}
	if len(out) != len(mbs) {
		t.Fatalf("every mint needs a resolution, got %d of %d", len(out), len(mbs))
	}
}
