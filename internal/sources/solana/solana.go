// Package solana is a read-only Solana wallet source: native SOL, every SPL token (Token and
// Token-2022 programs), native stake accounts and an NFT count, with token identity resolved from
// the Jupiter token API, Token-2022 metadata, or Metaplex metadata accounts.
//
// Pricing is not done here. The aggregator prices holdings by AssetKey (exchange symbol for
// well-known mints, "spl:<mint>" for the rest) and decides what counts as spam.
package solana

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
	"crypto-portfolio/internal/sources/solana/defi"
)

const (
	defaultRPCURL           = "https://api.mainnet-beta.solana.com"
	defaultJupiterTokensURL = "https://lite-api.jup.ag/tokens/v2/search"

	lamportsDecimals = 9
)

// Config configures one Solana wallet.
type Config struct {
	ID, Label, Address string
	RPCURL             string // may embed an API key; only its host is ever displayed
	IncludeStake       bool
	HideMints          []string
	AllowMints         []string // always shown and counted, even if Jupiter hasn't verified them
	Overrides          map[string]config.TokenOverride
	JupiterTokensURL   string   // default "https://lite-api.jup.ag/tokens/v2/search"
	DeFi               []string // adapters to run: "jupiter", "orca", "raydium"; empty = none
	// JupiterVault is the wallet's Jupiter limit-order vault, if any: Limit V2 moves the funds
	// of open orders into a custodial account of the wallet's own, which nothing on chain
	// links back to the wallet. Its SOL and tokens are read as one position, and its token
	// accounts are watched by Activity as the wallet's own, so a deposit, a fill inside the
	// vault and a withdrawal read as a move, a swap and a move.
	JupiterVault string
}

// Source implements core.Source and core.AddressLister for one Solana address.
type Source struct {
	cfg   Config
	rpc   *rpcClient
	res   *resolver
	hide  map[string]bool
	allow map[string]bool

	defi    []defi.Adapter
	defiRPC defi.RPC

	mu       sync.Mutex
	nftMints []string
	warned   map[string]bool // keys warnOnce has logged
}

// New validates cfg and returns a source. c and cache may be nil (defaults: httpx.New() and an
// in-memory cache).
func New(cfg Config, c *httpx.Client, cache MetaCache) (*Source, error) {
	if _, err := decodePubkey(cfg.Address); err != nil {
		return nil, fmt.Errorf("solana: invalid address %q: %w", cfg.Address, err)
	}
	if cfg.JupiterVault != "" {
		if _, err := decodePubkey(cfg.JupiterVault); err != nil {
			return nil, fmt.Errorf("solana: invalid Jupiter vault address %q: %w", cfg.JupiterVault, err)
		}
		if cfg.JupiterVault == cfg.Address {
			return nil, errors.New("solana: the Jupiter vault is a separate account, not the wallet's own address")
		}
	}
	if cfg.RPCURL == "" {
		cfg.RPCURL = defaultRPCURL
	}
	// Never echo the URL back in an error: it may contain an API key.
	if u, err := url.Parse(cfg.RPCURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("solana: RPC URL must be an absolute http(s) URL")
	}
	if cfg.JupiterTokensURL == "" {
		cfg.JupiterTokensURL = defaultJupiterTokensURL
	}
	if c == nil {
		c = httpx.New()
	}
	if cache == nil {
		cache = NewMemoryMetaCache()
	}
	rpc := newRPCClient(cfg.RPCURL, c)
	s := &Source{
		cfg:   cfg,
		rpc:   rpc,
		res:   &resolver{rpc: rpc, http: c, jupURL: cfg.JupiterTokensURL, cache: cache},
		hide:  make(map[string]bool, len(cfg.HideMints)),
		allow: make(map[string]bool, len(cfg.AllowMints)),
	}
	for _, m := range cfg.AllowMints {
		s.allow[m] = true
	}
	if len(cfg.DeFi) > 0 {
		s.defi = defi.Adapters(cfg.DeFi, c)
		s.defiRPC = defi.NewHTTPRPC(cfg.RPCURL, c)
	}
	for _, m := range cfg.HideMints {
		s.hide[m] = true
	}
	return s, nil
}

// ID returns the configured wallet id.
func (s *Source) ID() string { return s.cfg.ID }

// LastNFTMints returns the mints of the NFTs seen by the most recent Fetch (decimals 0, amount
// exactly 1), sorted. DeFi adapters use it to find position NFTs.
func (s *Source) LastNFTMints() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.nftMints)
}

// warnOnce logs a warning the first time this source meets key, so a refusal that repeats on
// every sync is logged once.
func (s *Source) warnOnce(key, msg string, args ...any) {
	s.mu.Lock()
	seen := s.warned[key]
	if s.warned == nil {
		s.warned = map[string]bool{}
	}
	s.warned[key] = true
	s.mu.Unlock()
	if !seen {
		slog.Warn(msg, args...)
	}
}

func (s *Source) balance(ctx context.Context) (uint64, error) { return s.balanceOf(ctx, s.cfg.Address) }

func (s *Source) balanceOf(ctx context.Context, addr string) (uint64, error) {
	var out struct {
		Value uint64 `json:"value"`
	}
	if err := s.rpc.call(ctx, "getBalance", []any{addr}, &out); err != nil {
		return 0, fmt.Errorf("getBalance: %w", err)
	}
	return out.Value, nil
}

// Addresses lists the watched address, and the Jupiter vault when there is one, with their
// SOL balances.
func (s *Source) Addresses(ctx context.Context) ([]core.Address, error) {
	var out []core.Address
	for _, addr := range s.owners() {
		lamports, err := s.balanceOf(ctx, addr)
		if err != nil {
			return nil, err
		}
		sol, _ := strconv.ParseFloat(formatAmount(new(big.Int).SetUint64(lamports), lamportsDecimals), 64)
		out = append(out, core.Address{Address: addr, Balance: sol})
	}
	return out, nil
}

// owners are the addresses whose balances are the wallet's: its own, then the Jupiter vault
// when configured.
func (s *Source) owners() []string {
	if s.cfg.JupiterVault == "" {
		return []string{s.cfg.Address}
	}
	return []string{s.cfg.Address, s.cfg.JupiterVault}
}

// Fetch reads SOL, SPL tokens and (optionally) stake accounts. Failures of the balance or token
// account calls fail the fetch; stake and metadata problems never do.
func (s *Source) Fetch(ctx context.Context) (core.WalletResult, error) {
	res := core.WalletResult{WalletID: s.cfg.ID}

	lamports, err := s.balance(ctx)
	if err != nil {
		return res, err
	}
	ts, err := s.tokenAccounts(ctx)
	if err != nil {
		return res, err
	}

	var stakeLamports *big.Int
	var stakeKeys []string
	stakeOK := false
	if s.cfg.IncludeStake {
		stakeLamports, stakeKeys, err = s.stakeAccounts(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.Warnings = append(res.Warnings, stakeWarning)
		} else {
			stakeOK = true
		}
	}
	if ts.skipped > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("Ignored %d token accounts with unreadable balances", ts.skipped))
	}

	// DeFi positions first: they claim position NFTs and LP tokens, which then don't
	// appear as plain tokens or count as NFTs.
	positions, claimed, defiWarnings := s.defiPositions(ctx, ts)
	res.Warnings = append(res.Warnings, defiWarnings...)

	// Native SOL: wallet balance plus wrapped SOL, in lamports.
	solLamports := new(big.Int).SetUint64(lamports)
	var tokens []*mintBalance
	var nfts []string
	for _, mint := range slices.Sorted(maps.Keys(ts.byMint)) {
		mb := ts.byMint[mint]
		switch {
		case claimed[mint]:
			// Accounted for by a DeFi position.
		case mint == WrappedSOLMint:
			solLamports.Add(solLamports, mb.Amount)
		case mb.Amount.Sign() == 0:
			// Closed-out or empty account: nothing to show, nothing worth a lookup.
		case mb.Decimals == 0 && mb.Amount.Cmp(big.NewInt(1)) == 0:
			nfts = append(nfts, mint)
		default:
			tokens = append(tokens, mb)
		}
	}

	res.Holdings = append(res.Holdings, s.solHolding(solLamports, false))
	if stakeOK && stakeLamports.Sign() > 0 {
		res.Holdings = append(res.Holdings, s.solHolding(stakeLamports, true))
	}

	resolved := s.res.resolveAll(ctx, tokens)
	if err := ctx.Err(); err != nil {
		return res, err
	}
	unnamed := 0
	var shown, hidden []core.Holding
	for _, mb := range tokens {
		h, named := s.tokenHolding(mb, resolved[mb.Mint])
		if !named {
			unnamed++
		}
		if s.hide[mb.Mint] {
			hidden = append(hidden, h)
		} else {
			shown = append(shown, h)
		}
	}
	if unnamed > 0 {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("Names pending for %d tokens; showing shortened mints until lookups catch up", unnamed))
	}
	sortHoldings(shown)
	sortHoldings(hidden)
	res.Holdings = append(res.Holdings, shown...)
	res.Holdings = append(res.Holdings, positions...)
	if s.cfg.JupiterVault != "" {
		vault, err := s.vaultHoldings(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.Warnings = append(res.Warnings, vaultWarning)
		}
		res.Holdings = append(res.Holdings, vault...)
	}
	res.Hidden = hidden
	res.NFTCount = len(nfts)

	stakeMeta := "unavailable"
	if stakeOK {
		stakeMeta = strconv.Itoa(len(stakeKeys))
	}
	res.Meta = []core.KV{
		{K: "Address", V: shortKey(s.cfg.Address)},
		{K: "RPC", V: hostOnly(s.cfg.RPCURL)},
		{K: "Token accounts", V: strconv.Itoa(ts.accounts)},
	}
	if s.cfg.IncludeStake {
		res.Meta = append(res.Meta, core.KV{K: "Stake accounts", V: stakeMeta})
	}
	if s.cfg.JupiterVault != "" {
		res.Meta = append(res.Meta, core.KV{K: "Limit-order vault", V: shortKey(s.cfg.JupiterVault)})
	}
	res.AsOf = time.Now().UTC()

	s.mu.Lock()
	s.nftMints = nfts
	s.mu.Unlock()
	return res, nil
}

// vaultWarning is shown when the Jupiter vault could not be read; the wallet's own balances
// still stand.
const vaultWarning = "Couldn't read the Jupiter limit-order vault; its balances are left out for now"

// vaultHoldings reads the Jupiter limit-order vault: its SOL (wrapped SOL included) and its
// tokens, tagged as one position of the wallet. Hidden mints and NFTs are left out.
func (s *Source) vaultHoldings(ctx context.Context) ([]core.Holding, error) {
	vault := s.cfg.JupiterVault
	lamports, err := s.balanceOf(ctx, vault)
	if err != nil {
		return nil, err
	}
	ts, err := s.tokenAccountsOf(ctx, vault)
	if err != nil {
		return nil, err
	}
	sol := new(big.Int).SetUint64(lamports)
	var tokens []*mintBalance
	for _, mint := range slices.Sorted(maps.Keys(ts.byMint)) {
		mb := ts.byMint[mint]
		switch {
		case mint == WrappedSOLMint:
			sol.Add(sol, mb.Amount)
		case mb.Amount.Sign() == 0, s.hide[mint]:
		case mb.Decimals == 0 && mb.Amount.Cmp(big.NewInt(1)) == 0:
			// An NFT: not what a limit order holds.
		default:
			tokens = append(tokens, mb)
		}
	}
	tag := func(h core.Holding) core.Holding {
		h.Vault = true
		h.Protocol, h.Position, h.PositionKind = "jupiter", shortKey(vault), defi.KindLimit
		h.Note = "Limit-order vault"
		return h
	}
	var out []core.Holding
	if sol.Sign() > 0 {
		out = append(out, tag(s.solHolding(sol, false)))
	}
	resolved := s.res.resolveAll(ctx, tokens)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var hs []core.Holding
	for _, mb := range tokens {
		h, _ := s.tokenHolding(mb, resolved[mb.Mint])
		hs = append(hs, tag(h))
	}
	sortHoldings(hs)
	return append(out, hs...), nil
}

func (s *Source) solHolding(lamports *big.Int, staked bool) core.Holding {
	str := formatAmount(lamports, lamportsDecimals)
	amt, _ := strconv.ParseFloat(str, 64)
	return core.Holding{
		WalletID: s.cfg.ID, AssetKey: "SOL", Symbol: "SOL", Name: "Solana",
		Amount: amt, AmountStr: str, Staked: staked,
	}
}

// tokenHolding builds the holding for one mint. named is false when the token ended up with only
// a shortened mint for a name because a lookup failed (so the caller can warn once).
func (s *Source) tokenHolding(mb *mintBalance, r resolution) (h core.Holding, named bool) {
	str := formatAmount(mb.Amount, mb.Decimals)
	amt, _ := strconv.ParseFloat(str, 64)
	h = core.Holding{
		WalletID: s.cfg.ID, AssetKey: core.SPLKey(mb.Mint), Mint: mb.Mint,
		Symbol: r.meta.Symbol, Name: r.meta.Name, LogoURL: r.meta.LogoURL,
		Amount: amt, AmountStr: str, Verified: r.meta.Verified,
	}
	named = !(r.fallback && r.lookupFailed)
	// Well-known mints take the exchange ticker, so they merge with exchange balances. This is
	// keyed by mint only: a look-alike "USDC" on another mint never gets the "USDC" key.
	if sym, name, ok := KnownMint(mb.Mint); ok {
		h.AssetKey, h.Symbol, h.Name = sym, sym, name
		h.Verified = true
		named = true
	}
	if s.allow[mb.Mint] {
		h.Verified = true
	}
	if o, ok := s.cfg.Overrides[mb.Mint]; ok {
		if o.Symbol != "" {
			h.Symbol = cleanText(o.Symbol, maxSymbolRunes)
			named = true
		}
		if o.Name != "" {
			h.Name = cleanText(o.Name, maxNameRunes)
		}
	}
	if h.Symbol == "" {
		h.Symbol = shortKey(mb.Mint)
	}
	return h, named
}

func sortHoldings(hs []core.Holding) {
	slices.SortFunc(hs, func(a, b core.Holding) int {
		return cmp.Or(cmp.Compare(a.Symbol, b.Symbol), cmp.Compare(a.Mint, b.Mint))
	})
}
