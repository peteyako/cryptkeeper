// Package kraken reads balances from a Kraken account through the private Spot REST API.
//
// The key only needs "Query Funds" (and "Query Ledger Entries" for activity). Requests are
// signed as documented at https://docs.kraken.com/api/docs/guides/spot-rest-auth/:
//
//	API-Sign = base64(HMAC-SHA512(base64decode(secret), urlPath + SHA256(nonce + postData)))
package kraken

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

const (
	defaultBaseURL = "https://api.kraken.com"
	balancePath    = "/0/private/Balance"
)

// Config describes one Kraken account.
type Config struct {
	ID, Label, APIKey, APISecret string
	// BaseURL defaults to https://api.kraken.com; tests point it at an httptest server.
	BaseURL string
}

// Source is a Kraken account. It implements core.Source.
type Source struct {
	id        string
	label     string
	apiKey    string
	secret    string
	baseURL   string
	http      *httpx.Client
	nonce     *atomic.Int64
	now       func() time.Time // overridable in tests
	maskedKey string
}

var _ core.Source = (*Source)(nil)

// nonces holds the last nonce used per API key for the whole process. Kraken requires a strictly
// increasing nonce per key, so two Source values built from the same key (for example after the
// source is edited in the UI while a fetch is still running) must share one counter.
var nonces sync.Map // API key -> *atomic.Int64

// New validates cfg and returns a Source. The secret must be the base64 string Kraken shows.
func New(cfg Config, c *httpx.Client) (*Source, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("kraken: API key is required")
	}
	if strings.TrimSpace(cfg.APISecret) == "" {
		return nil, errors.New("kraken: API secret is required")
	}
	secret := strings.TrimSpace(cfg.APISecret)
	if _, err := base64.StdEncoding.DecodeString(secret); err != nil {
		return nil, errors.New("kraken: API secret is not valid base64")
	}
	if c == nil {
		c = httpx.New()
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	key := strings.TrimSpace(cfg.APIKey)
	counter, _ := nonces.LoadOrStore(key, new(atomic.Int64))
	return &Source{
		id:        cfg.ID,
		label:     cfg.Label,
		apiKey:    key,
		secret:    secret,
		baseURL:   base,
		http:      c,
		nonce:     counter.(*atomic.Int64),
		now:       time.Now,
		maskedKey: maskKey(key),
	}, nil
}

// ID returns the source id.
func (s *Source) ID() string { return s.id }

// Sign returns the API-Sign header value for a private call.
//
// urlPath is the part of the URL starting at "/0/private", nonce is the nonce as sent, and
// postData is the exact request body (which itself starts with "nonce=<nonce>").
func Sign(urlPath string, nonce string, postData string, secretB64 string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return "", fmt.Errorf("kraken: API secret is not valid base64: %w", err)
	}
	sum := sha256.Sum256([]byte(nonce + postData))
	mac := hmac.New(sha512.New, key)
	mac.Write([]byte(urlPath))
	mac.Write(sum[:])
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// nextNonce returns max(now in ms, last+1) and records it. It is safe for concurrent use and
// never repeats or goes backwards, even when the clock stalls or steps back.
func (s *Source) nextNonce() string {
	for {
		last := s.nonce.Load()
		n := max(s.now().UnixMilli(), last+1)
		if s.nonce.CompareAndSwap(last, n) {
			return strconv.FormatInt(n, 10)
		}
	}
}

type balanceResponse struct {
	Error  []string          `json:"error"`
	Result map[string]amount `json:"result"`
}

// amount accepts a decimal as a JSON string (what Kraken sends) or a bare number.
type amount string

func (a *amount) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = amount(s)
		return nil
	}
	*a = amount(string(b))
	return nil
}

// Fetch calls the Balance endpoint and returns one holding per (symbol, staked) pair.
func (s *Source) Fetch(ctx context.Context) (core.WalletResult, error) {
	resp, err := s.balance(ctx)
	if err != nil {
		return core.WalletResult{}, err
	}

	type key struct {
		symbol string
		staked bool
	}
	sums := map[key]*decimalSum{}
	for code, raw := range resp.Result {
		sym, staked := NormalizeAsset(code)
		if sym == "" {
			continue
		}
		k := key{sym, staked}
		if sums[k] == nil {
			sums[k] = &decimalSum{}
		}
		if err := sums[k].add(string(raw)); err != nil {
			return core.WalletResult{}, fmt.Errorf("kraken: balance of %s: %w", code, err)
		}
	}

	keys := make([]key, 0, len(sums))
	for k, d := range sums {
		if !d.isZero() {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].symbol != keys[j].symbol {
			return keys[i].symbol < keys[j].symbol
		}
		return !keys[i].staked && keys[j].staked
	})
	holdings := make([]core.Holding, 0, len(keys))
	for _, k := range keys {
		d := sums[k]
		holdings = append(holdings, core.Holding{
			WalletID:  s.id,
			AssetKey:  k.symbol,
			Symbol:    k.symbol,
			Amount:    d.float(),
			AmountStr: d.String(),
			Staked:    k.staked,
		})
	}
	return core.WalletResult{
		WalletID: s.id,
		Holdings: holdings,
		Meta: []core.KV{
			{K: "API key", V: s.maskedKey},
			{K: "Permissions needed", V: "Query Funds, Query Ledger Entries"},
		},
		AsOf: s.now().UTC(),
	}, nil
}

// balance performs the signed POST. Each attempt (including httpx retries) gets a fresh nonce and
// signature, so a retried request is never rejected as a nonce replay.
func (s *Source) balance(ctx context.Context) (*balanceResponse, error) {
	endpoint := s.baseURL + balancePath
	body, err := s.http.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		nonce := s.nextNonce()
		postData := url.Values{"nonce": {nonce}}.Encode()
		sig, err := Sign(balancePath, nonce, postData, s.secret)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(postData))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("API-Key", s.apiKey)
		req.Header.Set("API-Sign", sig)
		return req, nil
	})

	var resp balanceResponse
	parseErr := json.Unmarshal(body, &resp)
	if err != nil {
		// A 4xx still often carries Kraken's own explanation; prefer that to the raw body.
		var se *httpx.StatusError
		if errors.As(err, &se) && parseErr == nil && len(resp.Error) > 0 {
			return nil, fmt.Errorf("kraken: %s (HTTP %d)", strings.Join(resp.Error, "; "), se.Status)
		}
		return nil, fmt.Errorf("kraken: %w", err)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("kraken: decode Balance response: %w", parseErr)
	}
	// Kraken reports failures in error[] even with HTTP 200.
	if len(resp.Error) > 0 {
		return nil, fmt.Errorf("kraken: %s", strings.Join(resp.Error, "; "))
	}
	return &resp, nil
}

// legacyAssets maps Kraken's legacy X/Z-prefixed codes to the common ticker. Plain codes that merely
// start with X or Z (XTZ, ZRX, XRP) are not legacy and are deliberately absent.
var legacyAssets = map[string]string{
	"XXBT": "BTC", "XBT": "BTC",
	"XETH": "ETH",
	"XXDG": "DOGE", "XDG": "DOGE",
	"XLTC": "LTC",
	"XXRP": "XRP",
	"XXLM": "XLM",
	"XXMR": "XMR",
	"XZEC": "ZEC",
	"XETC": "ETC",
	"XREP": "REP",
	"XMLN": "MLN",
	"ZUSD": "USD",
	"ZEUR": "EUR",
	"ZGBP": "GBP",
	"ZCAD": "CAD",
	"ZJPY": "JPY",
	"ZAUD": "AUD",
	"ZCHF": "CHF",
}

// stakedSuffixes mark balances that sit in a Kraken yield product. Per Kraken's Balance docs,
// ".S" is on-chain staking, ".M" opt-in rewards, ".B" yield-bearing products and ".F" automatic
// Kraken Rewards; ".P" is the legacy parachain suffix (DOT.P). All of them are the base asset
// working for yield, so they are merged under the base symbol with Staked=true.
var stakedSuffixes = []string{".S", ".M", ".F", ".B", ".P"}

// holdSuffix marks funds on hold (for example USD.HOLD). It is the same asset, not a staked one.
const holdSuffix = ".HOLD"

// NormalizeAsset maps a Kraken balance code to a common symbol and reports whether the balance is
// staked (earning in a yield product).
//
//	XXBT -> BTC          XBT.M -> BTC (staked)    ETH2, ETH2.S -> ETH (staked)
//	DOT.S -> DOT (staked) ZUSD -> USD             USD.HOLD -> USD
//
// Matching is case-sensitive on purpose: Kraken's tokenized stocks use a lowercase "x" (AAPLx).
// Unknown suffixes are left alone so that an unrecognized product shows up as its own, unpriced
// symbol instead of being silently merged into the wrong asset.
func NormalizeAsset(code string) (symbol string, staked bool) {
	base := strings.TrimSpace(code)

	base = strings.TrimSuffix(base, holdSuffix)
	for _, suf := range stakedSuffixes {
		if trimmed, ok := strings.CutSuffix(base, suf); ok && trimmed != "" {
			base, staked = trimmed, true
			break
		}
	}

	// ETH2 is the staked-ETH asset itself, with or without a suffix.
	if base == "ETH2" {
		return "ETH", true
	}
	// Bonded staking variants carry the lock-up in the code (DOT28.S, KSM07.S, SOL03.S): two digits
	// after the ticker, only ever with a staking suffix.
	if staked {
		base = trimBondingDigits(base)
	}
	if mapped, ok := legacyAssets[base]; ok {
		base = mapped
	}
	return base, staked
}

// trimBondingDigits turns DOT28 into DOT. It only strips exactly two trailing digits that follow at
// least two letters, so tickers such as 1INCH are never touched.
func trimBondingDigits(s string) string {
	if len(s) < 4 {
		return s
	}
	tail := s[len(s)-2:]
	if tail[0] < '0' || tail[0] > '9' || tail[1] < '0' || tail[1] > '9' {
		return s
	}
	head := s[:len(s)-2]
	for i := 0; i < len(head); i++ {
		if c := head[i]; c < 'A' || c > 'Z' {
			return s
		}
	}
	return head
}

func maskKey(k string) string {
	const dots = "••••"
	r := []rune(k)
	if len(r) <= 4 {
		return dots
	}
	return dots + string(r[len(r)-4:])
}

// decimalSum adds decimal strings exactly (no float rounding) and renders the result without
// trailing zeros.
type decimalSum struct {
	sum   big.Rat
	scale int // most fractional digits seen, enough to print the exact sum
}

func (d *decimalSum) add(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty amount")
	}
	var r big.Rat
	if _, ok := r.SetString(s); !ok {
		return fmt.Errorf("invalid amount %q", s)
	}
	scale := 0
	if strings.ContainsAny(s, "eE") {
		scale = 30
	} else if i := strings.IndexByte(s, '.'); i >= 0 {
		scale = len(s) - i - 1
	}
	d.scale = max(d.scale, scale)
	d.sum.Add(&d.sum, &r)
	return nil
}

func (d *decimalSum) isZero() bool { return d.sum.Sign() == 0 }

func (d *decimalSum) float() float64 {
	f, _ := d.sum.Float64()
	return f
}

func (d *decimalSum) String() string {
	out := d.sum.FloatString(d.scale)
	if strings.Contains(out, ".") {
		out = strings.TrimRight(out, "0")
		out = strings.TrimSuffix(out, ".")
	}
	return out
}
