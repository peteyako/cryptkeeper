// Package coinbase reads balances from a Coinbase account with a view-only CDP API key.
//
// Balances come from two APIs that are reconciled by account id:
//
//   - Advanced Trade  GET /api/v3/brokerage/accounts (cursor pagination)
//   - Coinbase App v2 GET /v2/accounts               (next_uri pagination; lists vaults and staking wallets)
//
// Every request carries its own short-lived JWT, see BuildJWT.
package coinbase

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
)

const (
	defaultBaseURL = "https://api.coinbase.com"

	advancedTradeAccountsPath = "/api/v3/brokerage/accounts"
	v2AccountsPath            = "/v2/accounts"

	advancedTradePageSize = 250 // documented maximum for Advanced Trade "limit"
	v2PageSize            = 100 // documented maximum for v2 "limit"
	maxPages              = 500 // hard stop against a server that never ends the listing

	// jwtLifetime is how long a token is valid. The CDP docs use 120 s and say a new JWT is
	// required for every request.
	jwtLifetime = 120 * time.Second

	// v2Version pins the v2 response shape (currency is an object with code and name).
	v2Version = "2024-01-01"
)

// Config describes one Coinbase account, authenticated with a CDP API key.
type Config struct {
	ID, Label string
	// KeyName is the key's full name, "organizations/{org_id}/apiKeys/{key_id}".
	KeyName string
	// PrivateKey is either the PEM of an ECDSA (P-256) key, with real or "\n"-escaped newlines,
	// or the base64 of a 64-byte Ed25519 key (seed followed by public key).
	PrivateKey string
	// BaseURL defaults to https://api.coinbase.com; tests point it at an httptest server.
	BaseURL string
}

// Source is a Coinbase account. It implements core.Source.
type Source struct {
	id      string
	label   string
	keyName string
	key     parsedKey
	baseURL string
	host    string // host (and port, if any) used in the JWT "uri" claim
	http    *httpx.Client
	now     func() time.Time
}

var _ core.Source = (*Source)(nil)

// New validates cfg and parses the private key eagerly, so a bad key fails when the source is
// configured rather than on the first fetch.
func New(cfg Config, c *httpx.Client) (*Source, error) {
	keyName := strings.TrimSpace(cfg.KeyName)
	if keyName == "" {
		return nil, errors.New("coinbase: API key name is required")
	}
	if strings.TrimSpace(cfg.PrivateKey) == "" {
		return nil, errors.New("coinbase: private key is required")
	}
	key, err := parseKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("coinbase: private key: %w", err)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("coinbase: invalid base URL %q", base)
	}
	if c == nil {
		c = httpx.New()
	}
	return &Source{
		id:      cfg.ID,
		label:   cfg.Label,
		keyName: keyName,
		key:     key,
		baseURL: base,
		host:    u.Host,
		http:    c,
		now:     time.Now,
	}, nil
}

// ID returns the source id.
func (s *Source) ID() string { return s.id }

// ---------------------------------------------------------------------------------------------
// Keys and JWTs
// ---------------------------------------------------------------------------------------------

type parsedKey struct {
	signer any // *ecdsa.PrivateKey (P-256) or ed25519.PrivateKey
	method jwt.SigningMethod
	alg    string // "ES256" or "EdDSA"
}

var pemRE = regexp.MustCompile(`(?s)-----BEGIN ([A-Z0-9 ]+)-----(.*?)-----END ([A-Z0-9 ]+)-----`)

// parseKey accepts the formats CDP hands out:
//
//   - ECDSA: PEM "EC PRIVATE KEY" (SEC1), with PKCS8 ("PRIVATE KEY") as a fallback.
//   - Ed25519: base64 of 64 bytes (32-byte seed followed by the 32-byte public key); a PKCS8 PEM
//     holding an Ed25519 key is accepted as well.
//
// Input pasted into a form is forgiven: "\n" written as two characters, CRLF, surrounding quotes
// or whitespace, and PEM whose line breaks were turned into spaces.
func parseKey(secret string) (parsedKey, error) {
	s := strings.TrimSpace(secret)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	s = strings.ReplaceAll(s, `\r`, "")
	s = strings.ReplaceAll(s, `\n`, "\n")
	if s == "" {
		return parsedKey{}, errors.New("empty")
	}

	if m := pemRE.FindStringSubmatch(s); m != nil {
		return parsePEM(m[1], m[2])
	}
	return parseEd25519(s)
}

func parsePEM(typ, body string) (parsedKey, error) {
	// Rebuild canonical PEM: drop all whitespace from the body and re-wrap at 64 columns.
	compact := strings.Join(strings.Fields(body), "")
	var b strings.Builder
	b.WriteString("-----BEGIN " + typ + "-----\n")
	for len(compact) > 64 {
		b.WriteString(compact[:64] + "\n")
		compact = compact[64:]
	}
	b.WriteString(compact + "\n-----END " + typ + "-----\n")
	block, _ := pem.Decode([]byte(b.String()))
	if block == nil {
		return parsedKey{}, errors.New("PEM block is not valid base64")
	}

	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return ecdsaKey(k)
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return parsedKey{}, errors.New("PEM is neither an EC (SEC1) nor a PKCS8 private key")
	}
	switch k := pk.(type) {
	case *ecdsa.PrivateKey:
		return ecdsaKey(k)
	case ed25519.PrivateKey:
		return parsedKey{signer: k, method: jwt.SigningMethodEdDSA, alg: "EdDSA"}, nil
	default:
		return parsedKey{}, fmt.Errorf("unsupported key type %T: CDP keys are ECDSA P-256 or Ed25519", pk)
	}
}

func ecdsaKey(k *ecdsa.PrivateKey) (parsedKey, error) {
	if k.Curve != elliptic.P256() {
		return parsedKey{}, fmt.Errorf("ES256 needs a P-256 key, got %s", k.Curve.Params().Name)
	}
	return parsedKey{signer: k, method: jwt.SigningMethodES256, alg: "ES256"}, nil
}

func parseEd25519(s string) (parsedKey, error) {
	compact := strings.Join(strings.Fields(strings.Trim(s, `"'`)), "")
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(compact); err == nil {
			break
		}
	}
	if err != nil {
		return parsedKey{}, errors.New("not a PEM block and not base64")
	}
	if len(raw) != ed25519.PrivateKeySize {
		return parsedKey{}, fmt.Errorf("Ed25519 key must be 64 bytes (seed + public key), got %d", len(raw))
	}
	key := ed25519.PrivateKey(raw)
	// The last 32 bytes are the public key; if they do not match the seed the key was mangled.
	if !key.Public().(ed25519.PublicKey).Equal(ed25519.NewKeyFromSeed(key.Seed()).Public()) {
		return parsedKey{}, errors.New("Ed25519 public half does not match its seed")
	}
	return parsedKey{signer: key, method: jwt.SigningMethodEdDSA, alg: "EdDSA"}, nil
}

// BuildJWT returns a token for one request, as described in Coinbase's CDP JWT authentication docs
// (https://docs.cdp.coinbase.com/get-started/authentication/jwt-authentication):
//
//	header: alg ES256|EdDSA, typ JWT, kid <key name>, nonce <16 random bytes, hex>
//	claims: sub <key name>, iss "cdp", nbf now, exp now+120s, uri "<METHOD> <host><path>"
//
// path must not contain a query string; if it does, the query is dropped. Ed25519 tokens also carry
// aud ["cdp_service"], as in the CDP Ed25519 samples. A token is single-use by convention: build a
// new one for each request.
func (s *Source) BuildJWT(method, host, path string, now time.Time) (string, error) {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("coinbase: nonce: %w", err)
	}
	claims := jwt.MapClaims{
		"sub": s.keyName,
		"iss": "cdp",
		"nbf": now.Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"uri": strings.ToUpper(method) + " " + host + path,
	}
	if s.key.alg == "EdDSA" {
		claims["aud"] = []string{"cdp_service"}
	}
	tok := jwt.NewWithClaims(s.key.method, claims)
	tok.Header["kid"] = s.keyName
	tok.Header["nonce"] = hex.EncodeToString(nonce[:])
	signed, err := tok.SignedString(s.key.signer)
	if err != nil {
		return "", fmt.Errorf("coinbase: sign JWT: %w", err)
	}
	return signed, nil
}

// getJSON GETs pathAndQuery on the base URL. Every attempt, including httpx retries, signs a fresh
// JWT whose uri claim is the path only.
func (s *Source) getJSON(ctx context.Context, pathAndQuery string, headers map[string]string, out any) error {
	u, err := url.Parse(pathAndQuery)
	if err != nil {
		return err
	}
	body, err := s.http.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		tok, err := s.BuildJWT(http.MethodGet, s.host, u.Path, s.now())
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+pathAndQuery, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", u.Path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------------------------

// decStr is a decimal that may arrive as a JSON string (what Coinbase sends), a bare number or null.
type decStr string

func (d *decStr) UnmarshalJSON(b []byte) error {
	switch {
	case string(b) == "null":
		*d = ""
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = decStr(s)
	default:
		*d = decStr(b)
	}
	return nil
}

type atMoney struct {
	Value    decStr `json:"value"`
	Currency string `json:"currency"`
}

type atAccount struct {
	UUID             string  `json:"uuid"`
	Name             string  `json:"name"`
	Currency         string  `json:"currency"`
	AvailableBalance atMoney `json:"available_balance"`
	Hold             atMoney `json:"hold"`
	Type             string  `json:"type"`
}

type atResponse struct {
	Accounts []atAccount `json:"accounts"`
	HasNext  bool        `json:"has_next"`
	Cursor   string      `json:"cursor"`
}

// v2Currency is the account's currency: an object on current API versions, a bare code on old ones.
type v2Currency struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (c *v2Currency) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.Code)
	}
	type plain v2Currency
	return json.Unmarshal(b, (*plain)(c))
}

type v2Account struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Type     string     `json:"type"` // "wallet", "vault", "fiat", ...
	Currency v2Currency `json:"currency"`
	Balance  struct {
		Amount   decStr `json:"amount"`
		Currency string `json:"currency"`
	} `json:"balance"`
}

type v2Response struct {
	Pagination struct {
		NextURI *string `json:"next_uri"`
	} `json:"pagination"`
	Data []v2Account `json:"data"`
}

// ---------------------------------------------------------------------------------------------
// Fetching
// ---------------------------------------------------------------------------------------------

// account is one account from either API, in a common shape.
type account struct {
	id       string
	code     string // currency code as the API reports it
	curName  string // currency name (v2 only)
	amount   decimal
	vault    bool
	inAT     bool
	inV2     bool
	atAmount decimal // Advanced Trade's own figure, kept to report differences
}

func (s *Source) fetchAdvancedTrade(ctx context.Context) ([]*account, error) {
	var out []*account
	cursor := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("more than %d pages", maxPages)
		}
		q := url.Values{"limit": {fmt.Sprint(advancedTradePageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp atResponse
		if err := s.getJSON(ctx, advancedTradeAccountsPath+"?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, a := range resp.Accounts {
			if a.UUID == "" || strings.TrimSpace(a.Currency) == "" {
				continue
			}
			avail, err := parseDecimal(string(a.AvailableBalance.Value))
			if err != nil {
				return nil, fmt.Errorf("account %s available balance: %w", a.UUID, err)
			}
			hold, err := parseDecimal(string(a.Hold.Value))
			if err != nil {
				return nil, fmt.Errorf("account %s hold: %w", a.UUID, err)
			}
			total := avail.plus(hold) // hold is funds reserved against the available balance; both are owned
			out = append(out, &account{
				id:       a.UUID,
				code:     a.Currency,
				amount:   total,
				atAmount: total,
				vault:    a.Type == "ACCOUNT_TYPE_VAULT",
				inAT:     true,
			})
		}
		if !resp.HasNext {
			return out, nil
		}
		if resp.Cursor == "" {
			return nil, errors.New("has_next is true but no cursor was returned")
		}
		if seen[resp.Cursor] {
			return nil, errors.New("pagination loop: cursor repeated")
		}
		seen[resp.Cursor] = true
		cursor = resp.Cursor
	}
}

func (s *Source) fetchV2(ctx context.Context) ([]*account, error) {
	var out []*account
	next := fmt.Sprintf("%s?limit=%d", v2AccountsPath, v2PageSize)
	seen := map[string]bool{}
	hdr := map[string]string{"CB-VERSION": v2Version}
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("more than %d pages", maxPages)
		}
		if seen[next] {
			return nil, errors.New("pagination loop: next_uri repeated")
		}
		seen[next] = true

		var resp v2Response
		if err := s.getJSON(ctx, next, hdr, &resp); err != nil {
			return nil, err
		}
		for _, a := range resp.Data {
			code := a.Currency.Code
			if code == "" {
				code = a.Balance.Currency
			}
			if a.ID == "" || strings.TrimSpace(code) == "" {
				continue
			}
			amt, err := parseDecimal(string(a.Balance.Amount))
			if err != nil {
				return nil, fmt.Errorf("account %s balance: %w", a.ID, err)
			}
			out = append(out, &account{
				id:      a.ID,
				code:    code,
				curName: a.Currency.Name,
				amount:  amt,
				vault:   a.Type == "vault",
				inV2:    true,
			})
		}

		next = ""
		if resp.Pagination.NextURI != nil && *resp.Pagination.NextURI != "" {
			// next_uri is a path plus query. Only that part is used, always against our own base
			// URL, so a hostile or odd absolute URL can never receive the bearer token.
			u, err := url.Parse(*resp.Pagination.NextURI)
			if err != nil || !strings.HasPrefix(u.Path, "/") {
				return nil, fmt.Errorf("unusable next_uri %q", *resp.Pagination.NextURI)
			}
			next = u.RequestURI()
		}
	}
	return out, nil
}

// Fetch returns the account's holdings. Either API may fail alone: the other one's holdings are
// returned with a warning. Only when both fail is it an error.
func (s *Source) Fetch(ctx context.Context) (core.WalletResult, error) {
	at, atErr := s.fetchAdvancedTrade(ctx)
	if err := ctx.Err(); err != nil {
		return core.WalletResult{}, err
	}
	v2, v2Err := s.fetchV2(ctx)
	if err := ctx.Err(); err != nil {
		return core.WalletResult{}, err
	}
	if atErr != nil && v2Err != nil {
		return core.WalletResult{}, fmt.Errorf("coinbase: Advanced Trade accounts: %w; v2 accounts: %w", atErr, v2Err)
	}

	var warnings []string
	var recon string
	switch {
	case atErr != nil:
		warnings = append(warnings, fmt.Sprintf("Advanced Trade accounts unavailable (%v); showing v2 balances only", atErr))
		recon = "v2 only (Advanced Trade unavailable)"
	case v2Err != nil:
		w := fmt.Sprintf("Coinbase v2 accounts unavailable (%v); showing Advanced Trade balances only, so vaults and staked balances may be missing", v2Err)
		var se *httpx.StatusError
		if s.key.alg == "EdDSA" && errors.As(v2Err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			w += ". Coinbase documents ECDSA keys only for the v2 API; create an ECDSA key to include vaults and staking"
		}
		warnings = append(warnings, w)
		recon = "Advanced Trade only (v2 unavailable)"
	}

	merged, stats := reconcile(at, v2)
	if recon == "" {
		recon = fmt.Sprintf("Advanced Trade only: %d · v2 only: %d", stats.atOnly, stats.v2Only)
	}

	holdings := buildHoldings(s.id, merged)
	meta := []core.KV{
		{K: "API key", V: maskKey(s.keyName)},
		{K: "Accounts", V: fmt.Sprintf("%d (%d vault)", stats.accounts, stats.vaults)},
		{K: "Reconciliation", V: recon},
	}
	if stats.differing > 0 {
		meta = append(meta, core.KV{K: "Balance differences", V: fmt.Sprintf("%d (v2 balance used)", stats.differing)})
	}
	return core.WalletResult{
		WalletID: s.id,
		Holdings: holdings,
		Warnings: warnings,
		Meta:     meta,
		AsOf:     s.now().UTC(),
	}, nil
}

type reconcileStats struct {
	accounts  int // non-zero accounts that contribute to the result
	vaults    int // ... of which vaults
	atOnly    int // non-zero accounts that Advanced Trade lists and v2 does not
	v2Only    int // non-zero accounts that v2 lists and Advanced Trade does not
	differing int // accounts in both whose balances differ (v2 wins)
}

// reconcile merges the two listings by account id. Where both list an account the v2 balance, code
// and type win; accounts in only one listing are kept as they are.
func reconcile(at, v2 []*account) ([]*account, reconcileStats) {
	var st reconcileStats
	byID := make(map[string]*account, len(at)+len(v2))
	var order []string
	for _, a := range at {
		if _, dup := byID[a.id]; !dup {
			order = append(order, a.id)
		}
		byID[a.id] = a
	}
	for _, a := range v2 {
		prev, both := byID[a.id]
		if !both {
			order = append(order, a.id)
		} else if prev.inAT {
			a.inAT = true
			a.atAmount = prev.atAmount
			if !prev.atAmount.equal(a.amount) || !strings.EqualFold(prev.code, a.code) {
				st.differing++
			}
		}
		byID[a.id] = a
	}

	out := make([]*account, 0, len(order))
	for _, id := range order {
		a := byID[id]
		out = append(out, a)
		if a.amount.isZero() {
			continue
		}
		st.accounts++
		if a.vault {
			st.vaults++
		}
		switch {
		case a.inAT && !a.inV2:
			st.atOnly++
		case a.inV2 && !a.inAT:
			st.v2Only++
		}
	}
	return out, st
}

// normalizeCurrency maps a Coinbase currency code (and, from v2, its display name) to a base symbol
// and a staked flag. The heuristics, in order:
//
//  1. "ETH2" is staked ETH.
//  2. A code ending in "-STAKED" is that asset, staked.
//  3. A currency name containing "staked" marks the account as staked, unless it also says
//     "unstaked" or "wrapped": wrapped staking tokens such as cbETH are ordinary liquid assets with
//     their own price and are not positions.
func normalizeCurrency(code, name string) (symbol string, staked bool) {
	sym := strings.ToUpper(strings.TrimSpace(code))
	if sym == "ETH2" {
		return "ETH", true
	}
	if base, ok := strings.CutSuffix(sym, "-STAKED"); ok && base != "" {
		return base, true
	}
	n := strings.ToLower(name)
	if strings.Contains(n, "staked") && !strings.Contains(n, "unstaked") && !strings.Contains(n, "wrapped") {
		return sym, true
	}
	return sym, false
}

// buildHoldings sums accounts into one holding per (symbol, staked, vault), dropping zeros.
func buildHoldings(walletID string, accts []*account) []core.Holding {
	type key struct {
		symbol string
		staked bool
		vault  bool
	}
	sums := map[key]decimal{}
	// A staked or vault holding takes the name of the plain asset when there is one (ETH2 -> "Ethereum").
	plainName := map[string]string{}
	anyName := map[string]string{}
	for _, a := range accts {
		sym, staked := normalizeCurrency(a.code, a.curName)
		if a.curName != "" {
			if !staked && plainName[sym] == "" {
				plainName[sym] = a.curName
			}
			if anyName[sym] == "" {
				anyName[sym] = a.curName
			}
		}
		k := key{sym, staked, a.vault}
		cur, ok := sums[k]
		if !ok {
			cur = decimal{r: new(big.Rat)}
		}
		sums[k] = cur.plus(a.amount)
	}

	keys := make([]key, 0, len(sums))
	for k, d := range sums {
		if !d.isZero() {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		switch {
		case a.symbol != b.symbol:
			return a.symbol < b.symbol
		case a.staked != b.staked:
			return !a.staked
		default:
			return !a.vault && b.vault
		}
	})
	out := make([]core.Holding, 0, len(keys))
	for _, k := range keys {
		d := sums[k]
		name := plainName[k.symbol]
		if name == "" {
			name = anyName[k.symbol]
		}
		out = append(out, core.Holding{
			WalletID:  walletID,
			AssetKey:  k.symbol,
			Symbol:    k.symbol,
			Name:      name,
			Amount:    d.float(),
			AmountStr: d.String(),
			Staked:    k.staked,
			Vault:     k.vault,
		})
	}
	return out
}

func maskKey(k string) string {
	const dots = "••••"
	r := []rune(k)
	if len(r) <= 4 {
		return dots
	}
	return dots + string(r[len(r)-4:])
}

// ---------------------------------------------------------------------------------------------
// Exact decimals
// ---------------------------------------------------------------------------------------------

// decimal is an exact amount. Amounts are summed as rationals so 0.1 + 0.2 is exactly 0.3, and
// String prints the exact sum without trailing zeros.
type decimal struct {
	r     *big.Rat
	scale int // fractional digits needed to print r exactly
}

// parseDecimal parses a decimal string. The empty string is zero (a missing hold, for example).
func parseDecimal(s string) (decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return decimal{r: new(big.Rat)}, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return decimal{}, fmt.Errorf("invalid amount %q", s)
	}
	scale := 0
	if strings.ContainsAny(s, "eE") {
		scale = 30
	} else if i := strings.IndexByte(s, '.'); i >= 0 {
		scale = len(s) - i - 1
	}
	return decimal{r: r, scale: scale}, nil
}

func (d decimal) plus(o decimal) decimal {
	l, r := d.r, o.r
	if l == nil {
		l = new(big.Rat)
	}
	if r == nil {
		r = new(big.Rat)
	}
	return decimal{r: new(big.Rat).Add(l, r), scale: max(d.scale, o.scale)}
}

func (d decimal) isZero() bool { return d.r == nil || d.r.Sign() == 0 }

func (d decimal) equal(o decimal) bool {
	l, r := d.r, o.r
	if l == nil {
		l = new(big.Rat)
	}
	if r == nil {
		r = new(big.Rat)
	}
	return l.Cmp(r) == 0
}

func (d decimal) float() float64 {
	if d.r == nil {
		return 0
	}
	f, _ := d.r.Float64()
	return f
}

func (d decimal) String() string {
	if d.r == nil {
		return "0"
	}
	out := d.r.FloatString(d.scale)
	if strings.Contains(out, ".") {
		out = strings.TrimRight(out, "0")
		out = strings.TrimSuffix(out, ".")
	}
	return out
}
