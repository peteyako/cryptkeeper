package solana

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources/solana/defi"
)

// Compile-time checks that the source plugs into the rest of the app.
var (
	_ core.Source        = (*Source)(nil)
	_ core.AddressLister = (*Source)(nil)
)

func find(hs []core.Holding, assetKey string, staked bool) (core.Holding, bool) {
	for _, h := range hs {
		if h.AssetKey == assetKey && h.Staked == staked {
			return h, true
		}
	}
	return core.Holding{}, false
}

func mustFind(t *testing.T, hs []core.Holding, assetKey string, staked bool) core.Holding {
	t.Helper()
	h, ok := find(hs, assetKey, staked)
	if !ok {
		t.Fatalf("no holding with AssetKey=%q staked=%v in %+v", assetKey, staked, hs)
	}
	return h
}

func meta(res core.WalletResult, k string) (string, bool) {
	for _, kv := range res.Meta {
		if kv.K == k {
			return kv.V, true
		}
	}
	return "", false
}

// richWallet is the wallet from the plan's step 5: SOL+wSOL, two USDC accounts, a Token-2022
// token with a metadata extension, a Metaplex-named token, a fake "USDC", an NFT, a zero
// balance, a spam token and two stake accounts.
type rich struct {
	t22, mpx, fakeUSDC, nft, zero, spam string
}

func richWallet(t *testing.T) (*fakeChain, rich) {
	t.Helper()
	f := newFakeChain(t)
	w := rich{
		t22: testMint("token2022"), mpx: testMint("metaplex"), fakeUSDC: testMint("fake-usdc"),
		nft: testMint("nft"), zero: testMint("zero"), spam: testMint("spam"),
	}
	f.balance = 2_500_000_000 // 2.5 SOL

	// wSOL (1.25) merges into SOL.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: WrappedSOLMint, amount: "1250000000", decimals: 9})
	// Two USDC accounts: 1.5 + 2.25.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: usdcMint, amount: "1500000", decimals: 6})
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: usdcMint, amount: "2250000", decimals: 6})
	f.loadJupiterFixture(usdcMint, "jupiter_usdc.json")
	// Token-2022 token named by its tokenMetadata extension.
	f.addTokenAccount(Token2022ProgramID, fakeAcct{mint: w.t22, amount: "5000000000", decimals: 9})
	f.mintInfo[w.t22] = token2022MintInfo(9, "Token Two", "TWO")
	// Unknown token named by a Metaplex metadata account.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: w.mpx, amount: "42000000", decimals: 6})
	f.addMetaplex(w.mpx, "Metaplex Coin", "MPX")
	// A fake "USDC" on a different mint.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: w.fakeUSDC, amount: "999000000", decimals: 6})
	f.addMetaplex(w.fakeUSDC, "Fake USD Coin", "USDC")
	// NFT, zero balance and spam.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: w.nft, amount: "1", decimals: 0})
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: w.zero, amount: "0", decimals: 6})
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: w.spam, amount: "1000000", decimals: 6})
	// Two stake accounts.
	f.stake = []fakeStakeAcct{{"StakeAcct1", 1_001_666_240}, {"StakeAcct2", 5_000_000_000}}
	return f, w
}

func TestFetchRichWallet(t *testing.T) {
	f, w := richWallet(t)
	s := f.source(func(c *Config) { c.HideMints = []string{w.spam} })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WalletID != "phantom" || res.AsOf.IsZero() || res.Err != "" || res.Stale {
		t.Errorf("result header = %+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}
	for _, h := range append(append([]core.Holding{}, res.Holdings...), res.Hidden...) {
		if h.WalletID != "phantom" {
			t.Errorf("holding %s has WalletID %q", h.AssetKey, h.WalletID)
		}
	}

	// SOL + wSOL merged: 2.5 + 1.25.
	sol := mustFind(t, res.Holdings, "SOL", false)
	if sol.Amount != 3.75 || sol.AmountStr != "3.75" || sol.Symbol != "SOL" || sol.Name != "Solana" || sol.Mint != "" {
		t.Errorf("SOL = %+v", sol)
	}
	// Staked SOL: 1.00166624 + 5.
	staked := mustFind(t, res.Holdings, "SOL", true)
	if staked.Amount != 6.00166624 || staked.AmountStr != "6.00166624" || staked.Symbol != "SOL" || !staked.Staked {
		t.Errorf("staked SOL = %+v", staked)
	}
	// SOL first, then staked SOL.
	if res.Holdings[0].AssetKey != "SOL" || res.Holdings[0].Staked || !res.Holdings[1].Staked {
		t.Errorf("order: %+v", res.Holdings[:2])
	}

	// Two USDC accounts summed, mapped to the exchange symbol, identity from Jupiter.
	usdc := mustFind(t, res.Holdings, "USDC", false)
	if usdc.Amount != 3.75 || usdc.AmountStr != "3.75" || usdc.Symbol != "USDC" || usdc.Name != "USD Coin" || usdc.Mint != usdcMint {
		t.Errorf("USDC = %+v", usdc)
	}
	if !strings.HasSuffix(usdc.LogoURL, "/logo.png") {
		t.Errorf("USDC LogoURL = %q", usdc.LogoURL)
	}

	// Token-2022 token named from its tokenMetadata extension.
	t22 := mustFind(t, res.Holdings, core.SPLKey(w.t22), false)
	if t22.Symbol != "TWO" || t22.Name != "Token Two" || t22.Amount != 5 || t22.Mint != w.t22 {
		t.Errorf("token-2022 = %+v", t22)
	}
	// Unknown token named by Metaplex.
	mpx := mustFind(t, res.Holdings, core.SPLKey(w.mpx), false)
	if mpx.Symbol != "MPX" || mpx.Name != "Metaplex Coin" || mpx.Amount != 42 || mpx.AmountStr != "42" {
		t.Errorf("metaplex token = %+v", mpx)
	}
	// The fake USDC keeps its own mint-based key; it must never be keyed "USDC".
	fake := mustFind(t, res.Holdings, core.SPLKey(w.fakeUSDC), false)
	if fake.Symbol != "USDC" || fake.Name != "Fake USD Coin" || fake.Amount != 999 {
		t.Errorf("fake USDC = %+v", fake)
	}
	n := 0
	for _, h := range res.Holdings {
		if h.AssetKey == "USDC" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d holdings keyed USDC, want exactly 1", n)
	}

	// NFT: counted, not a holding. Zero balance dropped. Spam hidden.
	if res.NFTCount != 1 {
		t.Errorf("NFTCount = %d", res.NFTCount)
	}
	if got := s.LastNFTMints(); len(got) != 1 || got[0] != w.nft {
		t.Errorf("LastNFTMints = %v", got)
	}
	for _, h := range res.Holdings {
		if h.Mint == w.nft || h.Mint == w.zero || h.Mint == w.spam {
			t.Errorf("unexpected holding %+v", h)
		}
	}
	if len(res.Hidden) != 1 || res.Hidden[0].Mint != w.spam || res.Hidden[0].Amount != 1 {
		t.Fatalf("Hidden = %+v", res.Hidden)
	}
	if res.Hidden[0].Symbol != shortKey(w.spam) {
		t.Errorf("hidden fallback symbol = %q, want %q", res.Hidden[0].Symbol, shortKey(w.spam))
	}
	if len(res.Holdings) != 6 { // SOL, staked SOL, USDC, token-2022, metaplex, fake USDC
		t.Errorf("%d holdings: %+v", len(res.Holdings), res.Holdings)
	}

	// Meta: ordered, host-only RPC, no key anywhere.
	wantMeta := []core.KV{
		{K: "Address", V: shortKey(testOwner)},
		{K: "RPC", V: strings.TrimPrefix(f.srv.URL, "http://")},
		{K: "Token accounts", V: "9"},
		{K: "Stake accounts", V: "2"},
	}
	if fmt.Sprint(res.Meta) != fmt.Sprint(wantMeta) {
		t.Errorf("Meta = %v\nwant   %v", res.Meta, wantMeta)
	}
	if strings.Contains(fmt.Sprintf("%+v", res), testRPCKey) {
		t.Errorf("RPC key leaked into the result: %+v", res)
	}

	// Classic mints are never probed with jsonParsed getAccountInfo; only the Token-2022 mint is.
	var parsedProbes []string
	for _, r := range f.infoRequests() {
		if strings.HasSuffix(r, "|jsonParsed") {
			parsedProbes = append(parsedProbes, strings.TrimSuffix(r, "|jsonParsed"))
		}
	}
	if len(parsedProbes) != 1 || parsedProbes[0] != w.t22 {
		t.Errorf("jsonParsed mint probes = %v, want only %s", parsedProbes, w.t22)
	}
	// The zero-balance mint and the NFT never trigger a metadata lookup.
	for _, q := range f.jupiterQueries() {
		for _, m := range q {
			if m == w.zero || m == w.nft {
				t.Errorf("metadata looked up for %s", m)
			}
		}
	}
}

func TestFetchStakeUnavailableIsAWarning(t *testing.T) {
	for _, mode := range []string{"rpc", "http"} {
		t.Run(mode, func(t *testing.T) {
			f, w := richWallet(t)
			f.stakeFail = mode
			s := f.source(func(c *Config) { c.HideMints = []string{w.spam} })

			res, err := s.Fetch(context.Background())
			if err != nil {
				t.Fatalf("stake failure must not fail the fetch: %v", err)
			}
			if len(res.Warnings) != 1 || res.Warnings[0] != "Stake accounts unavailable on this RPC (use Helius)" {
				t.Errorf("warnings = %q", res.Warnings)
			}
			if _, ok := find(res.Holdings, "SOL", true); ok {
				t.Error("staked SOL present despite stake failure")
			}
			if sol := mustFind(t, res.Holdings, "SOL", false); sol.Amount != 3.75 {
				t.Errorf("SOL = %+v", sol)
			}
			mustFind(t, res.Holdings, "USDC", false)
			if v, _ := meta(res, "Stake accounts"); v != "unavailable" {
				t.Errorf("Stake accounts meta = %q", v)
			}
			if strings.Contains(fmt.Sprintf("%+v", res), testRPCKey) {
				t.Error("RPC key leaked")
			}
		})
	}
}

func TestFetchWithoutStakeDoesNotCallGetProgramAccounts(t *testing.T) {
	f, _ := richWallet(t)
	s := f.source(func(c *Config) { c.IncludeStake = false })
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.callCount("getProgramAccounts") != 0 {
		t.Error("getProgramAccounts called although IncludeStake is false")
	}
	if _, ok := find(res.Holdings, "SOL", true); ok {
		t.Error("staked SOL present")
	}
	if _, ok := meta(res, "Stake accounts"); ok {
		t.Error("Stake accounts meta should be omitted when stake is not included")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestFetchNoStakeAccountsYieldsNoStakedHolding(t *testing.T) {
	f := newFakeChain(t)
	f.balance = 1_000_000_000
	s := f.source(nil)
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := find(res.Holdings, "SOL", true); ok {
		t.Error("staked SOL present with no stake accounts")
	}
	if v, _ := meta(res, "Stake accounts"); v != "0" {
		t.Errorf("Stake accounts = %q", v)
	}
}

func TestFetchEmptyWalletStillHasSOL(t *testing.T) {
	f := newFakeChain(t)
	s := f.source(func(c *Config) { c.IncludeStake = false })
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 {
		t.Fatalf("holdings = %+v", res.Holdings)
	}
	if h := res.Holdings[0]; h.AssetKey != "SOL" || h.Amount != 0 || h.AmountStr != "0" || h.Symbol != "SOL" {
		t.Errorf("SOL = %+v", h)
	}
}

func TestFetchRPCErrorsFailTheFetch(t *testing.T) {
	for _, method := range []string{"getBalance", "getTokenAccountsByOwner"} {
		t.Run(method, func(t *testing.T) {
			f, _ := richWallet(t)
			f.failMethods[method] = "node is behind by 4000 slots"
			s := f.source(nil)
			_, err := s.Fetch(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "node is behind by 4000 slots") {
				t.Errorf("error should carry the RPC message: %v", err)
			}
			if strings.Contains(err.Error(), testRPCKey) {
				t.Errorf("error leaks the RPC key: %v", err)
			}
		})
	}
}

func TestFetchSumsExactlyAndMergesWSOLEvenWithoutNativeBalance(t *testing.T) {
	f := newFakeChain(t)
	m := testMint("fine")
	f.addMetaplex(m, "Fine", "FINE")
	// 18 decimals: 0.1 + 0.2 must be exactly 0.3, and a >2^64 total must not overflow.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: m, amount: "100000000000000000", decimals: 18})
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: m, amount: "200000000000000000", decimals: 18})
	f.addTokenAccount(Token2022ProgramID, fakeAcct{mint: m, amount: "18446744073709551615", decimals: 18})
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: WrappedSOLMint, amount: "1", decimals: 9})
	s := f.source(func(c *Config) { c.IncludeStake = false })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := mustFind(t, res.Holdings, core.SPLKey(m), false)
	if h.AmountStr != "18.746744073709551615" {
		t.Errorf("AmountStr = %q", h.AmountStr)
	}
	if sol := mustFind(t, res.Holdings, "SOL", false); sol.AmountStr != "0.000000001" {
		t.Errorf("SOL AmountStr = %q (wSOL dust must merge even with 0 native balance)", sol.AmountStr)
	}
}

func TestFetchNFTsAreCountedAcrossBothPrograms(t *testing.T) {
	f := newFakeChain(t)
	nftA, nftB := testMint("nftA"), testMint("nftB")
	semi := testMint("semi-fungible")
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: nftA, amount: "1", decimals: 0})
	f.addTokenAccount(Token2022ProgramID, fakeAcct{mint: nftB, amount: "1", decimals: 0})
	// decimals 0 but 5 units: a fungible-ish token, not an NFT.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: semi, amount: "5", decimals: 0})
	// decimals 6 with exactly 0.000001 is a token, not an NFT.
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: testMint("dust"), amount: "1", decimals: 6})
	s := f.source(func(c *Config) { c.IncludeStake = false })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.NFTCount != 2 {
		t.Errorf("NFTCount = %d", res.NFTCount)
	}
	got := s.LastNFTMints()
	if len(got) != 2 || !((got[0] == nftA && got[1] == nftB) || (got[0] == nftB && got[1] == nftA)) {
		t.Errorf("LastNFTMints = %v", got)
	}
	// The returned slice is a copy.
	got[0] = "tampered"
	if again := s.LastNFTMints(); again[0] == "tampered" {
		t.Error("LastNFTMints exposes internal state")
	}
	mustFind(t, res.Holdings, core.SPLKey(semi), false)
	mustFind(t, res.Holdings, core.SPLKey(testMint("dust")), false)
}

func TestMetadataFailuresNeverFailTheFetch(t *testing.T) {
	f, w := richWallet(t)
	f.jupStatus = 500
	f.failInfo = true // every getAccountInfo errors too
	s := f.source(func(c *Config) { c.HideMints = []string{w.spam} })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("metadata failure failed the fetch: %v", err)
	}
	// Known mint keeps its exchange identity from the static table even with every lookup down.
	usdc := mustFind(t, res.Holdings, "USDC", false)
	if usdc.Symbol != "USDC" || usdc.Amount != 3.75 {
		t.Errorf("USDC = %+v", usdc)
	}
	// Unknown mints fall back to the shortened mint.
	h := mustFind(t, res.Holdings, core.SPLKey(w.mpx), false)
	if h.Symbol != shortKey(w.mpx) || h.Amount != 42 {
		t.Errorf("fallback = %+v", h)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Names pending") {
		t.Errorf("expected one token-name warning, got %q", res.Warnings)
	}
}

func TestMetadataIsCached(t *testing.T) {
	f := newFakeChain(t)
	t22, mpx := testMint("t22"), testMint("mpx")
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: usdcMint, amount: "1000000", decimals: 6})
	f.loadJupiterFixture(usdcMint, "jupiter_usdc.json")
	f.addTokenAccount(Token2022ProgramID, fakeAcct{mint: t22, amount: "1000000000", decimals: 9})
	f.mintInfo[t22] = token2022MintInfo(9, "Token Two", "TWO")
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: mpx, amount: "1000000", decimals: 6})
	f.addMetaplex(mpx, "Metaplex Coin", "MPX")
	s := f.source(func(c *Config) { c.IncludeStake = false })

	first, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	jupBefore, infoBefore := f.callCount("jupiter"), f.callCount("getAccountInfo")
	if jupBefore == 0 || infoBefore == 0 {
		t.Fatalf("first fetch made no lookups (jup=%d info=%d)", jupBefore, infoBefore)
	}

	second, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.callCount("jupiter") != jupBefore || f.callCount("getAccountInfo") != infoBefore {
		t.Errorf("second fetch re-did lookups: jup %d->%d info %d->%d",
			jupBefore, f.callCount("jupiter"), infoBefore, f.callCount("getAccountInfo"))
	}
	if fmt.Sprint(first.Holdings) != fmt.Sprint(second.Holdings) {
		t.Errorf("cached fetch differs:\n%+v\n%+v", first.Holdings, second.Holdings)
	}
	if h := mustFind(t, second.Holdings, core.SPLKey(t22), false); h.Symbol != "TWO" {
		t.Errorf("cached token-2022 = %+v", h)
	}
}

func TestFallbackIdentityIsNotCached(t *testing.T) {
	f := newFakeChain(t)
	m := testMint("late-metadata")
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: m, amount: "1000000", decimals: 6})
	s := f.source(func(c *Config) { c.IncludeStake = false })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h := mustFind(t, res.Holdings, core.SPLKey(m), false); h.Symbol != shortKey(m) {
		t.Fatalf("symbol = %q", h.Symbol)
	}
	// Metadata appears later; the next fetch must pick it up rather than serve a cached fallback.
	f.addMetaplex(m, "Late Coin", "LATE")
	res, err = s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h := mustFind(t, res.Holdings, core.SPLKey(m), false); h.Symbol != "LATE" {
		t.Errorf("symbol = %q, want LATE", h.Symbol)
	}
}

func TestOnChainMetadataFromJupiterOutageIsNotCached(t *testing.T) {
	// If Jupiter errored, an on-chain name is used for display but not cached, so the logo and
	// verified flag are retried on the next fetch instead of being missing for the whole TTL.
	f := newFakeChain(t)
	m := testMint("flaky-jup")
	f.addTokenAccount(TokenProgramID, fakeAcct{mint: m, amount: "1000000", decimals: 6})
	f.addMetaplex(m, "Flaky", "FLK")
	f.jupStatus = 503
	s := f.source(func(c *Config) { c.IncludeStake = false })

	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.jupStatus = 0
	f.addJupiter(m, "FLK", "Flaky Token", 6, true)
	f.mu.Unlock()
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := mustFind(t, res.Holdings, core.SPLKey(m), false)
	if h.Name != "Flaky Token" || h.LogoURL == "" {
		t.Errorf("holding = %+v (Jupiter identity should win once it is back)", h)
	}
}

func TestOverridesWinOverEverything(t *testing.T) {
	f, w := richWallet(t)
	s := f.source(func(c *Config) {
		c.HideMints = []string{w.spam}
		c.Overrides = map[string]config.TokenOverride{
			w.mpx:    {Symbol: "MINE", Name: "My Coin"},
			usdcMint: {Name: "Circle Dollar"}, // name only: symbol stays
			w.spam:   {Symbol: "JUNK"},
		}
	})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mpx := mustFind(t, res.Holdings, core.SPLKey(w.mpx), false)
	if mpx.Symbol != "MINE" || mpx.Name != "My Coin" {
		t.Errorf("override on unknown token: %+v", mpx)
	}
	usdc := mustFind(t, res.Holdings, "USDC", false)
	if usdc.Symbol != "USDC" || usdc.Name != "Circle Dollar" {
		t.Errorf("name-only override on known mint: %+v", usdc)
	}
	if len(res.Hidden) != 1 || res.Hidden[0].Symbol != "JUNK" {
		t.Errorf("override on hidden token: %+v", res.Hidden)
	}
}

func TestMetadataLookupConcurrencyIsBounded(t *testing.T) {
	f := newFakeChain(t)
	f.infoDelay = 15 * time.Millisecond
	for i := 0; i < 12; i++ {
		m := testMint(fmt.Sprintf("many-%d", i))
		f.addTokenAccount(TokenProgramID, fakeAcct{mint: m, amount: "1000000", decimals: 6})
		f.addMetaplex(m, fmt.Sprintf("Token %d", i), fmt.Sprintf("T%d", i))
	}
	s := f.source(func(c *Config) { c.IncludeStake = false })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 13 { // 12 tokens + SOL
		t.Errorf("holdings = %d", len(res.Holdings))
	}
	f.mu.Lock()
	max := f.maxInflight
	f.mu.Unlock()
	if max > 4 {
		t.Errorf("max concurrent metadata lookups = %d, want <= 4", max)
	}
	if max < 2 {
		t.Errorf("max concurrent metadata lookups = %d, lookups are not running concurrently", max)
	}
}

func TestJupiterLookupsAreBatched(t *testing.T) {
	f := newFakeChain(t)
	want := map[string]bool{}
	for i := 0; i < 120; i++ {
		m := testMint(fmt.Sprintf("batch-%d", i))
		want[m] = true
		f.addTokenAccount(TokenProgramID, fakeAcct{mint: m, amount: "1000000", decimals: 6})
		f.addJupiter(m, fmt.Sprintf("B%d", i), fmt.Sprintf("Batch %d", i), 6, true)
	}
	s := f.source(func(c *Config) { c.IncludeStake = false })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	queries := f.jupiterQueries()
	if len(queries) != 3 {
		t.Errorf("%d Jupiter requests for 120 mints, want 3", len(queries))
	}
	seen := map[string]bool{}
	for _, q := range queries {
		if len(q) > 50 {
			t.Errorf("batch of %d mints", len(q))
		}
		for _, m := range q {
			seen[m] = true
		}
	}
	if len(seen) != 120 {
		t.Errorf("queried %d distinct mints", len(seen))
	}
	if f.callCount("getAccountInfo") != 0 {
		t.Error("Jupiter hits should not need on-chain lookups")
	}
	if h := mustFind(t, res.Holdings, core.SPLKey(testMint("batch-7")), false); h.Symbol != "B7" {
		t.Errorf("holding = %+v", h)
	}
}

func TestAddresses(t *testing.T) {
	f, _ := richWallet(t)
	s := f.source(nil)
	got, err := s.Addresses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != testOwner || got[0].Balance != 2.5 {
		t.Errorf("Addresses = %+v", got)
	}
}

func TestNewValidatesAddressAndDefaults(t *testing.T) {
	bad := []string{
		"",
		"not base58 !!",
		"abc", // too short
		"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWMx", // 45 chars, decodes to 33 bytes
		"0OIl0OIl0OIl0OIl0OIl0OIl0OIl0OIl0OIl0OIl0OIl",  // chars outside the alphabet
	}
	for _, a := range bad {
		if _, err := New(Config{ID: "x", Address: a}, nil, nil); err == nil {
			t.Errorf("New accepted address %q", a)
		}
	}
	s, err := New(Config{ID: "x", Address: testOwner}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "x" {
		t.Errorf("ID = %q", s.ID())
	}
	if s.cfg.RPCURL != "https://api.mainnet-beta.solana.com" {
		t.Errorf("default RPC = %q", s.cfg.RPCURL)
	}
	if s.cfg.JupiterTokensURL != "https://lite-api.jup.ag/tokens/v2/search" {
		t.Errorf("default Jupiter URL = %q", s.cfg.JupiterTokensURL)
	}
	if _, err := New(Config{ID: "x", Address: testOwner, RPCURL: "ftp://nope"}, nil, nil); err == nil {
		t.Error("non-http RPC URL accepted")
	}
}

func TestFetchHonoursContextCancellation(t *testing.T) {
	f, _ := richWallet(t)
	s := f.source(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Fetch(ctx); err == nil {
		t.Error("expected an error from a cancelled context")
	}
}

func TestFetchJupiterVault(t *testing.T) {
	f := newFakeChain(t)
	f.balance = 2_000_000_000
	f.vault = testMint("vault")
	f.vaultBalance = 10_000_000 // rent
	f.addVaultAccount(TokenProgramID, fakeAcct{mint: WrappedSOLMint, amount: "40000000000", decimals: 9})
	f.addVaultAccount(TokenProgramID, fakeAcct{mint: usdcMint, amount: "0", decimals: 6})
	s := f.source(func(c *Config) { c.JupiterVault = f.vault; c.IncludeStake = false })

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var plain, vault []core.Holding
	for _, h := range res.Holdings {
		if h.Vault {
			vault = append(vault, h)
		} else {
			plain = append(plain, h)
		}
	}
	if len(plain) != 1 || plain[0].AssetKey != "SOL" || plain[0].Amount != 2 {
		t.Fatalf("wallet holdings = %+v", plain)
	}
	if len(vault) != 1 {
		t.Fatalf("vault holdings = %+v", vault)
	}
	v := vault[0]
	if v.AssetKey != "SOL" || v.Amount != 40.01 || v.Protocol != "jupiter" || v.PositionKind != defi.KindLimit ||
		v.Position != shortKey(f.vault) || v.Note != "Limit-order vault" {
		t.Errorf("vault SOL = %+v", v)
	}
	if !slices.Contains(res.Meta, core.KV{K: "Limit-order vault", V: shortKey(f.vault)}) {
		t.Errorf("meta = %+v", res.Meta)
	}
	addrs, err := s.Addresses(context.Background())
	if err != nil || len(addrs) != 2 || addrs[1].Address != f.vault || addrs[1].Balance != 0.01 {
		t.Errorf("addresses = %+v (%v)", addrs, err)
	}

	// The vault being unreadable is a warning: the wallet's own balances still stand.
	f.mu.Lock()
	f.vaultDown = true
	f.mu.Unlock()
	res, err = s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Holdings {
		if h.Vault {
			t.Errorf("vault row despite the vault being down: %+v", h)
		}
	}
	if !slices.Contains(res.Warnings, vaultWarning) {
		t.Errorf("warnings = %q", res.Warnings)
	}
}

func TestNewRejectsVaultEqualToAddress(t *testing.T) {
	if _, err := New(Config{ID: "w", Label: "w", Address: testOwner, JupiterVault: testOwner}, nil, nil); err == nil {
		t.Fatal("the vault must not be the wallet's own address")
	}
	if _, err := New(Config{ID: "w", Label: "w", Address: testOwner, JupiterVault: "not a key"}, nil, nil); err == nil {
		t.Fatal("an invalid vault address must be refused")
	}
}
