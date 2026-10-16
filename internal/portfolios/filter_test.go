package portfolios

import (
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

var (
	testNow    = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	testPrices = map[string]core.Price{
		"BTC":  {USD: 60000, AsOf: testNow},
		"USDC": {USD: 1, AsOf: testNow},
		"SOL":  {USD: 100, AsOf: testNow},
	}
)

func row(wallet, asset string, amount float64) portfolio.HoldingRow {
	return portfolio.PriceRow(core.Holding{WalletID: wallet, AssetKey: asset, Symbol: asset, Amount: amount}, testPrices)
}

func view(id string, rows ...portfolio.HoldingRow) portfolio.WalletView {
	return portfolio.WalletView{ID: id, Label: id, Type: "manual", Kind: core.KindManual, Rows: rows, Status: portfolio.StatusSynced, AsOf: testNow}
}

// testSnapshot: BTC on cold+coinbase, USDC on coinbase+phantom, SOL on phantom.
//
//	cold     1.0 BTC                     = 60000
//	coinbase 0.5 BTC + 1000 USDC         = 31000
//	phantom  500 USDC + 10 SOL           =  1500
//	total                                = 92500
func testSnapshot() portfolio.Snapshot {
	views := []portfolio.WalletView{
		view("cold", row("cold", "BTC", 1)),
		view("coinbase", row("coinbase", "BTC", 0.5), row("coinbase", "USDC", 1000)),
		view("phantom", row("phantom", "USDC", 500), row("phantom", "SOL", 10)),
	}
	views[2].HiddenRows = []portfolio.HoldingRow{row("phantom", "spl:SPAM", 1), row("phantom", "USDC", 0.01)}
	return portfolio.FromWallets(views, testPrices, testNow)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func walletIDs(s portfolio.Snapshot) []string {
	out := []string{}
	for _, w := range s.Wallets {
		out = append(out, w.ID)
	}
	slices.Sort(out)
	return out
}

func assetKeys(s portfolio.Snapshot) []string {
	out := []string{}
	for _, a := range s.Assets {
		out = append(out, a.AssetKey)
	}
	slices.Sort(out)
	return out
}

func rowKeys(s portfolio.Snapshot) []string {
	out := []string{}
	for _, w := range s.Wallets {
		for _, r := range w.Rows {
			out = append(out, w.ID+"/"+r.AssetKey)
		}
	}
	slices.Sort(out)
	return out
}

func TestTestSnapshotIsAsDocumented(t *testing.T) {
	snap := testSnapshot()
	if !near(snap.TotalUSD, 92500) {
		t.Fatalf("total %v", snap.TotalUSD)
	}
}

func TestApplyAssetsOnly(t *testing.T) {
	p := Portfolio{ID: "btc", Name: "BTC", Wallets: AllSelector(), Assets: Selector{IDs: []string{"BTC"}}}
	got := Apply(p, testSnapshot())
	if !near(got.TotalUSD, 90000) {
		t.Fatalf("total %v", got.TotalUSD)
	}
	if want := []string{"coinbase", "cold"}; !reflect.DeepEqual(walletIDs(got), want) {
		t.Fatalf("wallets %v want %v (phantom has no BTC and is not explicitly listed)", walletIDs(got), want)
	}
	if want := []string{"BTC"}; !reflect.DeepEqual(assetKeys(got), want) {
		t.Fatalf("assets %v", assetKeys(got))
	}
	if a := got.Assets[0]; !near(a.Amount, 1.5) || !near(a.Share, 1) || !reflect.DeepEqual(slices.Sorted(slices.Values(a.HeldIn)), []string{"coinbase", "cold"}) {
		t.Fatalf("asset row %+v", a)
	}
	for _, w := range got.Wallets {
		want := map[string]float64{"cold": 60000, "coinbase": 30000}[w.ID]
		if !near(w.ValueUSD, want) {
			t.Fatalf("wallet %s value %v want %v (recomputed from filtered rows)", w.ID, w.ValueUSD, want)
		}
	}
}

func TestApplyWalletsOnly(t *testing.T) {
	p := Portfolio{ID: "cb", Name: "CB", Wallets: Selector{IDs: []string{"coinbase", "phantom"}}, Assets: AllSelector()}
	got := Apply(p, testSnapshot())
	if !near(got.TotalUSD, 32500) {
		t.Fatalf("total %v", got.TotalUSD)
	}
	if want := []string{"coinbase", "phantom"}; !reflect.DeepEqual(walletIDs(got), want) {
		t.Fatalf("wallets %v", walletIDs(got))
	}
	if want := []string{"BTC", "SOL", "USDC"}; !reflect.DeepEqual(assetKeys(got), want) {
		t.Fatalf("assets %v", assetKeys(got))
	}
}

func TestApplyIntersection(t *testing.T) {
	p := Portfolio{ID: "x", Name: "X", Wallets: Selector{IDs: []string{"coinbase", "phantom"}}, Assets: Selector{IDs: []string{"USDC", "SOL"}}}
	got := Apply(p, testSnapshot())
	if !near(got.TotalUSD, 2500) { // coinbase USDC 1000 + phantom 500 USDC + 1000 SOL
		t.Fatalf("total %v", got.TotalUSD)
	}
	want := []string{"coinbase/USDC", "phantom/SOL", "phantom/USDC"}
	if !reflect.DeepEqual(rowKeys(got), want) {
		t.Fatalf("rows %v want %v", rowKeys(got), want)
	}
	// BTC is held in coinbase but is outside the asset selector; USDC in no-listed wallets is excluded.
	if slices.Contains(assetKeys(got), "BTC") {
		t.Fatal("BTC leaked in")
	}
	var shares float64
	for _, a := range got.Assets {
		shares += a.Share
	}
	if !near(shares, 1) {
		t.Fatalf("shares sum to %v, want recomputed against the filtered total", shares)
	}
}

func TestApplyWatchOnlyIsEmpty(t *testing.T) {
	p := Portfolio{ID: "w", Name: "W", Wallets: Selector{}, Assets: AllSelector(), Watch: []string{"ETH", "AVAX"}}
	got := Apply(p, testSnapshot())
	if got.TotalUSD != 0 || len(got.Wallets) != 0 || len(got.Assets) != 0 {
		t.Fatalf("%+v", got)
	}
	if !got.UpdatedAt.Equal(testNow) || !reflect.DeepEqual(got.Prices, testPrices) {
		t.Fatalf("UpdatedAt/Prices must carry over: %v %v", got.UpdatedAt, got.Prices)
	}
	if want := []string{"ETH", "AVAX"}; !reflect.DeepEqual(WatchKeys(p), want) {
		t.Fatalf("WatchKeys %v", WatchKeys(p))
	}
}

func TestApplyNoAssetsIsEmptyButKeepsExplicitWallets(t *testing.T) {
	p := Portfolio{ID: "n", Name: "N", Wallets: Selector{IDs: []string{"cold"}}, Assets: Selector{}}
	got := Apply(p, testSnapshot())
	if got.TotalUSD != 0 || !reflect.DeepEqual(walletIDs(got), []string{"cold"}) || len(got.Wallets[0].Rows) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestApplyAllAllIsIdentity(t *testing.T) {
	snap := testSnapshot()
	// Even a wallet that failed and has no rows survives an unfiltered portfolio.
	snap.Wallets = append(snap.Wallets, portfolio.WalletView{ID: "broken", Label: "broken", Status: portfolio.StatusError, Err: "boom"})
	p := Portfolio{ID: "everything", Name: "Everything", Wallets: AllSelector(), Assets: AllSelector()}
	got := Apply(p, snap)
	if !near(got.TotalUSD, snap.TotalUSD) {
		t.Fatalf("total %v want %v", got.TotalUSD, snap.TotalUSD)
	}
	if !reflect.DeepEqual(walletIDs(got), walletIDs(snap)) {
		t.Fatalf("wallets %v want %v", walletIDs(got), walletIDs(snap))
	}
	if !reflect.DeepEqual(assetKeys(got), assetKeys(snap)) || !reflect.DeepEqual(rowKeys(got), rowKeys(snap)) {
		t.Fatal("rows or assets differ")
	}
	for _, w := range got.Wallets {
		if w.ID == "phantom" && len(w.HiddenRows) != 2 {
			t.Fatalf("hidden rows should pass an unfiltered portfolio: %v", w.HiddenRows)
		}
	}
}

func TestApplyKeepsExplicitlyListedEmptyAndErroringWallets(t *testing.T) {
	snap := testSnapshot()
	snap.Wallets = append(snap.Wallets,
		portfolio.WalletView{ID: "broken", Label: "broken", Status: portfolio.StatusError, Err: "boom"},
		view("empty"))
	p := Portfolio{ID: "k", Name: "K", Wallets: Selector{IDs: []string{"broken", "empty", "phantom", "coinbase", "ghost"}}, Assets: Selector{IDs: []string{"BTC"}}}
	got := Apply(p, snap)
	// phantom has no BTC, but is listed, so it stays (empty); broken and empty too; ghost is not in the snapshot.
	if want := []string{"broken", "coinbase", "empty", "phantom"}; !reflect.DeepEqual(walletIDs(got), want) {
		t.Fatalf("wallets %v want %v", walletIDs(got), want)
	}
	for _, w := range got.Wallets {
		switch w.ID {
		case "broken":
			if w.Status != portfolio.StatusError || w.Err != "boom" {
				t.Fatalf("status lost: %+v", w)
			}
		case "phantom", "empty":
			if len(w.Rows) != 0 || w.ValueUSD != 0 {
				t.Fatalf("%s should be empty: %+v", w.ID, w)
			}
		}
	}
	if !near(got.TotalUSD, 30000) {
		t.Fatalf("total %v", got.TotalUSD)
	}

	// With wallets: all, the same empty wallets are dropped once assets narrow the view.
	p2 := Portfolio{ID: "k2", Name: "K2", Wallets: AllSelector(), Assets: Selector{IDs: []string{"BTC"}}}
	if want := []string{"coinbase", "cold"}; !reflect.DeepEqual(walletIDs(Apply(p2, snap)), want) {
		t.Fatalf("got %v", walletIDs(Apply(p2, snap)))
	}
}

func TestApplyFiltersHiddenRows(t *testing.T) {
	snap := testSnapshot()
	p := Portfolio{ID: "u", Name: "U", Wallets: Selector{IDs: []string{"phantom"}}, Assets: Selector{IDs: []string{"USDC", "SOL"}}}
	got := Apply(p, snap)
	var hidden []portfolio.HoldingRow
	for _, w := range got.Wallets {
		hidden = append(hidden, w.HiddenRows...)
	}
	if len(hidden) != 1 || hidden[0].AssetKey != "USDC" {
		t.Fatalf("hidden rows should be filtered by asset too: %+v", hidden)
	}
	// Hidden rows never count toward the total.
	if !near(got.TotalUSD, 1500) {
		t.Fatalf("total %v", got.TotalUSD)
	}
}

func TestApplyDoesNotMutateInput(t *testing.T) {
	snap := testSnapshot()
	rowsBefore := rowKeys(snap)
	hiddenBefore := len(snap.Wallets[0].HiddenRows) + len(snap.Wallets[1].HiddenRows) + len(snap.Wallets[2].HiddenRows)
	totalBefore := snap.TotalUSD
	Apply(Portfolio{ID: "x", Name: "X", Wallets: Selector{IDs: []string{"phantom"}}, Assets: Selector{IDs: []string{"SOL"}}}, snap)
	hiddenAfter := len(snap.Wallets[0].HiddenRows) + len(snap.Wallets[1].HiddenRows) + len(snap.Wallets[2].HiddenRows)
	if !reflect.DeepEqual(rowKeys(snap), rowsBefore) || hiddenAfter != hiddenBefore || snap.TotalUSD != totalBefore {
		t.Fatal("Apply modified its input snapshot")
	}
}

func TestNetWorthShare(t *testing.T) {
	all := testSnapshot()
	p := Portfolio{ID: "btc", Name: "BTC", Wallets: AllSelector(), Assets: Selector{IDs: []string{"BTC"}}}
	filtered := Apply(p, all)
	if got, want := NetWorthShare(filtered, all), 90000.0/92500.0; !near(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := NetWorthShare(all, all); !near(got, 1) {
		t.Fatalf("self share %v", got)
	}
	if got := NetWorthShare(filtered, portfolio.Snapshot{}); got != 0 {
		t.Fatalf("zero total must give 0, got %v", got)
	}
}

func TestIncludeAgreesWithApply(t *testing.T) {
	snap := testSnapshot()
	for name, p := range map[string]Portfolio{
		"assets only":  {Wallets: AllSelector(), Assets: Selector{IDs: []string{"BTC"}}},
		"wallets only": {Wallets: Selector{IDs: []string{"coinbase", "phantom"}}, Assets: AllSelector()},
		"intersection": {Wallets: Selector{IDs: []string{"coinbase", "phantom"}}, Assets: Selector{IDs: []string{"USDC", "SOL"}}},
		"watch only":   {Wallets: Selector{}, Assets: AllSelector()},
		"no assets":    {Wallets: AllSelector(), Assets: Selector{}},
		"all":          {Wallets: AllSelector(), Assets: AllSelector()},
		"unknown":      {Wallets: Selector{IDs: []string{"ghost"}}, Assets: Selector{IDs: []string{"DOGE"}}},
	} {
		t.Run(name, func(t *testing.T) {
			p.ID, p.Name = "x", "X"
			inc := Include(p)
			filtered := Apply(p, snap)
			var fromInclude []string
			var includeTotal float64
			for _, w := range snap.Wallets {
				for _, r := range w.Rows {
					if inc(w.ID, r.AssetKey) {
						fromInclude = append(fromInclude, w.ID+"/"+r.AssetKey)
						includeTotal += r.ValueUSD
					}
				}
			}
			slices.Sort(fromInclude)
			if want := rowKeys(filtered); !slices.Equal(fromInclude, want) && !(len(fromInclude) == 0 && len(want) == 0) {
				t.Fatalf("Include rows %v, Apply rows %v", fromInclude, want)
			}
			if !near(includeTotal, filtered.TotalUSD) {
				t.Fatalf("Include total %v, Apply total %v", includeTotal, filtered.TotalUSD)
			}
		})
	}
}

func TestMissingWallets(t *testing.T) {
	known := []string{"cold", "coinbase"}
	p := Portfolio{Wallets: Selector{IDs: []string{"cold", "gone", "coinbase", "also-gone"}}}
	if got, want := MissingWallets(p, known), []string{"gone", "also-gone"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := MissingWallets(Portfolio{Wallets: AllSelector()}, known); len(got) != 0 {
		t.Fatalf("all can't be missing anything: %v", got)
	}
	if got := MissingWallets(Portfolio{Wallets: Selector{}}, known); len(got) != 0 {
		t.Fatalf("none can't be missing anything: %v", got)
	}
	if got := MissingWallets(p, nil); len(got) != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestWatchKeysIsACopy(t *testing.T) {
	p := Portfolio{Watch: []string{"ETH"}}
	k := WatchKeys(p)
	k[0] = "mutated"
	if p.Watch[0] != "ETH" {
		t.Fatal("WatchKeys leaked the backing array")
	}
	if len(WatchKeys(Portfolio{})) != 0 {
		t.Fatal("no watch keys")
	}
}
