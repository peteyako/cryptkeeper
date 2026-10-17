package demo

import (
	"math"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
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

// ---- activity, reports, alerts -------------------------------------------------------------

func TestActivityFixtures(t *testing.T) {
	rows := ActivityRows(now)
	if len(rows) < 60 {
		t.Fatalf("want a long feed, got %d rows", len(rows))
	}
	ids := map[string]bool{}
	byID := map[string]core.Tx{}
	kinds := map[core.TxKind]bool{}
	statuses := map[core.TxStatus]bool{}
	for i, r := range rows {
		tx := r.Tx
		if ids[tx.ID] {
			t.Errorf("duplicate id %s", tx.ID)
		}
		ids[tx.ID] = true
		byID[tx.ID] = tx
		kinds[tx.Kind], statuses[tx.Status] = true, true
		if tx.ID != core.TxID(tx.SourceID, tx.ExternalID) {
			t.Errorf("%s: id is not source:external", tx.ID)
		}
		if i > 0 && tx.Time.After(rows[i-1].Tx.Time) {
			t.Errorf("row %d is newer than the one before it", i)
		}
		if tx.Time.After(now) {
			t.Errorf("%s is in the future", tx.ID)
		}
		if r.SourceLabel == "" {
			t.Errorf("%s: no source label", tx.ID)
		}
		if tx.Fee != nil && tx.Fee.Amount >= 0 {
			t.Errorf("%s: a fee is a negative leg", tx.ID)
		}
		for _, l := range tx.Legs {
			if l.USD != nil && (*l.USD < 0) != (l.Amount < 0) {
				t.Errorf("%s: %s value %v is not signed like its amount %v", tx.ID, l.Symbol, *l.USD, l.Amount)
			}
		}
		if tx.ExplorerURL != "" && !strings.HasPrefix(tx.ExplorerURL, "https://") {
			t.Errorf("%s: explorer link %s", tx.ID, tx.ExplorerURL)
		}
	}
	for _, k := range []core.TxKind{core.TxReceive, core.TxSend, core.TxTransfer, core.TxTrade, core.TxSwap, core.TxReward, core.TxStake, core.TxFee, core.TxOther} {
		if !kinds[k] {
			t.Errorf("no %s entry in the fixtures", k)
		}
	}
	for _, s := range []core.TxStatus{core.TxPending, core.TxConfirmed, core.TxFailed} {
		if !statuses[s] {
			t.Errorf("no %s entry in the fixtures", s)
		}
	}
	// Matched transfers point at each other, across two sources, and share a hash.
	matched := 0
	for _, r := range rows {
		tx := r.Tx
		if tx.PeerID == "" {
			continue
		}
		matched++
		peer, ok := byID[tx.PeerID]
		if !ok {
			t.Errorf("%s: peer %s does not exist", tx.ID, tx.PeerID)
			continue
		}
		if peer.PeerID != tx.ID || peer.SourceID != tx.PeerSource || tx.SourceID != peer.PeerSource || peer.TxHash != tx.TxHash {
			t.Errorf("%s and %s are not a matched pair", tx.ID, peer.ID)
		}
		if tx.Legs[0].Amount != -peer.Legs[0].Amount {
			t.Errorf("%s: the two sides of a transfer move opposite amounts", tx.ID)
		}
		if r.PeerLabel == "" {
			t.Errorf("%s: no peer label", tx.ID)
		}
	}
	if matched != 4 {
		t.Errorf("want two matched transfers (4 rows), got %d", matched)
	}
}

func TestActivityPaging(t *testing.T) {
	all := ActivityRows(now)
	var got []string
	page := Activity("", "", "", "", now)
	for n := 0; ; n++ {
		if n > 100 {
			t.Fatal("paging does not end")
		}
		if len(page.Rows) == 0 || len(page.Rows) > ActivityPageSize {
			t.Fatalf("page %d has %d rows", n, len(page.Rows))
		}
		for _, r := range page.Rows {
			got = append(got, r.Tx.ID)
		}
		if page.More == "" {
			break
		}
		if len(page.Rows) != ActivityPageSize {
			t.Errorf("a page with more after it must be full, got %d rows", len(page.Rows))
		}
		page = Activity("", "", "", page.More, now)
	}
	if len(got) != len(all) {
		t.Fatalf("paging returned %d rows, want %d", len(got), len(all))
	}
	for i, r := range all {
		if got[i] != r.Tx.ID {
			t.Fatalf("row %d is %s, want %s", i, got[i], r.Tx.ID)
		}
	}
	if p := Activity("", "", "", "not-a-cursor", now); len(p.Rows) != ActivityPageSize || p.More == "" {
		t.Error("a bad cursor starts from the top")
	}

	// Filters apply before paging.
	cb := Activity(IDCoinbase, "", "", "", now)
	for _, r := range cb.Rows {
		if r.Tx.SourceID != IDCoinbase {
			t.Errorf("source filter let %s through", r.Tx.SourceID)
		}
	}
	for _, r := range Activity("", "ETH", "", "", now).Rows {
		found := false
		for _, l := range r.Tx.Legs {
			found = found || l.AssetKey == "ETH"
		}
		if !found {
			t.Errorf("asset filter let %s through", r.Tx.ID)
		}
	}
	for _, r := range Activity("", "", "reward", "", now).Rows {
		if r.Tx.Kind != core.TxReward {
			t.Errorf("kind filter let %s through", r.Tx.Kind)
		}
	}
	if p := Activity(IDCold, "ETH", "", "", now); len(p.Rows) != 0 || p.More != "" {
		t.Error("Cold storage has no ETH activity")
	}
	p := Activity(IDKraken, "BTC", "trade", "", now)
	if p.SourceID != IDKraken || p.AssetKey != "BTC" || p.Kind != "trade" || len(p.Rows) == 0 {
		t.Errorf("filters are echoed back: %+v", p)
	}
	for _, o := range p.Sources {
		if o.ID == IDMom {
			t.Error("a manual source has no activity to filter by")
		}
	}
	if len(p.Assets) < 5 || len(p.Sync) != 4 {
		t.Errorf("filter options: %d assets, %d sync rows", len(p.Assets), len(p.Sync))
	}
	e := ActivityEmpty(now)
	if len(e.Rows) != 0 || e.More != "" {
		t.Error("empty activity has no rows")
	}
	for _, s := range e.Sync {
		if s.Count != 0 || !s.LastSync.IsZero() || s.BackfillDone {
			t.Errorf("an empty feed has nothing synced: %+v", s)
		}
	}
}

func TestWalletActivity(t *testing.T) {
	for _, tc := range []struct {
		id   string
		more bool
		min  int
	}{{IDCold, false, 5}, {IDCoinbase, true, 25}, {IDKraken, false, 3}, {IDPhantom, false, 5}, {IDMom, false, 0}} {
		p, _ := Wallet(tc.id, "activity", now)
		if p.ActivityMore != tc.more {
			t.Errorf("%s: ActivityMore = %v", tc.id, p.ActivityMore)
		}
		if len(p.Activity) < tc.min || len(p.Activity) > 25 {
			t.Errorf("%s: %d rows", tc.id, len(p.Activity))
		}
		for _, r := range p.Activity {
			if r.Tx.SourceID != tc.id {
				t.Errorf("%s shows an entry of %s", tc.id, r.Tx.SourceID)
			}
		}
	}
}

func TestReportsFixtures(t *testing.T) {
	p := Reports(now)
	snap := Snapshot(now)
	if len(p.Rows) != len(snap.Assets) {
		t.Fatalf("%d P/L rows for %d assets", len(p.Rows), len(snap.Assets))
	}
	var basis, unreal, known float64
	var missing, noBasis int
	for i, r := range p.Rows {
		a := snap.Assets[i]
		if r.AssetKey != a.AssetKey || !near(r.Amount, a.Amount) || !near(r.Value, a.ValueUSD) {
			t.Errorf("%s: row does not match the snapshot", r.Symbol)
		}
		if r.MissingUnits > 0 {
			missing++
			if r.MissingUnits > r.UnknownUnits {
				t.Errorf("%s: missing-basis units are part of the unknown ones", r.Symbol)
			}
		}
		if !r.HasBasis {
			noBasis++
			if r.Basis != 0 || r.Unrealized != 0 || r.UnknownUnits <= 0 {
				t.Errorf("%s: no basis, nothing computed: %+v", r.Symbol, r)
			}
			continue
		}
		k := r.Amount - r.UnknownUnits
		kv := r.Value * k / r.Amount
		if !near(r.Unrealized, kv-r.Basis) {
			t.Errorf("%s: unrealized %v != known value %v - basis %v", r.Symbol, r.Unrealized, kv, r.Basis)
		}
		if math.Abs(r.AvgCost*k-r.Basis) > 0.01 {
			t.Errorf("%s: avg cost x known units != basis", r.Symbol)
		}
		if math.Abs(r.UnrealizedPct-r.Unrealized/r.Basis*100) > 1e-9 {
			t.Errorf("%s: pct", r.Symbol)
		}
		basis += r.Basis
		unreal += r.Unrealized
		known += kv
	}
	if missing != 1 || noBasis != 2 {
		t.Errorf("want 1 with missing-basis units and 2 without a cost, got %d and %d", missing, noBasis)
	}
	if !near(p.TotalBasis, basis) || !near(p.TotalUnrealized, unreal) || !near(p.TotalValue, snap.TotalUSD) {
		t.Errorf("totals %v %v %v vs %v %v %v", p.TotalBasis, p.TotalUnrealized, p.TotalValue, basis, unreal, snap.TotalUSD)
	}
	if want := known / snap.TotalUSD; math.Abs(p.Coverage-want) > 1e-9 || p.Coverage <= 0 || p.Coverage >= 1 {
		t.Errorf("coverage %v, want %v (strictly between 0 and 1)", p.Coverage, want)
	}
	if len(p.Realized) != 3 || p.Realized[0].Year != now.Year() || p.Realized[0].Year < p.Realized[1].Year {
		t.Errorf("realized newest year first: %+v", p.Realized)
	}
	cells := PnLCells(now)
	if len(cells) != len(snap.Assets) {
		t.Errorf("%d cells", len(cells))
	}
	for _, r := range p.Rows {
		if c := cells[r.AssetKey]; c.OK != r.HasBasis || c.Pct != r.UnrealizedPct {
			t.Errorf("%s: cell %+v vs row", r.Symbol, c)
		}
	}
	if ov := Overview(now); len(ov.PnL) != len(snap.Assets) {
		t.Error("the overview carries the P/L cells")
	}

	if e := ReportsEmpty(now); len(e.Rows) != 0 || e.Coverage != 0 {
		t.Error("empty reports")
	}
}

func TestAlertFixtures(t *testing.T) {
	p := Alerts(now)
	types := map[string]bool{}
	var paused int
	for _, r := range p.Rules {
		types[r.Type] = true
		if !r.Enabled {
			paused++
		}
		if r.Name == "" || r.Summary == "" || r.ID == "" {
			t.Errorf("incomplete rule %+v", r)
		}
		if _, ok := alertValues[r.ID]; !ok {
			t.Errorf("rule %s has no form values", r.ID)
		}
	}
	for _, typ := range []string{"balance_drop", "price", "portfolio_move", "source_down", "large_activity"} {
		if !types[typ] {
			t.Errorf("no %s rule", typ)
		}
	}
	if paused != 1 {
		t.Errorf("want one paused rule, got %d", paused)
	}
	var withErr int
	for i, e := range p.Recent {
		if e.Err != "" {
			withErr++
		}
		if i > 0 && e.Time.After(p.Recent[i-1].Time) {
			t.Error("recent alerts are newest first")
		}
	}
	if withErr != 1 || p.NtfyHost == "" || !p.Redact {
		t.Errorf("one failed delivery, ntfy configured, amounts hidden: %d %q %v", withErr, p.NtfyHost, p.Redact)
	}
	if e := AlertsEmpty(now); len(e.Rules) != 0 || e.NtfyHost != "" {
		t.Error("empty alerts: no rules, no ntfy")
	}
	if l := AlertsLoadError(now); l.LoadError == "" {
		t.Error("load error")
	}

	n := AlertForm("", now)
	if n.EditID != "" || n.Values["type"] != "balance_drop" || n.Values["enabled"] == "" || n.Values["cooldown"] != "6h" {
		t.Errorf("new form defaults: %+v", n.Values)
	}
	if len(n.WalletOptions) != 5 || len(n.AssetOptions) == 0 || n.PortfolioOptions[0].ID != "all" {
		t.Errorf("form options: %d wallets, %d assets, %v", len(n.WalletOptions), len(n.AssetOptions), n.PortfolioOptions)
	}
	for id := range alertValues {
		f := AlertForm(id, now)
		if f.EditID != id || f.Values["type"] == "" {
			t.Errorf("edit %s: %+v", id, f)
		}
	}
	if AlertForm("nope", now).EditID != "" {
		t.Error("an unknown id gives the create form")
	}
	// The fixture map is not shared between forms.
	a := AlertForm(AlertBTCAbove, now)
	a.Values["asset"] = "changed"
	if AlertForm(AlertBTCAbove, now).Values["asset"] != "BTC" {
		t.Error("form values must be copied")
	}
	if inv := AlertFormInvalid(now); inv.Errors["asset"] == "" || inv.Errors[""] == "" {
		t.Error("invalid form has errors")
	}
}

func TestSettingsCarriesSync(t *testing.T) {
	s := Settings(now)
	if len(s.Sync) != 4 || s.Now.IsZero() {
		t.Errorf("settings sync: %d rows, now %v", len(s.Sync), s.Now)
	}
	if got := SyncRows(now); got[2].LastError == "" || got[0].BackfillDone != true || got[2].BackfillDone {
		t.Errorf("sync fixtures: %+v", got)
	}
}
