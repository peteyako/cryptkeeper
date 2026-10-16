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

func TestChange7dFixtures(t *testing.T) {
	var with, without int
	for _, a := range Snapshot(now).Assets {
		if a.Change7d != nil {
			with++
		} else {
			without++
		}
	}
	if with < 4 || without < 1 {
		t.Errorf("want some assets with a 7d change and some without, got %d/%d", with, without)
	}
}

func TestMajorsPortfolio(t *testing.T) {
	s, ok := SnapshotFor(PFMajors, now)
	if !ok || len(s.Assets) != 5 {
		t.Fatalf("majors: ok=%v assets=%d", ok, len(s.Assets))
	}
	var walletSum float64
	for _, w := range s.Wallets {
		walletSum += w.ValueUSD
		for _, r := range w.Rows {
			if !contains(portfolioAssets[PFMajors], r.AssetKey) {
				t.Errorf("%s holds %s, which Majors excludes", w.ID, r.AssetKey)
			}
		}
	}
	if !near(walletSum, s.TotalUSD) {
		t.Errorf("majors wallets sum to %v, total is %v", walletSum, s.TotalUSD)
	}
	if s.TotalUSD >= Snapshot(now).TotalUSD {
		t.Error("majors should be a subset of everything")
	}
}

func TestPortfolioPageBuilders(t *testing.T) {
	p := Portfolios(now)
	if len(p.Items) != 3 || len(p.Backups) == 0 || p.Active != "portfolios" {
		t.Fatalf("portfolios page: %d items, %d backups, active %q", len(p.Items), len(p.Backups), p.Active)
	}
	all := Snapshot(now).TotalUSD
	for _, it := range p.Items {
		snap, _ := SnapshotFor(it.ID, now)
		if !near(it.ValueUSD, snap.TotalUSD) || math.Abs(it.Share-snap.TotalUSD/all) > 1e-9 {
			t.Errorf("%s: value %v share %v do not match the snapshot", it.ID, it.ValueUSD, it.Share)
		}
		if it.Name == "" || it.WalletsText == "" || it.AssetsText == "" {
			t.Errorf("%s: texts missing: %+v", it.ID, it)
		}
	}
	if got := assetsText([]string{"BTC", "ETH", "SOL", "LINK", "DOT"}); got != "BTC, ETH, SOL +2" {
		t.Errorf("assetsText = %q", got)
	}
	if assetsText(nil) != "All assets" || assetsText([]string{"BTC", "ETH"}) != "BTC, ETH" {
		t.Error("assetsText short forms")
	}
	for i := 1; i < len(p.Backups); i++ {
		if !p.Backups[i].Time.Before(p.Backups[i-1].Time) {
			t.Error("backups should be newest first")
		}
	}

	nf := PortfolioForm("", now)
	if nf.EditID != "" || !nf.AllWallets || !nf.AllAssets || len(nf.WalletOptions) != 5 || len(nf.AssetOptions) != 8 {
		t.Errorf("new form: %+v", nf.EditID)
	}
	for i := 1; i < len(nf.AssetOptions); i++ {
		if nf.AssetOptions[i].ValueUSD > nf.AssetOptions[i-1].ValueUSD {
			t.Error("asset options should be by value descending")
		}
	}
	for _, id := range []string{PFLongTerm, PFTrading, PFMajors} {
		f := PortfolioForm(id, now)
		if f.EditID != id || f.Name == "" || (f.AllWallets && f.AllAssets) {
			t.Errorf("%s edit form: %+v", id, f.EditID)
		}
	}
	if PortfolioForm("nope", now).EditID != "" {
		t.Error("an unknown id gives the create form")
	}
	if bad := PortfolioFormInvalid(now); bad.Errors["name"] == "" || bad.Errors[""] == "" || bad.AllWallets {
		t.Error("invalid form should show errors")
	}
}
