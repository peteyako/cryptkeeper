package defi

import (
	"context"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

const (
	raySOLUSDCPool = "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"
	// Three real positions in the Raydium SOL/USDC CLMM pool (captured 2026-10-16), by NFT mint.
	rayMintIn    = "HaRZVFCxgBs7XB1DW5mnVkoZSfGczaoMR73w1kVbN9UE" // position Faq8a…, ticks [-22951, -21095)
	rayMintBelow = "5t6fsYZJrEmuqWGhC3LacfW6BHtr2AtfPgLjF4cjymEw" // position Fkkm…, ticks [-16110, -15109)
	rayMintAbove = "81vrQtLF91sWPUUdJoicqM1SQuuemYAf8chMSTvfMojz" // position Hqxym…, ticks [-26313, -23230)
	rayPosIn     = "Faq8aCWF19Jyc8pcPQXHF7EnZNJe42mbuDodHm4ScCKL"
	rayPosBelow  = "FkkmjY7S9ALxZrWNYAQXBxn1m68tDBAtdQ9BSmE1Kkqe"
	rayPosAbove  = "Hqxym7DxyQr13Q4PWpwJWCzyvjrg7x4g1NGzE5ud7kJK"

	// LP mints in raydium_lps_api.json.
	raySOLUSDCLP  = "8HoQnePLqPj4M7PUDzfw8e3Ymdwgc7NLGnaTUapubyvu" // AMM v4 pool 58oQCh…
	rayNEARUSDCLP = "ABuX9ddQpXps92PyMxeuWimTeeCyL4WD176P9AZhpxm2" // CPMM pool gyYigq…
	nearMint      = "3ZLekZYq2qkZiSpnSvabjit34tUkjSwD1JFuW9as9wBG"
)

// Expected CLMM amounts computed independently with the official Raydium SDK
// (@raydium-io/raydium-sdk-v2 0.2.74-alpha: TickUtil.getSqrtPriceAtTick +
// LiquidityMathUtil.getAmountsForLiquidity, roundUp=false, over the same captured accounts).
var rayWant = map[string]struct {
	inRange bool
	a, b    string
}{
	rayPosIn:    {true, "22.54164983", "312.653323"},
	rayPosBelow: {false, "8.636313615", "0"},
	rayPosAbove: {false, "0", "673.482131"},
}

func rayInput() Input {
	return Input{
		Owner: "94gKsZWhwNWRzkJ9a7JyL13sreDMepwLkmpEKndX3SPH",
		Tokens: []TokenBalance{
			nft(rayMintIn), nft(rayMintBelow), nft(rayMintAbove),
			{Mint: raySOLUSDCLP, Amount: big.NewInt(12_345_678_901), Decimals: 9}, // 12.345678901 LP
			{Mint: rayNEARUSDCLP, Amount: big.NewInt(3_000_000_000), Decimals: 9},
			{Mint: rayNEARUSDCLP, Amount: big.NewInt(500_000_000), Decimals: 9}, // two accounts: 3.5 LP in total
			{Mint: usdcMint, Amount: big.NewInt(1_000_000), Decimals: 6},        // not an LP mint (null)
		},
	}
}

func rayAdapter(base string) *raydiumAdapter {
	r := newRaydium(fastClient())
	r.apiBase = base
	return r
}

func TestRaydiumCLMMAndLPFromMainnet(t *testing.T) {
	accts, _ := loadAccounts(t, "raydium_clmm_accounts.json")
	var urls []string
	srv := serveJSON(t, http.StatusOK, readFixture(t, "raydium_lps_api.json"), &urls)
	rpc := newFakeRPC(accts)
	ps, err := rayAdapter(srv.URL).Positions(context.Background(), rpc, rayInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 5 {
		t.Fatalf("got %d positions: %+v", len(ps), ps)
	}
	for id, w := range rayWant {
		p := findPosition(t, ps, id)
		if p.Protocol != "raydium" || p.Kind != KindLP || p.InRange == nil || *p.InRange != w.inRange {
			t.Errorf("%s: %+v", id, p)
		}
		if len(p.Legs) != 2 || p.Legs[0].Mint != wsolMint || p.Legs[0].AmountStr != w.a || p.Legs[0].Decimals != 9 ||
			p.Legs[1].Mint != usdcMint || p.Legs[1].AmountStr != w.b || p.Legs[1].Decimals != 6 {
			t.Errorf("%s: legs %+v, want SOL %s / USDC %s", id, p.Legs, w.a, w.b)
		}
	}
	if l := findPosition(t, ps, rayPosIn).Label; l != "SOL/USDC · in range" {
		t.Errorf("label %q", l)
	}
	if c := findPosition(t, ps, rayPosAbove).ClaimedMints; !slices.Equal(c, []string{rayMintAbove}) {
		t.Errorf("claimed %v", c)
	}
	// Positions and pool: two RPC batches (Raydium pools store mint decimals).
	if len(rpc.calls) != 2 {
		t.Errorf("RPC calls %v", rpc.calls)
	}

	// One API call for all fungible candidates; position NFTs are never sent.
	if len(urls) != 1 {
		t.Fatalf("API calls %v", urls)
	}
	u, _ := url.Parse(urls[0])
	sent := strings.Split(u.Query().Get("lps"), ",")
	if u.Path != "/pools/info/lps" || !slices.Equal(sent, []string{raySOLUSDCLP, rayNEARUSDCLP, usdcMint}) {
		t.Errorf("API request %s (lps %v)", urls[0], sent)
	}

	// AMM v4 SOL-USDC: expected legs from Python decimal arithmetic over the same API fixture
	// (share = balance / lpAmount, leg = share × reserve, truncated to the mint's decimals).
	amm := findPosition(t, ps, "58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2")
	if amm.Label != "SOL/USDC · AMM LP" || amm.InRange != nil || !slices.Equal(amm.ClaimedMints, []string{raySOLUSDCLP}) {
		t.Errorf("amm position %+v", amm)
	}
	if amm.Legs[0].Mint != wsolMint || amm.Legs[0].AmountStr != "14.646885808" || amm.Legs[1].AmountStr != "1511.862242" {
		t.Errorf("amm legs %+v", amm.Legs)
	}
	cpmm := findPosition(t, ps, "gyYigqG8vdEMkDNTTVL6at2msbHdTDWyb6bccyR238U")
	if cpmm.Label != "NEAR/USDC · CPMM LP" || !slices.Equal(cpmm.ClaimedMints, []string{rayNEARUSDCLP}) {
		t.Errorf("cpmm position %+v", cpmm)
	}
	if cpmm.Legs[0].Mint != nearMint || cpmm.Legs[0].AmountStr != "58.402580828" || cpmm.Legs[1].AmountStr != "291.376905" {
		t.Errorf("cpmm legs %+v", cpmm.Legs)
	}

	// Independent cross-check with the API's own prices: legA·price + legB ≈ balance × lpPrice.
	for _, c := range []struct {
		p                 Position
		price, lpPrice, n float64
	}{
		{amm, 103.22072977886596, 245.11294075371998, 12.345678901},
		{cpmm, 4.989110087209608, 166.5008364663182, 3.5},
	} {
		got := c.p.Legs[0].Amount*c.price + c.p.Legs[1].Amount
		want := c.n * c.lpPrice
		if math.Abs(got-want)/want > 0.005 {
			t.Errorf("%s: value %.2f vs lpPrice value %.2f", c.p.ID, got, want)
		}
	}
}

func TestRaydiumPositionPDA(t *testing.T) {
	mb, _ := pubkeyBytes(rayMintIn)
	got, err := findProgramAddress([][]byte{[]byte("position"), mb}, RaydiumCLMMProgram)
	if err != nil || got != rayPosIn {
		t.Fatalf("PDA = %s, %v; want %s", got, err, rayPosIn)
	}
}

func TestRaydiumAPIFailureKeepsCLMM(t *testing.T) {
	accts, _ := loadAccounts(t, "raydium_clmm_accounts.json")
	for name, srv := range map[string]struct {
		status int
		body   string
	}{
		"http 500":      {http.StatusInternalServerError, `oops`},
		"success false": {http.StatusOK, `{"success":false,"msg":"query lps value error"}`},
		"bad json":      {http.StatusOK, `{"success":true,"data":[{"lpMint":`},
		"bad supply":    {http.StatusOK, `{"success":true,"data":[{"id":"p","mintA":{"address":"a","decimals":9},"mintB":{"address":"b","decimals":6},"mintAmountA":1,"mintAmountB":2,"lpMint":{"address":"` + raySOLUSDCLP + `","decimals":9},"lpAmount":0}]}`},
		"neg reserves":  {http.StatusOK, `{"success":true,"data":[{"id":"p","mintA":{"address":"a","decimals":9},"mintB":{"address":"b","decimals":6},"mintAmountA":-1,"mintAmountB":2,"lpMint":{"address":"` + raySOLUSDCLP + `","decimals":9},"lpAmount":5}]}`},
		"over supply":   {http.StatusOK, `{"success":true,"data":[{"id":"p","mintA":{"address":"a","decimals":9},"mintB":{"address":"b","decimals":6},"mintAmountA":1,"mintAmountB":2,"lpMint":{"address":"` + raySOLUSDCLP + `","decimals":9},"lpAmount":1}]}`},
	} {
		s := serveJSON(t, srv.status, []byte(srv.body), nil)
		ps, err := rayAdapter(s.URL).Positions(context.Background(), newFakeRPC(accts), rayInput())
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if len(ps) != 3 {
			t.Errorf("%s: got %d positions, want the 3 CLMM positions", name, len(ps))
		}
	}
}

func TestRaydiumLPOnlyIgnoresUnknownAndMismatched(t *testing.T) {
	// An entry whose lpMint is not one we asked for is ignored even if positionally aligned.
	body := `{"success":true,"data":[null,{"id":"p","mintA":{"address":"a","decimals":9},"mintB":{"address":"b","decimals":6},"mintAmountA":1,"mintAmountB":2,"lpMint":{"address":"someOtherMint","decimals":9},"lpAmount":5}]}`
	srv := serveJSON(t, http.StatusOK, []byte(body), nil)
	in := Input{Tokens: []TokenBalance{
		{Mint: raySOLUSDCLP, Amount: big.NewInt(1e9), Decimals: 9},
		{Mint: rayNEARUSDCLP, Amount: big.NewInt(1e9), Decimals: 9},
	}}
	ps, err := rayAdapter(srv.URL).Positions(context.Background(), newFakeRPC(nil), in)
	if err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
}

func TestRaydiumNoTokensNoCalls(t *testing.T) {
	var urls []string
	srv := serveJSON(t, http.StatusOK, []byte(`{}`), &urls)
	rpc := newFakeRPC(nil)
	ps, err := rayAdapter(srv.URL).Positions(context.Background(), rpc, Input{})
	if err != nil || len(ps) != 0 || len(urls) != 0 || len(rpc.calls) != 0 {
		t.Fatalf("got %v, %v, urls %v, rpc %v", ps, err, urls, rpc.calls)
	}
}

func TestRaydiumMalformedAccounts(t *testing.T) {
	base, _ := loadAccounts(t, "raydium_clmm_accounts.json")
	srv := serveJSON(t, http.StatusOK, []byte(`{"success":true,"data":[null,null,null,null]}`), nil)
	for _, key := range []string{rayPosIn, raySOLUSDCPool} {
		for n := 0; n <= len(base[key]); n++ {
			accts := map[string][]byte{}
			for k, v := range base {
				accts[k] = v
			}
			accts[key] = base[key][:n]
			ps, err := rayAdapter(srv.URL).Positions(context.Background(), newFakeRPC(accts), rayInput())
			if key == raySOLUSDCPool && n < rayPoolMinLen && (err == nil || len(ps) != 0) {
				t.Fatalf("truncated pool (%d bytes): got %d positions, %v", n, len(ps), err)
			}
			if key == rayPosIn && n < rayPosMinLen && len(ps) != 2 {
				t.Fatalf("truncated position (%d bytes): got %d positions, %v", n, len(ps), err)
			}
		}
	}
}
