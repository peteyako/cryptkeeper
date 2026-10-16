package demo

import (
	"math"
	"testing"
	"time"

	"crypto-portfolio/internal/portfolio"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func TestSnapshotIsConsistent(t *testing.T) {
	s := Snapshot(now)
	if len(s.Wallets) != 5 {
		t.Fatalf("want 5 wallets, got %d", len(s.Wallets))
	}

	var walletSum float64
	for _, w := range s.Wallets {
		var rows float64
		for _, r := range w.Rows {
			if r.Priced && !near(r.ValueUSD, math.Round(r.Amount*r.Price.USD*100)/100) {
				t.Errorf("%s %s: value %v != amount x price", w.ID, r.Symbol, r.ValueUSD)
			}
			rows += r.ValueUSD
		}
		if !near(rows, w.ValueUSD) {
			t.Errorf("%s: rows sum to %v, wallet says %v", w.ID, rows, w.ValueUSD)
		}
		walletSum += w.ValueUSD
	}
	if !near(walletSum, s.TotalUSD) {
		t.Errorf("wallets sum to %v, total is %v", walletSum, s.TotalUSD)
	}

	var assetSum, shares float64
	for i, a := range s.Assets {
		assetSum += a.ValueUSD
		shares += a.Share
		if i > 0 && a.ValueUSD > s.Assets[i-1].ValueUSD {
			t.Errorf("assets not sorted by value at %d", i)
		}
		if len(a.HeldIn) == 0 {
			t.Errorf("%s is held nowhere", a.Symbol)
		}
	}
	if !near(assetSum, s.TotalUSD) {
		t.Errorf("assets sum to %v, total is %v", assetSum, s.TotalUSD)
	}
	if math.Abs(shares-1) > 1e-9 {
		t.Errorf("shares sum to %v, want 1", shares)
	}
	if !s.HasStale {
		t.Error("Kraken is stale, so the snapshot has stale data")
	}
}

func TestFixtureShape(t *testing.T) {
	ws := Wallets(now)
	byID := map[string]portfolio.WalletView{}
	for _, w := range ws {
		byID[w.ID] = w
	}
	if w := byID[IDKraken]; w.Status != portfolio.StatusStale || now.Sub(w.AsOf) != 3*time.Hour {
		t.Errorf("Kraken should be stale for 3h: %+v", w.Status)
	}
	ph := byID[IDPhantom]
	if len(ph.HiddenRows) != 3 || ph.NFTCount != 12 {
		t.Errorf("Phantom: %d hidden tokens, %d NFTs", len(ph.HiddenRows), ph.NFTCount)
	}
	syms := map[string]bool{}
	var staked, defi bool
	for _, r := range ph.Rows {
		syms[r.Symbol] = true
		staked = staked || r.Staked
		defi = defi || r.Position != ""
	}
	for _, s := range []string{"SOL", "USDC", "JUP", "BONK"} {
		if !syms[s] {
			t.Errorf("Phantom lacks %s", s)
		}
	}
	if !staked || !defi {
		t.Errorf("Phantom should have a staked row and a DeFi position (staked=%v defi=%v)", staked, defi)
	}
	if byID[IDCold].Type != "bitcoin" || byID[IDMom].Type != "manual" {
		t.Error("cold is bitcoin and mom-safe is manual")
	}
}

func TestChartAndChange(t *testing.T) {
	total := Snapshot(now).TotalUSD
	for _, rng := range []string{"1D", "1W", "1M", "1Y", "All", "bogus"} {
		pts := Chart(rng, now)
		if len(pts) < 2 {
			t.Fatalf("%s: %d points", rng, len(pts))
		}
		if pts[len(pts)-1].V != total {
			t.Errorf("%s: chart ends at %v, total is %v", rng, pts[len(pts)-1].V, total)
		}
		for i := 1; i < len(pts); i++ {
			if !pts[i].T.After(pts[i-1].T) {
				t.Errorf("%s: timestamps not increasing at %d", rng, i)
			}
		}
		c := Change(rng, now)
		if !c.Valid || c.Label == "" || !near(c.Abs, pts[len(pts)-1].V-pts[0].V) {
			t.Errorf("%s: change %+v does not match the chart", rng, c)
		}
	}
	if c := Change("1W", now); c.Label != "past week" || c.Pct <= 0 {
		t.Errorf("1W change: %+v", c)
	}
}

func TestPortfolioFilter(t *testing.T) {
	all := Snapshot(now)
	var sum float64
	for _, pf := range []string{PFLongTerm, PFTrading} {
		s, ok := SnapshotFor(pf, now)
		if !ok || s.TotalUSD <= 0 || s.TotalUSD >= all.TotalUSD {
			t.Fatalf("%s: ok=%v total=%v", pf, ok, s.TotalUSD)
		}
		var shares float64
		for _, a := range s.Assets {
			shares += a.Share
		}
		if math.Abs(shares-1) > 1e-9 {
			t.Errorf("%s: shares sum to %v", pf, shares)
		}
		sum += s.TotalUSD
	}
	// Long-term and Trading partition the five sources.
	if !near(sum, all.TotalUSD) {
		t.Errorf("portfolios sum to %v, all is %v", sum, all.TotalUSD)
	}
	if _, ok := SnapshotFor("nope", now); ok {
		t.Error("unknown portfolio should not resolve")
	}

	p, ok := OverviewFor(PFTrading, "1M", now)
	if !ok || p.NetWorthShare <= 0 || p.NetWorthShare >= 1 {
		t.Fatalf("share = %v ok=%v", p.NetWorthShare, ok)
	}
	if last := p.Chart[len(p.Chart)-1].V; !near(last, p.Snap.TotalUSD) {
		t.Errorf("filtered chart ends at %v, total is %v", last, p.Snap.TotalUSD)
	}
}

func TestPageBuilders(t *testing.T) {
	if p := Overview(now); p.Snap.TotalUSD == 0 || len(p.Watch) == 0 || p.PortfolioName != "All holdings" {
		t.Errorf("overview: %+v", p.PortfolioName)
	}
	if p := OverviewEmpty(now); len(p.Snap.Wallets) != 0 {
		t.Error("empty overview has wallets")
	}
	if _, ok := Wallet("nope", "holdings", now); ok {
		t.Error("unknown wallet should not resolve")
	}
	cold, _ := Wallet(IDCold, "addresses", now)
	if !cold.IsBitcoin || cold.Tab != "addresses" || cold.Receive == "" || cold.ReceiveQR == nil || len(cold.Addresses) == 0 || len(cold.UTXOs) == 0 {
		t.Errorf("cold wallet page incomplete: %+v", cold.Tab)
	}
	ph, _ := Wallet(IDPhantom, "utxos", now)
	if ph.Tab != "holdings" || ph.IsBitcoin || ph.Receive != "" || ph.CostBasis != nil {
		t.Errorf("phantom: tab=%q bitcoin=%v", ph.Tab, ph.IsBitcoin)
	}
	for _, typ := range []string{"coinbase", "kraken", "bitcoin", "solana", "manual"} {
		add, edit, bad := SourceForm(typ), SourceFormEdit(typ), SourceFormInvalid(typ)
		if add.Type != typ || add.EditID != "" || edit.EditID == "" || len(bad.Errors) < 2 {
			t.Errorf("%s: forms look wrong", typ)
		}
	}
	if len(Settings(now).Wallets) != 5 {
		t.Error("settings should list every source")
	}
	if !TestOK(now).OK || TestFailed().OK {
		t.Error("test results")
	}
	if QR("bitcoin:"+ReceiveAddress) == nil {
		t.Error("QR")
	}
}
