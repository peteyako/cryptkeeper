package coinbase

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

const testKeyName = "organizations/0000-org/apiKeys/1111-key-a1b2"

func fastClient() *httpx.Client {
	return &httpx.Client{HTTP: &http.Client{Timeout: 5 * time.Second}, MaxRetries: 1, BaseDelay: time.Millisecond}
}

func genP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sec1PEM(t *testing.T, k *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func pkcs8PEM(t *testing.T, k any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func genEd25519(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(priv)
}

// ---------------------------------------------------------------------------------------------
// Key parsing
// ---------------------------------------------------------------------------------------------

func TestParseKeyFormats(t *testing.T) {
	k := genP256(t)
	sec1 := sec1PEM(t, k)
	cases := map[string]string{
		"SEC1 PEM":                 sec1,
		"PKCS8 PEM":                pkcs8PEM(t, k),
		"escaped newlines":         strings.ReplaceAll(sec1, "\n", `\n`),
		"CRLF":                     strings.ReplaceAll(sec1, "\n", "\r\n"),
		"surrounding whitespace":   "\n  " + sec1 + "  \n",
		"double quoted":            `"` + strings.ReplaceAll(sec1, "\n", `\n`) + `"`,
		"newlines collapsed":       strings.ReplaceAll(strings.TrimSpace(sec1), "\n", " "),
		"newlines removed in body": strings.Replace(strings.Replace(strings.TrimSpace(sec1), "-----\n", "----- ", 1), "\n-----END", " -----END", 1),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			pk, err := parseKey(in)
			if err != nil {
				t.Fatalf("parseKey: %v", err)
			}
			if pk.alg != "ES256" {
				t.Errorf("alg = %q", pk.alg)
			}
			got, ok := pk.signer.(*ecdsa.PrivateKey)
			if !ok || !got.Equal(k) {
				t.Error("parsed key differs from the original")
			}
		})
	}
}

func TestParseKeyEd25519(t *testing.T) {
	priv, b64 := genEd25519(t)
	for name, in := range map[string]string{
		"plain":      b64,
		"whitespace": "  " + b64[:20] + "\n" + b64[20:] + " \n",
		"quoted":     `"` + b64 + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			pk, err := parseKey(in)
			if err != nil {
				t.Fatal(err)
			}
			if pk.alg != "EdDSA" {
				t.Errorf("alg = %q", pk.alg)
			}
			got, ok := pk.signer.(ed25519.PrivateKey)
			if !ok || !got.Equal(priv) {
				t.Error("parsed key differs from the original")
			}
		})
	}
	// PKCS8-wrapped Ed25519 is accepted too.
	pk, err := parseKey(pkcs8PEM(t, priv))
	if err != nil || pk.alg != "EdDSA" {
		t.Fatalf("PKCS8 Ed25519: %v %+v", err, pk)
	}
}

func TestParseKeyRejects(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384DER, _ := x509.MarshalECPrivateKey(p384)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := genEd25519(t)
	mismatched := append(ed25519.PrivateKey{}, priv...)
	mismatched[40] ^= 0xff // corrupt the public half

	cases := map[string]string{
		"empty":            "",
		"garbage":          "not a key",
		"truncated PEM":    "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n",
		"P-384":            string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: p384DER})),
		"RSA":              pkcs8PEM(t, rsaKey),
		"short ed25519":    base64.StdEncoding.EncodeToString(priv[:32]),
		"mismatched ed255": base64.StdEncoding.EncodeToString(mismatched),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseKey(in); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	k := sec1PEM(t, genP256(t))
	c := fastClient()
	if _, err := New(Config{ID: "cb", PrivateKey: k}, c); err == nil {
		t.Error("expected error for missing key name")
	}
	if _, err := New(Config{ID: "cb", KeyName: testKeyName}, c); err == nil {
		t.Error("expected error for missing private key")
	}
	if _, err := New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: "nonsense"}, c); err == nil {
		t.Error("expected the key to be parsed eagerly")
	}
	s, err := New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: k}, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "cb" {
		t.Errorf("ID() = %q", s.ID())
	}
	if s.baseURL != "https://api.coinbase.com" || s.host != "api.coinbase.com" {
		t.Errorf("defaults: baseURL=%q host=%q", s.baseURL, s.host)
	}
	s, err = New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: k, BaseURL: "http://127.0.0.1:9999/"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.baseURL != "http://127.0.0.1:9999" || s.host != "127.0.0.1:9999" {
		t.Errorf("custom: baseURL=%q host=%q", s.baseURL, s.host)
	}
}

// ---------------------------------------------------------------------------------------------
// JWT
// ---------------------------------------------------------------------------------------------

var nonceRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func parseWith(t *testing.T, tok string, pub any, method string, now time.Time) *jwt.Token {
	t.Helper()
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{method}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("cdp"),
		jwt.WithSubject(testKeyName),
	)
	parsed, err := parser.Parse(tok, func(*jwt.Token) (any, error) { return pub, nil })
	if err != nil {
		t.Fatalf("JWT does not verify: %v", err)
	}
	return parsed
}

func TestBuildJWTES256(t *testing.T) {
	k := genP256(t)
	s, err := New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: sec1PEM(t, k)}, fastClient())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 123_000_000)
	tok, err := s.BuildJWT("get", "api.coinbase.com", "/api/v3/brokerage/accounts", now)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseWith(t, tok, &k.PublicKey, "ES256", now)

	h := parsed.Header
	if h["alg"] != "ES256" || h["typ"] != "JWT" || h["kid"] != testKeyName {
		t.Errorf("header = %v", h)
	}
	if n, _ := h["nonce"].(string); !nonceRE.MatchString(n) {
		t.Errorf("nonce %q is not 16 random bytes in hex", h["nonce"])
	}
	c := parsed.Claims.(jwt.MapClaims)
	if c["sub"] != testKeyName || c["iss"] != "cdp" {
		t.Errorf("claims = %v", c)
	}
	if c["nbf"] != float64(1_700_000_000) || c["exp"] != float64(1_700_000_120) {
		t.Errorf("nbf/exp = %v/%v", c["nbf"], c["exp"])
	}
	if c["uri"] != "GET api.coinbase.com/api/v3/brokerage/accounts" {
		t.Errorf("uri = %v", c["uri"])
	}
	if _, has := c["aud"]; has {
		t.Errorf("ES256 tokens follow the Coinbase App docs and carry no aud, got %v", c["aud"])
	}

	// Another key must not verify.
	if _, err := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now })).Parse(tok,
		func(*jwt.Token) (any, error) { return &genP256(t).PublicKey, nil }); err == nil {
		t.Error("token verified with an unrelated key")
	}
	// Expiry is enforced: 121 s later it is invalid.
	if _, err := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now.Add(121 * time.Second) })).Parse(tok,
		func(*jwt.Token) (any, error) { return &k.PublicKey, nil }); err == nil {
		t.Error("token still valid after exp")
	}
}

func TestBuildJWTFreshNoncePerCall(t *testing.T) {
	s, _ := New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: sec1PEM(t, genP256(t))}, fastClient())
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tok, err := s.BuildJWT("GET", "api.coinbase.com", "/v2/accounts", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		parsed, _, err := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
		if err != nil {
			t.Fatal(err)
		}
		n := parsed.Header["nonce"].(string)
		if seen[n] {
			t.Fatalf("nonce %q repeated", n)
		}
		seen[n] = true
	}
}

func TestBuildJWTURIIsPathOnly(t *testing.T) {
	k := genP256(t)
	s, _ := New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: sec1PEM(t, k)}, fastClient())
	now := time.Now()
	tok, err := s.BuildJWT("GET", "api.coinbase.com", "/v2/accounts?limit=100&starting_after=abc", now)
	if err != nil {
		t.Fatal(err)
	}
	c := parseWith(t, tok, &k.PublicKey, "ES256", now).Claims.(jwt.MapClaims)
	if c["uri"] != "GET api.coinbase.com/v2/accounts" {
		t.Errorf("uri = %v, want the path without the query string", c["uri"])
	}
}

func TestBuildJWTEd25519(t *testing.T) {
	priv, b64 := genEd25519(t)
	s, err := New(Config{ID: "cb", KeyName: testKeyName, PrivateKey: b64}, fastClient())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	tok, err := s.BuildJWT("GET", "api.coinbase.com", "/v2/accounts", now)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseWith(t, tok, priv.Public(), "EdDSA", now)
	h := parsed.Header
	if h["alg"] != "EdDSA" || h["typ"] != "JWT" || h["kid"] != testKeyName {
		t.Errorf("header = %v", h)
	}
	if n, _ := h["nonce"].(string); !nonceRE.MatchString(n) {
		t.Errorf("nonce = %q", h["nonce"])
	}
	c := parsed.Claims.(jwt.MapClaims)
	if c["uri"] != "GET api.coinbase.com/v2/accounts" || c["nbf"] != float64(1_700_000_000) || c["exp"] != float64(1_700_000_120) {
		t.Errorf("claims = %v", c)
	}
	// CDP's Ed25519 samples add aud=["cdp_service"].
	aud, _ := c["aud"].([]any)
	if len(aud) != 1 || aud[0] != "cdp_service" {
		t.Errorf("aud = %v, want [cdp_service]", c["aud"])
	}
}

// ---------------------------------------------------------------------------------------------
// Fake Coinbase server
// ---------------------------------------------------------------------------------------------

type seenReq struct {
	path, query, uriClaim string
}

type fakeCB struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []seenReq
	nonces map[string]bool
}

// newFakeCB serves routes behind a gate that verifies the Authorization JWT like Coinbase would:
// signature, iss/sub, nbf/exp window, kid, a never-before-seen nonce, and uri == "GET host path".
func newFakeCB(t *testing.T, pub any, method string, routes http.HandlerFunc) *fakeCB {
	t.Helper()
	f := &fakeCB{nonces: map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(format string, args ...any) {
			t.Errorf("%s %s: "+format, append([]any{r.Method, r.URL.RequestURI()}, args...)...)
			http.Error(w, `{"errors":[{"id":"authentication_error","message":"bad jwt"}]}`, http.StatusUnauthorized)
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			fail("missing Bearer token")
			return
		}
		parsed, err := jwt.NewParser(
			jwt.WithValidMethods([]string{method}),
			jwt.WithExpirationRequired(),
			jwt.WithIssuer("cdp"),
			jwt.WithSubject(testKeyName),
		).Parse(tok, func(*jwt.Token) (any, error) { return pub, nil })
		if err != nil {
			fail("invalid JWT: %v", err)
			return
		}
		claims := parsed.Claims.(jwt.MapClaims)
		wantURI := "GET " + r.Host + r.URL.Path
		if claims["uri"] != wantURI {
			fail("uri claim = %v, want %q", claims["uri"], wantURI)
			return
		}
		nbf, _ := claims["nbf"].(float64)
		exp, _ := claims["exp"].(float64)
		if exp-nbf != 120 {
			fail("exp-nbf = %v, want 120", exp-nbf)
			return
		}
		if parsed.Header["kid"] != testKeyName {
			fail("kid = %v", parsed.Header["kid"])
			return
		}
		nonce, _ := parsed.Header["nonce"].(string)
		f.mu.Lock()
		dup := f.nonces[nonce]
		f.nonces[nonce] = true
		f.reqs = append(f.reqs, seenReq{r.URL.Path, r.URL.RawQuery, wantURI})
		f.mu.Unlock()
		if !nonceRE.MatchString(nonce) || dup {
			fail("nonce %q malformed or reused", nonce)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		routes(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCB) requests() []seenReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenReq(nil), f.reqs...)
}

func newES256Source(t *testing.T, routes http.HandlerFunc) (*Source, *fakeCB) {
	t.Helper()
	k := genP256(t)
	f := newFakeCB(t, &k.PublicKey, "ES256", routes)
	s, err := New(Config{ID: "coinbase", Label: "Coinbase", KeyName: testKeyName, PrivateKey: sec1PEM(t, k), BaseURL: f.URL}, fastClient())
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

// ---------------------------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------------------------

const (
	atCursor = "p2&x=1" // needs URL escaping

	atPage1 = `{"accounts":[
		{"uuid":"u-btc","name":"BTC Wallet","currency":"BTC","available_balance":{"value":"1.0","currency":"BTC"},"hold":{"value":"0.5","currency":"BTC"},"type":"ACCOUNT_TYPE_CRYPTO"},
		{"uuid":"u-usd","name":"Cash (USD)","currency":"USD","available_balance":{"value":"100.10","currency":"USD"},"hold":{"value":"0.05","currency":"USD"},"type":"ACCOUNT_TYPE_FIAT"}
	],"has_next":true,"cursor":"` + atCursor + `","size":2}`

	atPage2 = `{"accounts":[
		{"uuid":"u-sol","name":"SOL Wallet","currency":"SOL","available_balance":{"value":"10","currency":"SOL"},"hold":{"value":"2","currency":"SOL"},"type":"ACCOUNT_TYPE_CRYPTO"},
		{"uuid":"u-eth","name":"ETH Wallet","currency":"ETH","available_balance":{"value":"0","currency":"ETH"},"hold":{"value":"0","currency":"ETH"},"type":"ACCOUNT_TYPE_CRYPTO"}
	],"has_next":false,"cursor":"","size":2}`

	v2Page1 = `{"pagination":{"limit":100,"next_uri":"/v2/accounts?limit=100&starting_after=v-btc"},"data":[
		{"id":"u-btc","name":"BTC Wallet","type":"wallet","currency":{"code":"BTC","name":"Bitcoin"},"balance":{"amount":"1.75000000","currency":"BTC"}},
		{"id":"v-btc","name":"My Vault","type":"vault","currency":{"code":"BTC","name":"Bitcoin"},"balance":{"amount":"4.00000000","currency":"BTC"}}
	]}`

	v2Page2 = `{"pagination":{"limit":100,"next_uri":null},"data":[
		{"id":"u-eth2","name":"ETH2 Wallet","type":"wallet","currency":{"code":"ETH2","name":"Ethereum 2"},"balance":{"amount":"3.5","currency":"ETH2"}},
		{"id":"u-eth","name":"ETH Wallet","type":"wallet","currency":{"code":"ETH","name":"Ethereum"},"balance":{"amount":"0.00000000","currency":"ETH"}},
		{"id":"u-atom","name":"ATOM Wallet","type":"wallet","currency":{"code":"ATOM","name":"Staked Cosmos"},"balance":{"amount":"5","currency":"ATOM"}},
		{"id":"u-dot","name":"DOT Wallet","type":"wallet","currency":{"code":"DOT-STAKED","name":"Polkadot"},"balance":{"amount":"2","currency":"DOT-STAKED"}},
		{"id":"u-cbeth","name":"cbETH Wallet","type":"wallet","currency":{"code":"CBETH","name":"Coinbase Wrapped Staked ETH"},"balance":{"amount":"0.5","currency":"CBETH"}},
		{"id":"f-eur","name":"EUR Wallet","type":"fiat","currency":{"code":"EUR","name":"Euro"},"balance":{"amount":"50.00","currency":"EUR"}}
	]}`
)

func routesFull(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v3/brokerage/accounts":
		if r.URL.Query().Get("cursor") == atCursor {
			fmt.Fprint(w, atPage2)
			return
		}
		fmt.Fprint(w, atPage1)
	case "/v2/accounts":
		if r.URL.Query().Get("starting_after") == "v-btc" {
			fmt.Fprint(w, v2Page2)
			return
		}
		fmt.Fprint(w, v2Page1)
	default:
		http.NotFound(w, r)
	}
}

type holdKey struct {
	sym    string
	staked bool
	vault  bool
}

func holdingsByKey(t *testing.T, hs []core.Holding) map[holdKey]core.Holding {
	t.Helper()
	out := map[holdKey]core.Holding{}
	for _, h := range hs {
		k := holdKey{h.Symbol, h.Staked, h.Vault}
		if _, dup := out[k]; dup {
			t.Errorf("duplicate holding for %+v", k)
		}
		if h.AssetKey != h.Symbol {
			t.Errorf("%+v: AssetKey %q != Symbol %q", k, h.AssetKey, h.Symbol)
		}
		if h.WalletID != "coinbase" {
			t.Errorf("%+v: WalletID = %q", k, h.WalletID)
		}
		out[k] = h
	}
	return out
}

func meta(res core.WalletResult, k string) (string, bool) {
	for _, kv := range res.Meta {
		if kv.K == k {
			return kv.V, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------------------------

func TestFetchPaginationAndPerRequestJWT(t *testing.T) {
	s, f := newES256Source(t, routesFull)
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}

	reqs := f.requests()
	host := strings.TrimPrefix(f.URL, "http://")
	want := []seenReq{
		{"/api/v3/brokerage/accounts", "limit=250", "GET " + host + "/api/v3/brokerage/accounts"},
		{"/api/v3/brokerage/accounts", "cursor=p2%26x%3D1&limit=250", "GET " + host + "/api/v3/brokerage/accounts"},
		{"/v2/accounts", "limit=100", "GET " + host + "/v2/accounts"},
		{"/v2/accounts", "limit=100&starting_after=v-btc", "GET " + host + "/v2/accounts"},
	}
	if len(reqs) != len(want) {
		t.Fatalf("got %d requests %+v, want %d", len(reqs), reqs, len(want))
	}
	for i := range want {
		if reqs[i].path != want[i].path || reqs[i].uriClaim != want[i].uriClaim {
			t.Errorf("request %d = %+v, want %+v", i, reqs[i], want[i])
		}
		if !sameQuery(reqs[i].query, want[i].query) {
			t.Errorf("request %d query = %q, want %q", i, reqs[i].query, want[i].query)
		}
	}
	// The gate already asserted a distinct, valid JWT per request (one nonce each).
	if len(f.nonces) != len(want) {
		t.Errorf("saw %d distinct nonces for %d requests", len(f.nonces), len(want))
	}
}

func sameQuery(a, b string) bool {
	split := func(q string) map[string]bool {
		m := map[string]bool{}
		for _, p := range strings.Split(q, "&") {
			m[p] = true
		}
		return m
	}
	am, bm := split(a), split(b)
	if len(am) != len(bm) {
		return false
	}
	for k := range am {
		if !bm[k] {
			return false
		}
	}
	return true
}

func TestFetchReconciliation(t *testing.T) {
	s, _ := newES256Source(t, routesFull)
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := holdingsByKey(t, res.Holdings)

	want := map[holdKey]string{
		{"BTC", false, false}:   "1.75", // overlap: v2 (1.75) wins over Advanced Trade (1.0 + 0.5 hold)
		{"BTC", false, true}:    "4",    // vault only in v2
		{"ETH", true, false}:    "3.5",  // ETH2 -> staked ETH
		{"SOL", false, false}:   "12",   // Advanced Trade only: available + hold
		{"USD", false, false}:   "100.15",
		{"EUR", false, false}:   "50",
		{"ATOM", true, false}:   "5",   // name contains "Staked"
		{"DOT", true, false}:    "2",   // code ends in -STAKED
		{"CBETH", false, false}: "0.5", // wrapped liquid token, not a staked position
	}
	for k, amt := range want {
		h, ok := got[k]
		if !ok {
			t.Errorf("missing holding %+v", k)
			continue
		}
		if h.AmountStr != amt {
			t.Errorf("%+v: AmountStr = %q, want %q", k, h.AmountStr, amt)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected holding %+v = %+v", k, got[k])
		}
	}
	if _, ok := got[holdKey{"ETH", false, false}]; ok {
		t.Error("zero ETH wallet should have been dropped")
	}
	if h := got[holdKey{"BTC", false, false}]; h.Amount != 1.75 || h.Name != "Bitcoin" {
		t.Errorf("BTC holding = %+v", h)
	}
	if h := got[holdKey{"BTC", false, true}]; h.Name != "Bitcoin" {
		t.Errorf("vault BTC name = %q", h.Name)
	}
	if h := got[holdKey{"ETH", true, false}]; h.Name != "Ethereum" {
		t.Errorf("staked ETH should reuse the base asset name, got %q", h.Name)
	}
	if h := got[holdKey{"SOL", false, false}]; h.Name != "" {
		t.Errorf("Advanced Trade carries no currency name, got %q", h.Name)
	}

	if v, _ := meta(res, "API key"); v != "••••a1b2" {
		t.Errorf("API key meta = %q", v)
	}
	// 9 non-zero accounts contribute: u-btc, v-btc, u-eth2, u-atom, u-dot, u-cbeth, f-eur, u-usd, u-sol.
	if v, _ := meta(res, "Accounts"); v != "9 (1 vault)" {
		t.Errorf("Accounts meta = %q", v)
	}
	if v, _ := meta(res, "Reconciliation"); v != "Advanced Trade only: 2 · v2 only: 6" {
		t.Errorf("Reconciliation meta = %q", v)
	}
	if v, ok := meta(res, "Balance differences"); !ok || !strings.HasPrefix(v, "1 ") {
		t.Errorf("Balance differences meta = %q (present=%v)", v, ok)
	}
	if res.WalletID != "coinbase" || res.AsOf.IsZero() {
		t.Errorf("WalletID=%q AsOf=%v", res.WalletID, res.AsOf)
	}
}

func TestFetchNoDifferencesMeta(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[{"uuid":"a","currency":"BTC","available_balance":{"value":"1"},"hold":{"value":"0.5"},"type":"ACCOUNT_TYPE_CRYPTO"}],"has_next":false}`)
		case "/v2/accounts":
			fmt.Fprint(w, `{"pagination":{"next_uri":null},"data":[{"id":"a","type":"wallet","currency":{"code":"BTC","name":"Bitcoin"},"balance":{"amount":"1.50000000"}}]}`)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta(res, "Balance differences"); ok {
		t.Error("equal balances must not report differences (1.5 == 1.50000000)")
	}
	if v, _ := meta(res, "Reconciliation"); v != "Advanced Trade only: 0 · v2 only: 0" {
		t.Errorf("Reconciliation = %q", v)
	}
	if v, _ := meta(res, "Accounts"); v != "1 (0 vault)" {
		t.Errorf("Accounts = %q", v)
	}
}

func TestFetchAdvancedTradeVaultType(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[{"uuid":"v1","currency":"BTC","available_balance":{"value":"2"},"hold":{"value":"0"},"type":"ACCOUNT_TYPE_VAULT"}],"has_next":false}`)
		case "/v2/accounts":
			fmt.Fprint(w, `{"pagination":{"next_uri":null},"data":[]}`)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 || !res.Holdings[0].Vault {
		t.Fatalf("holdings = %+v, want one vault holding", res.Holdings)
	}
}

func TestFetchV2CurrencyAsString(t *testing.T) {
	// Older CB-VERSIONs return currency as a bare code.
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[],"has_next":false}`)
		case "/v2/accounts":
			fmt.Fprint(w, `{"pagination":{"next_uri":null},"data":[{"id":"a","type":"wallet","currency":"BTC","balance":{"amount":"0.5","currency":"BTC"}}]}`)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 || res.Holdings[0].Symbol != "BTC" || res.Holdings[0].AmountStr != "0.5" {
		t.Fatalf("holdings = %+v", res.Holdings)
	}
}

func TestFetchSumsAcrossAccountsExactly(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[],"has_next":false}`)
		case "/v2/accounts":
			fmt.Fprint(w, `{"pagination":{"next_uri":null},"data":[
				{"id":"a","type":"wallet","currency":{"code":"BTC","name":"Bitcoin"},"balance":{"amount":"0.1"}},
				{"id":"b","type":"wallet","currency":{"code":"BTC","name":"Bitcoin"},"balance":{"amount":"0.2"}}]}`)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 || res.Holdings[0].AmountStr != "0.3" {
		t.Fatalf("holdings = %+v, want a single BTC holding of exactly 0.3", res.Holdings)
	}
}

func TestFetchV2DownKeepsAdvancedTrade(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, atPage2)
		case "/v2/accounts":
			http.Error(w, `{"errors":[{"id":"internal_server_error"}]}`, http.StatusInternalServerError)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("one endpoint working must not fail the fetch: %v", err)
	}
	if len(res.Holdings) != 1 || res.Holdings[0].Symbol != "SOL" || res.Holdings[0].AmountStr != "12" {
		t.Fatalf("holdings = %+v", res.Holdings)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "v2") {
		t.Fatalf("warnings = %v, want one mentioning v2", res.Warnings)
	}
	if v, _ := meta(res, "Reconciliation"); !strings.Contains(v, "v2 unavailable") {
		t.Errorf("Reconciliation = %q", v)
	}
}

func TestFetchAdvancedTradeDownKeepsV2(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			http.Error(w, `{"error":"PERMISSION_DENIED","message":"no view permission"}`, http.StatusForbidden)
		case "/v2/accounts":
			fmt.Fprint(w, v2Page2)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) == 0 {
		t.Fatal("expected the v2 holdings")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Advanced Trade") || !strings.Contains(res.Warnings[0], "no view permission") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if v, _ := meta(res, "Reconciliation"); !strings.Contains(v, "Advanced Trade unavailable") {
		t.Errorf("Reconciliation = %q", v)
	}
}

func TestFetchBothDownIsError(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusForbidden)
	})
	res, err := s.Fetch(context.Background())
	if err == nil {
		t.Fatalf("expected an error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "Advanced Trade") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("error should name both endpoints: %v", err)
	}
}

func TestFetchEd25519V2UnsupportedHint(t *testing.T) {
	priv, b64 := genEd25519(t)
	f := newFakeCB(t, priv.Public(), "EdDSA", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, atPage2)
		case "/v2/accounts":
			http.Error(w, `{"errors":[{"id":"authentication_error","message":"invalid signature"}]}`, http.StatusUnauthorized)
		}
	})
	s, err := New(Config{ID: "coinbase", KeyName: testKeyName, PrivateKey: b64, BaseURL: f.URL}, fastClient())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 {
		t.Fatalf("holdings = %+v", res.Holdings)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ECDSA") {
		t.Fatalf("warnings = %v, want a hint to use an ECDSA key", res.Warnings)
	}
}

func TestFetchStopsOnPaginationLoop(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[],"has_next":true,"cursor":"same"}`)
		case "/v2/accounts":
			fmt.Fprint(w, `{"pagination":{"next_uri":"/v2/accounts?starting_after=same"},"data":[]}`)
		}
	})
	_, err := s.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("err = %v, want a pagination loop error", err)
	}
}

func TestFetchNextURIStaysOnBaseURL(t *testing.T) {
	// An absolute next_uri must never send the bearer token to another host.
	var hit bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer evil.Close()
	s, f := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[],"has_next":false}`)
		case "/v2/accounts":
			if r.URL.Query().Get("starting_after") == "x" {
				fmt.Fprint(w, `{"pagination":{"next_uri":null},"data":[]}`)
				return
			}
			fmt.Fprintf(w, `{"pagination":{"next_uri":%q},"data":[]}`, evil.URL+"/v2/accounts?starting_after=x")
		}
	})
	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("followed next_uri to a foreign host")
	}
	if n := len(f.requests()); n != 3 {
		t.Errorf("got %d requests, want 3", n)
	}
}

func TestFetchBadAmountFailsThatEndpoint(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/brokerage/accounts":
			fmt.Fprint(w, `{"accounts":[{"uuid":"a","currency":"BTC","available_balance":{"value":"lots"},"type":"ACCOUNT_TYPE_CRYPTO"}],"has_next":false}`)
		case "/v2/accounts":
			fmt.Fprint(w, v2Page2)
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "lots") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestFetchHonoursContext(t *testing.T) {
	s, _ := newES256Source(t, routesFull)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := s.Fetch(ctx)
	if err == nil {
		t.Fatalf("expected an error from a cancelled context, got %+v", res)
	}
}

func TestSendsCBVersionAndBearer(t *testing.T) {
	var mu sync.Mutex
	var versions []string
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		versions = append(versions, r.Header.Get("CB-VERSION"))
		mu.Unlock()
		if r.URL.Path == "/v2/accounts" {
			fmt.Fprint(w, `{"pagination":{"next_uri":null},"data":[]}`)
			return
		}
		fmt.Fprint(w, `{"accounts":[],"has_next":false}`)
	})
	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(versions) != 2 {
		t.Fatalf("versions = %v", versions)
	}
	if versions[1] == "" {
		t.Error("v2 requests should pin CB-VERSION")
	}
}

// ---------------------------------------------------------------------------------------------
// Currency normalization
// ---------------------------------------------------------------------------------------------

func TestNormalizeCurrency(t *testing.T) {
	cases := []struct {
		code, name string
		symbol     string
		staked     bool
	}{
		{"BTC", "Bitcoin", "BTC", false},
		{"btc", "", "BTC", false},
		{"ETH", "Ethereum", "ETH", false},
		{"ETH2", "", "ETH", true},
		{"eth2", "Ethereum 2", "ETH", true},
		{"SOL-STAKED", "", "SOL", true},
		{"sol-staked", "Solana", "SOL", true},
		{"ATOM", "Staked Cosmos", "ATOM", true},
		{"ADA", "Cardano (staked)", "ADA", true},
		{"XYZ", "Unstaked XYZ", "XYZ", false},
		{"CBETH", "Coinbase Wrapped Staked ETH", "CBETH", false},
		{"USD", "United States Dollar", "USD", false},
		{"STAKED", "", "STAKED", false}, // a coin literally called STAKED is not a staked position
	}
	for _, tc := range cases {
		sym, staked := normalizeCurrency(tc.code, tc.name)
		if sym != tc.symbol || staked != tc.staked {
			t.Errorf("normalizeCurrency(%q, %q) = (%q, %v), want (%q, %v)", tc.code, tc.name, sym, staked, tc.symbol, tc.staked)
		}
	}
}

func TestSourceImplementsCoreSource(t *testing.T) {
	var _ core.Source = (*Source)(nil)
}
