package kraken

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
)

// The signature example from Kraken's Spot REST authentication guide
// (https://docs.kraken.com/api/docs/guides/spot-rest-auth/).
const (
	docSecret    = "kQH5HW/8p1uGOVjbgWA7FunAmGO8lsSUXNsu3eow76sz84Q18fWxnyRzBHCd3pd5nE9qa99HAZtuZuj6F1huXg=="
	docPath      = "/0/private/AddOrder"
	docNonce     = "1616492376594"
	docPostData  = "nonce=1616492376594&ordertype=limit&pair=XBTUSD&price=37500&type=buy&volume=1.25"
	docSignature = "4/dpxb3iT4tp/ZCVEwSnEsLxx0bqyhLpdfOpc6fn7OR8+UClSV5n9E6aSS8MPtnRfp32bAb0nmbRn6H8ndwLUQ=="
)

func TestSignDocsVector(t *testing.T) {
	got, err := Sign(docPath, docNonce, docPostData, docSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got != docSignature {
		t.Fatalf("signature mismatch\n got  %s\n want %s", got, docSignature)
	}
}

func TestSignRejectsBadSecret(t *testing.T) {
	if _, err := Sign(docPath, docNonce, docPostData, "not base64!!"); err == nil {
		t.Fatal("expected error for a secret that is not base64")
	}
}

func TestNormalizeAsset(t *testing.T) {
	cases := []struct {
		in     string
		symbol string
		staked bool
	}{
		{"XXBT", "BTC", false},
		{"XBT", "BTC", false},
		{"XBT.M", "BTC", true},
		{"XETH", "ETH", false},
		{"ETH", "ETH", false},
		{"ETH2", "ETH", true},
		{"ETH2.S", "ETH", true},
		{"DOT", "DOT", false},
		{"DOT.S", "DOT", true},
		{"DOT.P", "DOT", true},
		{"DOT28.S", "DOT", true},
		{"KSM07.S", "KSM", true},
		{"USDT.F", "USDT", true},
		{"USDC.B", "USDC", true},
		{"USDC", "USDC", false},
		{"USD.M", "USD", true},
		{"USD.HOLD", "USD", false},
		{"EUR.HOLD", "EUR", false},
		{"ZUSD", "USD", false},
		{"ZEUR", "EUR", false},
		{"ZGBP", "GBP", false},
		{"ZCAD", "CAD", false},
		{"ZJPY", "JPY", false},
		{"ZAUD", "AUD", false},
		{"ZCHF", "CHF", false},
		{"XXDG", "DOGE", false},
		{"XDG", "DOGE", false},
		{"XLTC", "LTC", false},
		{"XXRP", "XRP", false},
		{"XXLM", "XLM", false},
		{"XXMR", "XMR", false},
		{"XZEC", "ZEC", false},
		{"XETC", "ETC", false},
		{"XREP", "REP", false},
		{"XMLN", "MLN", false},
		// Plain codes that merely start with X or Z must not be touched.
		{"XTZ", "XTZ", false},
		{"ZRX", "ZRX", false},
		{"XRP", "XRP", false},
		{"1INCH", "1INCH", false},
		// Case is significant (tokenized stocks use a lowercase x).
		{"AAPLx", "AAPLx", false},
		{"  XXBT ", "BTC", false},
	}
	for _, tc := range cases {
		sym, staked := NormalizeAsset(tc.in)
		if sym != tc.symbol || staked != tc.staked {
			t.Errorf("NormalizeAsset(%q) = (%q, %v), want (%q, %v)", tc.in, sym, staked, tc.symbol, tc.staked)
		}
	}
}

func fastClient() *httpx.Client {
	return &httpx.Client{HTTP: &http.Client{Timeout: 5 * time.Second}, MaxRetries: 2, BaseDelay: time.Millisecond}
}

var keySeq atomic.Int64

// uniqueKey returns an API key nobody else in this process has used. The nonce counter is shared
// per API key for the whole process, so tests that assert exact nonce values (or run under
// -count=N) must not reuse a key.
func uniqueKey(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), keySeq.Add(1))
}

func newTestSource(t *testing.T, apiKey, baseURL string) *Source {
	t.Helper()
	s, err := New(Config{ID: "kraken", Label: "Kraken", APIKey: apiKey, APISecret: docSecret, BaseURL: baseURL}, fastClient())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewValidation(t *testing.T) {
	c := fastClient()
	if _, err := New(Config{ID: "k", APISecret: docSecret}, c); err == nil {
		t.Error("expected error for missing API key")
	}
	if _, err := New(Config{ID: "k", APIKey: "key"}, c); err == nil {
		t.Error("expected error for missing API secret")
	}
	if _, err := New(Config{ID: "k", APIKey: "key", APISecret: "%%%"}, c); err == nil {
		t.Error("expected error for a secret that is not base64")
	}
	s, err := New(Config{ID: "k", APIKey: "key", APISecret: docSecret}, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "k" {
		t.Errorf("ID() = %q", s.ID())
	}
	if s.baseURL != "https://api.kraken.com" {
		t.Errorf("default base URL = %q", s.baseURL)
	}
	// Trailing slash on a custom base URL is tolerated.
	s, err = New(Config{ID: "k", APIKey: "key", APISecret: docSecret, BaseURL: "http://localhost:1/"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.baseURL != "http://localhost:1" {
		t.Errorf("base URL = %q", s.baseURL)
	}
}

func TestNonceStrictlyIncreasingConcurrent(t *testing.T) {
	s := newTestSource(t, uniqueKey("nonce-concurrent"), "http://unused")
	// A frozen clock forces the "last+1" branch for every call.
	frozen := time.UnixMilli(1_700_000_000_000)
	s.now = func() time.Time { return frozen }

	const goroutines, perG = 32, 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[int64]bool, goroutines*perG)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev := int64(0)
			for i := 0; i < perG; i++ {
				n, err := strconv.ParseInt(s.nextNonce(), 10, 64)
				if err != nil {
					t.Error(err)
					return
				}
				if n <= prev {
					t.Errorf("nonce went backwards within a goroutine: %d after %d", n, prev)
					return
				}
				prev = n
				mu.Lock()
				if seen[n] {
					t.Errorf("duplicate nonce %d", n)
				}
				seen[n] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*perG {
		t.Fatalf("got %d unique nonces, want %d", len(seen), goroutines*perG)
	}
}

func TestNonceFollowsClockAndNeverRegresses(t *testing.T) {
	s := newTestSource(t, uniqueKey("nonce-clock"), "http://unused")
	now := time.UnixMilli(5_000)
	s.now = func() time.Time { return now }
	if got := s.nextNonce(); got != "5000" {
		t.Fatalf("first nonce = %s, want 5000", got)
	}
	if got := s.nextNonce(); got != "5001" {
		t.Fatalf("same-millisecond nonce = %s, want 5001", got)
	}
	now = time.UnixMilli(1_000) // clock stepped backwards
	if got := s.nextNonce(); got != "5002" {
		t.Fatalf("nonce after clock regression = %s, want 5002", got)
	}
	now = time.UnixMilli(9_000)
	if got := s.nextNonce(); got != "9000" {
		t.Fatalf("nonce after clock catches up = %s, want 9000", got)
	}
}

func TestNonceSharedAcrossSourcesWithSameKey(t *testing.T) {
	key := uniqueKey("nonce-shared")
	a := newTestSource(t, key, "http://unused")
	b := newTestSource(t, key, "http://unused")
	frozen := time.UnixMilli(7_000)
	a.now = func() time.Time { return frozen }
	b.now = func() time.Time { return frozen }
	n1, _ := strconv.ParseInt(a.nextNonce(), 10, 64)
	n2, _ := strconv.ParseInt(b.nextNonce(), 10, 64)
	if n2 <= n1 {
		t.Fatalf("a second Source for the same API key reused a nonce: %d then %d", n1, n2)
	}
}

type recorded struct {
	method, path, contentType, apiKey, apiSign, body string
}

func balanceServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recorded{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("API-Key"), r.Header.Get("API-Sign"), string(b)})
		mu.Unlock()
		handler(w, r, int(count.Add(1)))
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestFetchSignsRequest(t *testing.T) {
	srv, reqs := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprint(w, `{"error":[],"result":{"XXBT":"1.0"}}`)
	})
	s := newTestSource(t, "AAAA-key-3f9a", srv.URL)
	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(*reqs))
	}
	r := (*reqs)[0]
	if r.method != http.MethodPost || r.path != "/0/private/Balance" {
		t.Errorf("request = %s %s", r.method, r.path)
	}
	if !strings.HasPrefix(r.contentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q", r.contentType)
	}
	if r.apiKey != "AAAA-key-3f9a" {
		t.Errorf("API-Key = %q", r.apiKey)
	}
	form, err := url.ParseQuery(r.body)
	if err != nil {
		t.Fatal(err)
	}
	nonce := form.Get("nonce")
	if _, err := strconv.ParseUint(nonce, 10, 64); err != nil {
		t.Fatalf("nonce %q is not an unsigned integer", nonce)
	}
	if r.body != "nonce="+nonce {
		t.Errorf("body = %q, want only the nonce", r.body)
	}
	want, err := Sign("/0/private/Balance", nonce, r.body, docSecret)
	if err != nil {
		t.Fatal(err)
	}
	if r.apiSign != want {
		t.Errorf("API-Sign = %q, want %q", r.apiSign, want)
	}
	if _, err := base64.StdEncoding.DecodeString(r.apiSign); err != nil {
		t.Errorf("API-Sign is not base64: %v", err)
	}
}

func TestFetchMergesAndDropsZeros(t *testing.T) {
	srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprint(w, `{"error":[],"result":{
			"XXBT":"1.5000000000",
			"XBT.M":"0.2500000000",
			"XETH":"0.0000000000",
			"ETH2.S":"1.1000000000",
			"ETH2":"0.2000000000",
			"ETH":"0",
			"DOT":"3",
			"DOT.S":"2.0000000001",
			"DOT28.S":"1",
			"ZUSD":"100.1234",
			"USD.HOLD":"0.0001",
			"USDC":"0.1",
			"USDC.F":"0.2",
			"XXDG":"0"
		}}`)
	})
	s := newTestSource(t, "AAAA-key-3f9a", srv.URL)
	before := time.Now()
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WalletID != "kraken" {
		t.Errorf("WalletID = %q", res.WalletID)
	}
	if res.AsOf.Before(before) || res.AsOf.After(time.Now()) {
		t.Errorf("AsOf = %v", res.AsOf)
	}
	type key struct {
		sym    string
		staked bool
	}
	got := map[key]core.Holding{}
	for _, h := range res.Holdings {
		k := key{h.Symbol, h.Staked}
		if _, dup := got[k]; dup {
			t.Errorf("duplicate holding for %+v", k)
		}
		got[k] = h
		if h.AssetKey != h.Symbol {
			t.Errorf("%s: AssetKey %q != Symbol %q", h.Symbol, h.AssetKey, h.Symbol)
		}
		if h.WalletID != "kraken" {
			t.Errorf("%s: WalletID = %q", h.Symbol, h.WalletID)
		}
		if h.Vault {
			t.Errorf("%s: Kraken holdings are never vaults", h.Symbol)
		}
	}
	want := map[key]string{
		{"BTC", false}:  "1.5",
		{"BTC", true}:   "0.25",
		{"ETH", true}:   "1.3",
		{"DOT", false}:  "3",
		{"DOT", true}:   "3.0000000001",
		{"USD", false}:  "100.1235",
		{"USDC", false}: "0.1",
		{"USDC", true}:  "0.2",
	}
	if len(got) != len(want) {
		t.Errorf("got %d holdings %v, want %d", len(got), res.Holdings, len(want))
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
		f, _ := strconv.ParseFloat(amt, 64)
		if h.Amount != f {
			t.Errorf("%+v: Amount = %v, want %v", k, h.Amount, f)
		}
	}
	for _, k := range []key{{"ETH", false}, {"DOGE", false}} {
		if _, ok := got[k]; ok {
			t.Errorf("zero balance %+v should have been dropped", k)
		}
	}
}

func TestFetchSumsDecimalsExactly(t *testing.T) {
	srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprint(w, `{"error":[],"result":{"XXBT":"0.1","XBT":"0.2"}}`)
	})
	res, err := newTestSource(t, "k", srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 || res.Holdings[0].AmountStr != "0.3" {
		t.Fatalf("holdings = %+v, want a single BTC holding of exactly 0.3", res.Holdings)
	}
}

func TestFetchMeta(t *testing.T) {
	srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprint(w, `{"error":[],"result":{}}`)
	})
	res, err := newTestSource(t, "AbCdEf-3f9a", srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 0 {
		t.Errorf("holdings = %+v", res.Holdings)
	}
	want := []core.KV{
		{K: "API key", V: "••••3f9a"},
		{K: "Permissions needed", V: "Query Funds, Query Ledger Entries"},
	}
	if len(res.Meta) != len(want) {
		t.Fatalf("meta = %+v", res.Meta)
	}
	for i := range want {
		if res.Meta[i] != want[i] {
			t.Errorf("meta[%d] = %+v, want %+v", i, res.Meta[i], want[i])
		}
	}
	if strings.Contains(fmt.Sprint(res.Meta), "AbCdEf") {
		t.Error("meta leaks more than the last four characters of the API key")
	}
}

func TestFetchErrorArray(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"invalid key", `{"error":["EAPI:Invalid key"]}`, []string{"EAPI:Invalid key"}},
		{"permission", `{"error":["EGeneral:Permission denied"]}`, []string{"EGeneral:Permission denied"}},
		{"nonce", `{"error":["EAPI:Invalid nonce"],"result":{}}`, []string{"EAPI:Invalid nonce"}},
		{"multiple", `{"error":["EAPI:Invalid key","EGeneral:Internal error"]}`, []string{"EAPI:Invalid key", "EGeneral:Internal error"}},
		// A result next to a non-empty error must still be treated as a failure.
		{"result ignored", `{"error":["EAPI:Invalid signature"],"result":{"XXBT":"1"}}`, []string{"EAPI:Invalid signature"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
				calls.Add(1)
				fmt.Fprint(w, tc.body) // HTTP 200
			})
			res, err := newTestSource(t, "k", srv.URL).Fetch(context.Background())
			if err == nil {
				t.Fatalf("expected an error, got holdings %+v", res.Holdings)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if calls.Load() != 1 {
				t.Errorf("an error[] response was retried %d times", calls.Load())
			}
		})
	}
}

func TestFetchHTTPErrorWithKrakenBody(t *testing.T) {
	srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":["EGeneral:Permission denied"]}`)
	})
	_, err := newTestSource(t, "k", srv.URL).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "EGeneral:Permission denied") {
		t.Fatalf("err = %v, want the Kraken message", err)
	}
}

func TestFetchBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>nope</html>`,
		"bad amount":    `{"error":[],"result":{"XXBT":"lots"}}`,
		"missing error": `{"result":{"XXBT":"1"}}`, // tolerated: no error field means no error
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) { fmt.Fprint(w, body) })
			res, err := newTestSource(t, "k", srv.URL).Fetch(context.Background())
			if name == "missing error" {
				if err != nil || len(res.Holdings) != 1 {
					t.Fatalf("res=%+v err=%v", res, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got %+v", res)
			}
		})
	}
}

func TestFetchRetryUsesFreshNonceAndSignature(t *testing.T) {
	srv, reqs := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"error":[],"result":{"XXBT":"2"}}`)
	})
	res, err := newTestSource(t, "k-retry", srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 || res.Holdings[0].AmountStr != "2" {
		t.Fatalf("holdings = %+v", res.Holdings)
	}
	if len(*reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(*reqs))
	}
	a, b := (*reqs)[0], (*reqs)[1]
	if a.body == b.body {
		t.Errorf("retry reused the nonce: %q", a.body)
	}
	if a.apiSign == b.apiSign {
		t.Error("retry reused the signature")
	}
	nonce := strings.TrimPrefix(b.body, "nonce=")
	if want, _ := Sign("/0/private/Balance", nonce, b.body, docSecret); b.apiSign != want {
		t.Error("retry signature does not match its own nonce")
	}
}

func TestFetchHonoursContext(t *testing.T) {
	srv, _ := balanceServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprint(w, `{"error":[],"result":{}}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestSource(t, "k", srv.URL).Fetch(ctx); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}

func TestSourceImplementsCoreSource(t *testing.T) {
	var _ core.Source = (*Source)(nil)
}
