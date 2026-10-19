package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"cryptkeeper/internal/httpx"
)

const (
	// jupiterBatchSize is how many mints go into one Jupiter search request (the API accepts
	// up to 100 comma-separated mints).
	jupiterBatchSize = 50
	// metaConcurrency bounds concurrent on-chain metadata lookups.
	metaConcurrency = 4
	// Per-fetch limits, so a wallet full of spam airdrops (thousands of mints) can't blow the
	// fetch deadline or hammer rate-limited APIs. Mints left unresolved show a shortened mint
	// and are retried on later refreshes; the cache fills progressively.
	maxJupiterBatches = 4
	maxOnChainLookups = 40
	resolveBudget     = 20 * time.Second

	// Source values of TokenMeta.
	sourceJupiter   = "jupiter"
	sourceToken2022 = "token2022"
	sourceMetaplex  = "metaplex"
	sourceFallback  = "fallback"
)

// TokenMeta is the resolved identity of an SPL token.
type TokenMeta struct {
	Mint     string `json:"mint"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	LogoURL  string `json:"logoUrl,omitempty"`
	Decimals int    `json:"decimals"`
	Verified bool   `json:"verified,omitempty"`
	Source   string `json:"source"` // jupiter|token2022|metaplex|fallback
}

// mintBalance is the summed balance of one mint across all of the owner's token accounts.
type mintBalance struct {
	Mint      string
	Amount    *big.Int
	Decimals  int
	Token2022 bool // at least one account lives under the Token-2022 program
}

// tokenAccountsResult is the part of getTokenAccountsByOwner (jsonParsed) we read.
type tokenAccountsResult struct {
	Value []struct {
		Pubkey  string `json:"pubkey"`
		Account struct {
			Data json.RawMessage `json:"data"`
		} `json:"account"`
	} `json:"value"`
}

type parsedTokenAccount struct {
	Parsed struct {
		Info struct {
			Mint        string `json:"mint"`
			TokenAmount struct {
				Amount   string `json:"amount"`
				Decimals int    `json:"decimals"`
			} `json:"tokenAmount"`
		} `json:"info"`
	} `json:"parsed"`
}

// tokenSet is the owner's token holdings grouped by mint.
type tokenSet struct {
	byMint   map[string]*mintBalance
	accounts int // token accounts returned by the RPC, including empty ones
	skipped  int // accounts ignored because their balance could not be read
	// addrs are the token account addresses in RPC order (Token program first), with a flag for
	// accounts that hold nothing; Activity watches them because incoming transfers to a token
	// account never appear in the owner's own signature list.
	addrs []tokenAcctRef
}

// tokenAcctRef is one of the owner's token accounts.
type tokenAcctRef struct {
	Pubkey string
	Empty  bool
}

// tokenAccounts lists the owner's token accounts under both token programs and sums them per
// mint with exact integer arithmetic.
func (s *Source) tokenAccounts(ctx context.Context) (*tokenSet, error) {
	return s.tokenAccountsOf(ctx, s.cfg.Address)
}

// tokenAccountsOf lists owner's token accounts under both token programs.
func (s *Source) tokenAccountsOf(ctx context.Context, owner string) (*tokenSet, error) {
	ts := &tokenSet{byMint: map[string]*mintBalance{}}
	for _, prog := range []struct{ id, label string }{
		{TokenProgramID, "Token"}, {Token2022ProgramID, "Token-2022"},
	} {
		var out tokenAccountsResult
		err := s.rpc.call(ctx, "getTokenAccountsByOwner", []any{
			owner,
			map[string]string{"programId": prog.id},
			map[string]string{"encoding": "jsonParsed"},
		}, &out)
		if err != nil {
			return nil, fmt.Errorf("getTokenAccountsByOwner (%s program): %w", prog.label, err)
		}
		for _, v := range out.Value {
			ts.accounts++
			var d parsedTokenAccount
			if json.Unmarshal(v.Account.Data, &d) != nil {
				ts.skipped++
				continue
			}
			info := d.Parsed.Info
			amt, ok := new(big.Int).SetString(info.TokenAmount.Amount, 10)
			if !ok || amt.Sign() < 0 || info.Mint == "" {
				ts.skipped++
				continue
			}
			ts.addrs = append(ts.addrs, tokenAcctRef{Pubkey: v.Pubkey, Empty: amt.Sign() == 0})
			mb := ts.byMint[info.Mint]
			if mb == nil {
				mb = &mintBalance{Mint: info.Mint, Amount: new(big.Int), Decimals: info.TokenAmount.Decimals}
				ts.byMint[info.Mint] = mb
			}
			mb.Amount.Add(mb.Amount, amt)
			mb.Token2022 = mb.Token2022 || prog.id == Token2022ProgramID
		}
	}
	return ts, nil
}

// resolution is the outcome of identity resolution for one mint.
type resolution struct {
	meta         TokenMeta
	fallback     bool // nothing named this token
	lookupFailed bool // at least one lookup errored (as opposed to cleanly finding nothing)
}

// resolver turns mints into token identities: cache -> Jupiter -> Token-2022 metadata
// extension -> Metaplex metadata PDA -> shortened mint.
type resolver struct {
	rpc    *rpcClient
	http   *httpx.Client
	jupURL string
	cache  MetaCache
}

// resolveAll resolves every mint. It never fails: a lookup error just means a worse name.
func (r *resolver) resolveAll(ctx context.Context, mbs []*mintBalance) map[string]resolution {
	out := make(map[string]resolution, len(mbs))
	var pending []*mintBalance
	for _, mb := range mbs {
		if m, ok := r.cache.Get(mb.Mint); ok {
			out[mb.Mint] = resolution{meta: m}
			continue
		}
		pending = append(pending, mb)
	}
	if len(pending) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, resolveBudget)
	defer cancel()

	// 1. Jupiter's verified list (one request a day) names nearly every real token locally.
	verified := verifiedTokens(ctx, r.http, r.jupURL)
	var rest []*mintBalance
	for _, mb := range pending {
		if m, ok := verified[mb.Mint]; ok {
			m.Decimals = mb.Decimals
			r.cache.Put(mb.Mint, m)
			out[mb.Mint] = resolution{meta: m}
			continue
		}
		rest = append(rest, mb)
	}

	// 2. Jupiter search for the rest, in a bounded number of batches.
	found, jupFailed := r.jupiterAll(ctx, rest)
	var onchain []*mintBalance
	for _, mb := range rest {
		if m, ok := found[mb.Mint]; ok {
			r.cache.Put(mb.Mint, m)
			out[mb.Mint] = resolution{meta: m}
			continue
		}
		onchain = append(onchain, mb)
	}

	// 3. On-chain metadata for a bounded number of the remainder; the rest wait for later.
	if len(onchain) > maxOnChainLookups {
		for _, mb := range onchain[maxOnChainLookups:] {
			out[mb.Mint] = fallbackResolution(mb, true)
		}
		onchain = onchain[:maxOnChainLookups]
	}
	results := make([]resolution, len(onchain))
	var g errgroup.Group
	g.SetLimit(metaConcurrency)
	for i, mb := range onchain {
		g.Go(func() error {
			if ctx.Err() != nil {
				results[i] = fallbackResolution(mb, true)
				return nil
			}
			results[i] = r.onChain(ctx, mb, jupFailed[mb.Mint])
			return nil // failures are recorded in the resolution, never propagated
		})
	}
	_ = g.Wait()
	for i, mb := range onchain {
		out[mb.Mint] = results[i]
	}
	return out
}

func fallbackResolution(mb *mintBalance, failed bool) resolution {
	return resolution{
		meta:     TokenMeta{Mint: mb.Mint, Symbol: shortKey(mb.Mint), Decimals: mb.Decimals, Source: sourceFallback},
		fallback: true, lookupFailed: failed,
	}
}

// jupiterAll looks mints up in at most maxJupiterBatches batches and stops at the first
// failure (usually a 429): retrying a rate-limited API in the same fetch only digs deeper.
// failed holds the mints that were not looked up or whose batch errored.
func (r *resolver) jupiterAll(ctx context.Context, mbs []*mintBalance) (found map[string]TokenMeta, failed map[string]bool) {
	found, failed = map[string]TokenMeta{}, map[string]bool{}
	mints := make([]string, len(mbs))
	for i, mb := range mbs {
		mints[i] = mb.Mint
	}
	batches, stop := 0, false
	for chunk := range slices.Chunk(mints, jupiterBatchSize) {
		if stop || batches >= maxJupiterBatches || ctx.Err() != nil {
			for _, m := range chunk {
				failed[m] = true
			}
			continue
		}
		batches++
		got, err := r.jupiter(ctx, chunk)
		if err != nil {
			stop = true
			for _, m := range chunk {
				failed[m] = true
			}
			continue
		}
		for m, t := range got {
			found[m] = t
		}
	}
	return found, failed
}

// verified list: shared across sources, refreshed daily.
var verifiedCache struct {
	sync.Mutex
	byURL map[string]verifiedEntry
}

type verifiedEntry struct {
	at     time.Time
	tokens map[string]TokenMeta
	failed bool
}

// verifiedTokens returns Jupiter's verified token list keyed by mint (empty on failure).
// The tag endpoint sits next to the search endpoint: .../tokens/v2/tag?query=verified.
// A failure is remembered for 10 minutes so a down API isn't retried on every fetch.
func verifiedTokens(ctx context.Context, c *httpx.Client, searchURL string) map[string]TokenMeta {
	tagURL := strings.TrimSuffix(searchURL, "/search") + "/tag?query=verified"
	if !strings.HasSuffix(searchURL, "/search") {
		return nil
	}
	verifiedCache.Lock()
	defer verifiedCache.Unlock() // serialises concurrent sources: one download, then cache hits
	if verifiedCache.byURL == nil {
		verifiedCache.byURL = map[string]verifiedEntry{}
	}
	e, ok := verifiedCache.byURL[tagURL]
	if ok && (!e.failed && time.Since(e.at) < 24*time.Hour || e.failed && time.Since(e.at) < 10*time.Minute) {
		return e.tokens
	}
	var toks []jupToken
	if err := c.GetJSON(ctx, tagURL, nil, &toks); err != nil {
		if ok && !e.failed {
			return e.tokens // keep serving yesterday's list
		}
		verifiedCache.byURL[tagURL] = verifiedEntry{at: time.Now(), failed: true}
		return nil
	}
	m := make(map[string]TokenMeta, len(toks))
	for _, t := range toks {
		sym := cleanText(t.Symbol, maxSymbolRunes)
		if t.ID == "" || sym == "" {
			continue
		}
		m[t.ID] = TokenMeta{
			Mint: t.ID, Symbol: sym, Name: cleanText(t.Name, maxNameRunes),
			LogoURL: safeHTTPURL(t.Icon), Decimals: t.Decimals, Verified: true, Source: sourceJupiter,
		}
	}
	verifiedCache.byURL[tagURL] = verifiedEntry{at: time.Now(), tokens: m}
	return m
}

type jupToken struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Symbol     string `json:"symbol"`
	Icon       string `json:"icon"`
	Decimals   int    `json:"decimals"`
	IsVerified bool   `json:"isVerified"`
}

// jupiter makes one Jupiter token-search request for up to ~100 mints. Only entries whose id is
// one of the requested mints are used: the search is fuzzy by nature.
func (r *resolver) jupiter(ctx context.Context, mints []string) (map[string]TokenMeta, error) {
	u, err := url.Parse(r.jupURL)
	if err != nil {
		return nil, fmt.Errorf("jupiter URL: %w", err)
	}
	q := u.Query()
	q.Set("query", strings.Join(mints, ","))
	u.RawQuery = q.Encode()

	var toks []jupToken
	if err := r.http.GetJSON(ctx, u.String(), nil, &toks); err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(mints))
	for _, m := range mints {
		want[m] = true
	}
	out := map[string]TokenMeta{}
	for _, t := range toks {
		if !want[t.ID] {
			continue
		}
		sym := cleanText(t.Symbol, maxSymbolRunes)
		if sym == "" {
			continue
		}
		out[t.ID] = TokenMeta{
			Mint: t.ID, Symbol: sym, Name: cleanText(t.Name, maxNameRunes),
			LogoURL: safeHTTPURL(t.Icon), Decimals: t.Decimals, Verified: t.IsVerified, Source: sourceJupiter,
		}
	}
	return out, nil
}

// onChain names a mint Jupiter does not know, from the Token-2022 metadata extension (for
// Token-2022 mints) and then the Metaplex metadata account. jupFailed says Jupiter errored (as
// opposed to cleanly not listing the mint); then the result is not cached so the logo and
// verified flag are retried next time.
func (r *resolver) onChain(ctx context.Context, mb *mintBalance, jupFailed bool) resolution {
	failed := jupFailed
	var meta TokenMeta
	found := false
	if mb.Token2022 {
		m, ok, err := r.token2022(ctx, mb.Mint)
		failed = failed || err != nil
		meta, found = m, ok
	}
	if !found {
		m, ok, err := r.metaplex(ctx, mb.Mint)
		failed = failed || err != nil
		meta, found = m, ok
	}
	if !found {
		return fallbackResolution(mb, failed)
	}
	meta.Decimals = mb.Decimals
	if !jupFailed {
		r.cache.Put(mb.Mint, meta)
	}
	return resolution{meta: meta, lookupFailed: failed}
}

// token2022 reads the tokenMetadata extension from the parsed mint account.
func (r *resolver) token2022(ctx context.Context, mint string) (TokenMeta, bool, error) {
	var out struct {
		Value *struct {
			Data json.RawMessage `json:"data"`
		} `json:"value"`
	}
	err := r.rpc.call(ctx, "getAccountInfo", []any{mint, map[string]string{"encoding": "jsonParsed"}}, &out)
	if err != nil {
		return TokenMeta{}, false, err
	}
	if out.Value == nil {
		return TokenMeta{}, false, nil
	}
	var d struct {
		Parsed struct {
			Info struct {
				Decimals   int `json:"decimals"`
				Extensions []struct {
					Extension string          `json:"extension"`
					State     json.RawMessage `json:"state"`
				} `json:"extensions"`
			} `json:"info"`
		} `json:"parsed"`
	}
	if json.Unmarshal(out.Value.Data, &d) != nil {
		return TokenMeta{}, false, nil // unparsed data (e.g. an unsupported program): nothing to read
	}
	for _, e := range d.Parsed.Info.Extensions {
		if e.Extension != "tokenMetadata" {
			continue
		}
		var st struct {
			Name   string `json:"name"`
			Symbol string `json:"symbol"`
		}
		if json.Unmarshal(e.State, &st) != nil {
			continue
		}
		sym, name := cleanText(st.Symbol, maxSymbolRunes), cleanText(st.Name, maxNameRunes)
		if sym == "" && name == "" {
			continue
		}
		if sym == "" {
			sym = shortKey(mint)
		}
		return TokenMeta{Mint: mint, Symbol: sym, Name: name, Decimals: d.Parsed.Info.Decimals, Source: sourceToken2022}, true, nil
	}
	return TokenMeta{}, false, nil
}

// metaplex reads the Metaplex Token Metadata account at the mint's PDA.
func (r *resolver) metaplex(ctx context.Context, mint string) (TokenMeta, bool, error) {
	pda, err := metadataPDA(mint)
	if err != nil {
		return TokenMeta{}, false, err
	}
	var out struct {
		Value *struct {
			Data  []string `json:"data"`
			Owner string   `json:"owner"`
		} `json:"value"`
	}
	err = r.rpc.call(ctx, "getAccountInfo", []any{pda, map[string]string{"encoding": "base64"}}, &out)
	if err != nil {
		return TokenMeta{}, false, err
	}
	if out.Value == nil || out.Value.Owner != MetadataProgramID {
		return TokenMeta{}, false, nil
	}
	raw, err := decodeBase64Account(out.Value.Data)
	if err != nil {
		return TokenMeta{}, false, err
	}
	md, err := decodeMetaplexMetadata(raw)
	if err != nil {
		return TokenMeta{}, false, err
	}
	if md.Mint != mint || (md.Symbol == "" && md.Name == "") {
		return TokenMeta{}, false, nil
	}
	sym := md.Symbol
	if sym == "" {
		sym = shortKey(mint)
	}
	return TokenMeta{Mint: mint, Symbol: sym, Name: md.Name, Source: sourceMetaplex}, true, nil
}
