package defi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real JUP escrows (captured 2026-10-16), keyed by owner.
var escrowCases = []struct {
	owner, escrow, vault string
	raw                  string // amount + partial_unstaking_amount (= vault balance)
	amountStr, label     string
}{
	{"43kwrxUjL7hiwg5t5na3iUp2PH4TAvXiJKxUjmqB2PBu", "CMSRvCzfCpB8YQxQaVCC3CZhXJThiobR19Us24qDohLY",
		"Fjwm759FKY3UfRUCQCmzL2rYXDuSJK6eaW8aNhQefrj1", "4999895496", "4999.895496", "Staked JUP"},
	{"94gKsZWhwNWRzkJ9a7JyL13sreDMepwLkmpEKndX3SPH", "Hhbgsc2rrsQWS7Eb74LXYWF2fN87WGgL5ggweSF9UWup",
		"DYmum1Fb6eYCApzij5B4KL5xTGS9nGQhZ5ZGk7KVgAwy", "314600532668", "314600.532668", "Locked JUP · unlocks Oct 23, 2026"},
	{"B1zSDzmejrXs4yFMGCkma7FPdKnh1AYn5NH1XJ6bJiXB", "2JLxx53s7znxgL4u4JgJLbanKERDo5XfMN6rATWXtBpB",
		"7Z1nWqz54FMWMVN2hgsmLwYLBkDsBrnXTby2VmUh9Qro", "12000000000", "12000", "Staked JUP · 1,998 unstaking"},
}

const perpsOwner = "H3ogGm2QeDiELWVVo5pyn9ddZhZyxpoGzgEFe6Wng4BQ"

// jupAdapter returns an adapter whose perps API answers with an empty position list (or the
// given server), and whose clock is the escrow capture time.
func jupAdapter(t *testing.T, perpsBase, priceBase string, now time.Time) *jupiterAdapter {
	t.Helper()
	j := newJupiter(fastClient())
	if perpsBase == "" {
		perpsBase = serveJSON(t, http.StatusOK, []byte(`{"dataList":[],"count":0}`), nil).URL
	}
	j.perpsBase, j.priceBase = perpsBase, priceBase
	j.now = func() time.Time { return now }
	return j
}

func TestLockedJUPFromMainnet(t *testing.T) {
	accts, capturedAt := loadAccounts(t, "jup_escrow_accounts.json")
	for _, c := range escrowCases {
		t.Run(c.owner[:6], func(t *testing.T) {
			ps, err := jupAdapter(t, "", "", capturedAt).Positions(context.Background(), newFakeRPC(accts), Input{Owner: c.owner})
			if err != nil {
				t.Fatal(err)
			}
			if len(ps) != 1 {
				t.Fatalf("positions %+v", ps)
			}
			p := ps[0]
			if p.Protocol != "jupiter" || p.Kind != KindLocked || p.ID != c.escrow || p.Label != c.label || p.InRange != nil || len(p.ClaimedMints) != 0 {
				t.Errorf("position %+v", p)
			}
			if len(p.Legs) != 1 || p.Legs[0].Mint != jupMint || p.Legs[0].Decimals != 6 || p.Legs[0].AmountStr != c.amountStr || p.Legs[0].USDValue != nil {
				t.Errorf("legs %+v, want %s JUP", p.Legs, c.amountStr)
			}
			// Independent check: the escrow's token vault holds exactly amount + partial unstaking.
			vault, ok := accts[c.vault]
			if !ok || len(vault) < 72 || pubkeyAt(vault, 0) != jupMint || pubkeyAt(vault, 32) != c.escrow {
				t.Fatalf("vault %s missing or not the escrow's JUP account", c.vault)
			}
			if got := new(big.Int).SetUint64(u64At(vault, 64)).String(); got != c.raw {
				t.Errorf("vault balance %s, want %s", got, c.raw)
			}
		})
	}
}

func TestEscrowPDAAndLocker(t *testing.T) {
	ob, _ := pubkeyBytes(escrowCases[1].owner)
	lb, _ := pubkeyBytes(JupLocker)
	got, err := findProgramAddress([][]byte{[]byte("Escrow"), lb, ob}, JupLockedVoterProgram)
	if err != nil || got != escrowCases[1].escrow {
		t.Fatalf("escrow PDA %s, %v", got, err)
	}
	base, _ := pubkeyBytes("bJ1TRoFo2P6UHVwqdiipp6Qhp2HaaHpLowZ5LHet8Gm")
	if locker, _ := findProgramAddress([][]byte{[]byte("Locker"), base}, JupLockedVoterProgram); locker != JupLocker {
		t.Errorf("locker PDA %s", locker)
	}
	// The captured locker account locks the JUP mint.
	accts, _ := loadAccounts(t, "jup_escrow_accounts.json")
	l := accts[JupLocker]
	if !hasDiscriminator(l, anchorDiscriminator("Locker")) || pubkeyAt(l, 41) != jupMint {
		t.Errorf("locker account does not lock JUP")
	}
}

func TestEscrowLabels(t *testing.T) {
	accts, capturedAt := loadAccounts(t, "jup_escrow_accounts.json")
	c := escrowCases[1]
	later := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ps, err := jupAdapter(t, "", "", later).Positions(context.Background(), newFakeRPC(accts), Input{Owner: c.owner})
	if err != nil || len(ps) != 1 || ps[0].Label != "Locked JUP · unlocked Oct 23, 2026" {
		t.Fatalf("after unlock: %+v, %v", ps, err)
	}
	j := jupAdapter(t, "", "", capturedAt)
	for _, tc := range []struct {
		e    jupEscrow
		want string
	}{
		{jupEscrow{endsAt: 0}, "Staked JUP"},
		{jupEscrow{endsAt: -5}, "Staked JUP"},
		{jupEscrow{endsAt: 1 << 62}, "Staked JUP"}, // implausible date
		{jupEscrow{isMaxLock: true, endsAt: 1792751537}, "Staked JUP"},
		{jupEscrow{endsAt: 1803945600, partial: 1_234_567_891}, "Locked JUP · unlocks Mar 2, 2027 · 1,234.56 unstaking"},
	} {
		if got := j.escrowLabel(tc.e); got != tc.want {
			t.Errorf("label(%+v) = %q, want %q", tc.e, got, tc.want)
		}
	}
}

func TestLockedJUPMissingAndMalformed(t *testing.T) {
	accts, now := loadAccounts(t, "jup_escrow_accounts.json")
	// No escrow for this owner → no locked position.
	ps, err := jupAdapter(t, "", "", now).Positions(context.Background(), newFakeRPC(accts), Input{Owner: perpsOwner})
	if err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
	// Invalid owner.
	if _, err := jupAdapter(t, "", "", now).Positions(context.Background(), newFakeRPC(accts), Input{Owner: "nope"}); err == nil {
		t.Error("invalid owner accepted")
	}
	c := escrowCases[0]
	orig := accts[c.escrow]
	for n := 0; n < escMinLen; n++ {
		accts[c.escrow] = orig[:n]
		ps, err := jupAdapter(t, "", "", now).Positions(context.Background(), newFakeRPC(accts), Input{Owner: c.owner})
		if err == nil || len(ps) != 0 {
			t.Fatalf("truncated escrow (%d bytes): %v, %v", n, ps, err)
		}
	}
	// An escrow of another locker at the PDA (impossible on chain) is rejected, not reported.
	bad := append([]byte(nil), orig...)
	copy(bad[escLocker:], make([]byte, 32))
	accts[c.escrow] = bad
	if ps, err := jupAdapter(t, "", "", now).Positions(context.Background(), newFakeRPC(accts), Input{Owner: c.owner}); err == nil || len(ps) != 0 {
		t.Errorf("foreign locker: %v, %v", ps, err)
	}
	// Emptied escrow → no position.
	empty := append([]byte(nil), orig...)
	copy(empty[escAmount:], make([]byte, 8))
	accts[c.escrow] = empty
	if ps, err := jupAdapter(t, "", "", now).Positions(context.Background(), newFakeRPC(accts), Input{Owner: c.owner}); err != nil || len(ps) != 0 {
		t.Errorf("empty escrow: %v, %v", ps, err)
	}
}

type perpsFixture struct {
	DataList []struct {
		PositionPubkey   string `json:"positionPubkey"`
		SizeUsd          string `json:"sizeUsd"`
		CollateralUsd    string `json:"collateralUsd"`
		ValueUsd         string `json:"valueUsd"`
		PnlAfterFeesUsd  string `json:"pnlAfterFeesUsd"`
		PnlBeforeFeesUsd string `json:"pnlBeforeFeesUsd"`
	} `json:"dataList"`
}

func loadPerpsFixture(t *testing.T) perpsFixture {
	var f perpsFixture
	if err := json.Unmarshal(readFixture(t, "perps_api_positions.json"), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPerpsFromAPI(t *testing.T) {
	var urls []string
	srv := serveJSON(t, http.StatusOK, readFixture(t, "perps_api_positions.json"), &urls)
	j := jupAdapter(t, srv.URL, "", time.Now())
	ps, err := j.Positions(context.Background(), newFakeRPC(nil), Input{Owner: perpsOwner})
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 {
		t.Fatalf("API calls %v", urls)
	}
	if u, _ := url.Parse(urls[0]); u.Path != "/positions" || u.Query().Get("walletAddress") != perpsOwner {
		t.Errorf("request %s", urls[0])
	}
	f := loadPerpsFixture(t)
	if len(ps) != len(f.DataList) || len(ps) != 4 {
		t.Fatalf("got %d positions", len(ps))
	}
	labels := map[string]string{
		"GvJ3DnBxGtxpqVhjUi8L8ekPwVLpNU4DoayN5cRATuK5": "Long BTC · 3.99x",
		"C8YNyEdf2pZCjnn524wZFCVTBVwS8j6vd3BkZJ7a3veB": "Long SOL · 3.99x",
		"6cJTw71ezsgkhAoZDa8skvt1FKS8oUMssj3mgcEVzDFD": "Long ETH · 3.99x",
		"47JhaEuHpaRPAGaDzJjK4EnPqtr8FKjjzVrfj6QbmRDT": "Short BTC · 4x",
	}
	for _, d := range f.DataList {
		p := findPosition(t, ps, d.PositionPubkey)
		if p.Protocol != "jupiter" || p.Kind != KindPerp || p.Label != labels[d.PositionPubkey] {
			t.Errorf("%s: %+v", d.PositionPubkey, p)
		}
		// valueUsd is collateral + PnL after fees (checked on the fixture itself).
		coll, value, pnl := bigStr(t, d.CollateralUsd), bigStr(t, d.ValueUsd), bigStr(t, d.PnlAfterFeesUsd)
		if new(big.Int).Add(coll, pnl).Cmp(value) != 0 {
			t.Errorf("%s: valueUsd != collateral + pnlAfterFees", d.PositionPubkey)
		}
		leg := p.Legs[0]
		if len(p.Legs) != 1 || leg.Mint != usdcMint || leg.Decimals != 6 || leg.AmountStr != formatUnits(value, 6) ||
			leg.USDValue == nil || *leg.USDValue != leg.Amount {
			t.Errorf("%s: leg %+v (want %s USD)", d.PositionPubkey, leg, formatUnits(value, 6))
		}
	}
	if got := findPosition(t, ps, "C8YNyEdf2pZCjnn524wZFCVTBVwS8j6vd3BkZJ7a3veB").Legs[0].AmountStr; got != "103123.928988" {
		t.Errorf("SOL long value %s", got)
	}
}

func TestPerpsFallbackToChain(t *testing.T) {
	accts, _ := loadAccounts(t, "perps_accounts.json")
	down := serveJSON(t, http.StatusServiceUnavailable, []byte(`down`), nil)
	var priceURLs []string
	prices := serveJSON(t, http.StatusOK, readFixture(t, "jupiter_price_perps.json"), &priceURLs)
	rpc := newFakeRPC(accts)
	ps, err := jupAdapter(t, down.URL, prices.URL, time.Now()).Positions(context.Background(), rpc, Input{Owner: perpsOwner})
	if err != nil {
		t.Fatal(err)
	}
	// Nine PDAs derived and fetched in one batch (plus the escrow lookup); closed accounts skipped.
	if len(rpc.calls) != 2 || len(rpc.calls[1]) != 9 {
		t.Fatalf("rpc calls %v", rpc.calls)
	}
	if len(priceURLs) != 1 || !strings.Contains(priceURLs[0], "ids=") {
		t.Errorf("price calls %v", priceURLs)
	}
	f := loadPerpsFixture(t)
	if len(ps) != 4 {
		t.Fatalf("got %d positions: %+v", len(ps), ps)
	}
	for _, d := range f.DataList {
		p := findPosition(t, ps, d.PositionPubkey)
		if p.Kind != KindPerp || !strings.HasSuffix(p.Label, "· est.") {
			t.Errorf("%s: %+v", d.PositionPubkey, p)
		}
		// Collateral + uPnL from chain + Price API agrees with the perps API's collateral + PnL
		// before fees (captured seconds apart) to within 0.5% of the position size.
		est := *p.Legs[0].USDValue
		ref := (float64(bigStr(t, d.CollateralUsd).Int64()) + float64(bigStr(t, d.PnlBeforeFeesUsd).Int64())) / 1e6
		size := float64(bigStr(t, d.SizeUsd).Int64()) / 1e6
		if math.Abs(est-ref) > 0.005*size {
			t.Errorf("%s: estimate %.2f vs API %.2f (size %.0f)", d.PositionPubkey, est, ref, size)
		}
	}
	if l := findPosition(t, ps, "47JhaEuHpaRPAGaDzJjK4EnPqtr8FKjjzVrfj6QbmRDT").Label; l != "Short BTC · 4.01x · est." {
		t.Errorf("short label %q", l)
	}
}

func TestPerpsBothSourcesFail(t *testing.T) {
	accts, _ := loadAccounts(t, "perps_accounts.json")
	down := serveJSON(t, http.StatusServiceUnavailable, []byte(`down`), nil)
	ps, err := jupAdapter(t, down.URL, down.URL, time.Now()).Positions(context.Background(), newFakeRPC(accts), Input{Owner: perpsOwner})
	if err == nil || !strings.Contains(err.Error(), "perps") || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
}

func TestPerpsBadAPIData(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, []byte(`{"dataList":[{"positionPubkey":"x","side":"long","valueUsd":"12.5"}]}`), nil)
	// The API data is rejected; with no on-chain positions the fallback finds nothing.
	ps, err := jupAdapter(t, srv.URL, srv.URL, time.Now()).Positions(context.Background(), newFakeRPC(nil), Input{Owner: perpsOwner})
	if err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
}

func TestPerpsMalformedAccounts(t *testing.T) {
	base, _ := loadAccounts(t, "perps_accounts.json")
	prices := serveJSON(t, http.StatusOK, readFixture(t, "jupiter_price_perps.json"), nil)
	down := serveJSON(t, http.StatusServiceUnavailable, []byte(`down`), nil)
	key := "GvJ3DnBxGtxpqVhjUi8L8ekPwVLpNU4DoayN5cRATuK5"
	for n := 0; n < perpMinLen; n++ {
		accts := map[string][]byte{}
		for k, v := range base {
			accts[k] = v
		}
		accts[key] = base[key][:n]
		if _, err := jupAdapter(t, down.URL, prices.URL, time.Now()).Positions(context.Background(), newFakeRPC(accts), Input{Owner: perpsOwner}); err == nil {
			t.Fatalf("truncated position (%d bytes) accepted", n)
		}
	}
	// Zero entry price must not divide by zero.
	accts := map[string][]byte{}
	for k, v := range base {
		accts[k] = v
	}
	zero := append([]byte(nil), base[key]...)
	copy(zero[perpPrice:], make([]byte, 8))
	accts[key] = zero
	if _, err := jupAdapter(t, down.URL, prices.URL, time.Now()).Positions(context.Background(), newFakeRPC(accts), Input{Owner: perpsOwner}); err == nil {
		t.Error("zero entry price accepted")
	}
}

func TestJupiterPartialFailure(t *testing.T) {
	// Escrow RPC fails, perps API works: perps are returned together with the error.
	srv := serveJSON(t, http.StatusOK, readFixture(t, "perps_api_positions.json"), nil)
	rpc := newFakeRPC(nil)
	rpc.gmaErr = errors.New("rpc down")
	ps, err := jupAdapter(t, srv.URL, "", time.Now()).Positions(context.Background(), rpc, Input{Owner: perpsOwner})
	if err == nil || !strings.Contains(err.Error(), "rpc down") || len(ps) != 4 {
		t.Fatalf("got %d positions, %v", len(ps), err)
	}
}

func TestPerpsPDAs(t *testing.T) {
	ob, _ := pubkeyBytes(perpsOwner)
	keys, err := perpsPDAs(ob)
	if err != nil || len(keys) != 9 {
		t.Fatalf("%v, %v", keys, err)
	}
	accts, _ := loadAccounts(t, "perps_accounts.json")
	found := 0
	for _, k := range keys {
		if _, ok := accts[k]; ok {
			found++
		}
	}
	// The captured fixture holds the same nine keys; six exist on chain (4 open, 2 closed).
	if found != 6 {
		t.Errorf("found %d of the derived PDAs in the fixture", found)
	}
}

func TestPerpsAPIContextCancel(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := jupAdapter(t, srv.URL, srv.URL, time.Now()).Positions(ctx, newFakeRPC(nil), Input{Owner: perpsOwner})
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
}
