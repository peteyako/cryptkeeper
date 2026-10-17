package server

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/web/templates"
)

func reviewRecv(id, src, asset string, amt float64, at time.Time) core.Tx {
	return core.Tx{ID: src + ":" + id, SourceID: src, ExternalID: id, Time: at, Kind: core.TxReceive, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: asset, Symbol: "TKN", Amount: amt}}}
}

func (e *env) upsertTxs(txs ...core.Tx) {
	e.t.Helper()
	if _, err := e.act.Upsert(context.Background(), txs); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) review() templates.ReviewPage {
	e.t.Helper()
	d := e.deps
	d.Now, d.Log = time.Now, slog.Default()
	return (&Server{Deps: d}).reviewPage(context.Background())
}

func TestReviewGroupsByKindAndAsset(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	e.upsertTxs(
		reviewRecv("1", "hot", "spl:A", 1, at),
		reviewRecv("2", "hot", "spl:A", 2, at.Add(time.Hour)),
		reviewRecv("3", "hot", "spl:A", 3, at.Add(2*time.Hour)),
		reviewRecv("4", "hot", "SOL", 4, at),
	)
	p := e.review()
	var recv []templates.ReviewGroup
	for _, g := range p.Groups {
		if g.Kind == tax.IssueUnmatchedReceive {
			recv = append(recv, g)
		}
	}
	if len(recv) != 2 || recv[0].Asset != "spl:A" || recv[0].Count != 3 || recv[1].Asset != "SOL" || recv[1].Count != 1 {
		t.Fatalf("groups: %+v", p.Groups)
	}
	if recv[0].Title != "Unmatched receives" || len(recv[0].Items) != 3 {
		t.Fatalf("group: %+v", recv[0])
	}
	if recv[0].Items[0].TxID != "hot:3" {
		t.Errorf("items must be newest first: %+v", recv[0].Items)
	}
}

func TestReviewTagOptionsByDirection(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	out := reviewRecv("out", "hot", "SOL", -2, at)
	out.Kind = core.TxSend
	e.upsertTxs(reviewRecv("in", "hot", "SOL", 2, at), out)
	p := e.review()
	byID := map[string]templates.ReviewItem{}
	for _, g := range p.Groups {
		for _, it := range g.Items {
			byID[it.TxID] = it
		}
	}
	in, snd := byID["hot:in"], byID["hot:out"]
	if in.Direction != "in" || !slices.Equal(in.TagOptions, []string{"purchase", "income", "airdrop", "gift-received", "own-wallet", "position", "spam"}) {
		t.Errorf("receive item: %+v", in)
	}
	if snd.Direction != "out" || !slices.Equal(snd.TagOptions, []string{"sale", "gift", "lost", "own-wallet", "position", "spam"}) {
		t.Errorf("send item: %+v", snd)
	}
	pos := core.Tx{Kind: core.TxPosition}
	if got := reviewTagOptions(pos, "in", ""); !slices.Contains(got, "position-gain") {
		t.Errorf("position receive: %v", got)
	}
	if got := reviewTagOptions(core.Tx{Kind: core.TxReceive}, "in", tax.TagPosition); !slices.Contains(got, "position-gain") {
		t.Errorf("receive tagged position: %v", got)
	}
	if got := reviewTagOptions(pos, "out", ""); slices.Contains(got, "position-gain") {
		t.Errorf("send must not offer position-gain: %v", got)
	}
}

func TestReviewParkedRows(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol wallet", Holdings: []manual.Holding{{Asset: "SOL", Amount: 6}}})
	e.upsertTxs(core.Tx{ID: "sol:dep", SourceID: "sol", ExternalID: "dep", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Kind: core.TxPosition, Status: core.TxConfirmed, Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -4}}})
	p := e.review()
	if len(p.Parked) != 1 || p.Parked[0].WalletLabel != "Sol wallet" || p.Parked[0].Amount != 4 || p.Parked[0].Symbol != "SOL" {
		t.Fatalf("parked: %+v", p.Parked)
	}
	if p.OpenYear != 2026 {
		t.Errorf("open year %d", p.OpenYear)
	}
}

func TestStaleTagListed(t *testing.T) {
	e := newEnv(t, "")
	if err := e.taxSet.Update(func(s *tax.Settings) {
		s.Tags = map[string]tax.TagEntry{"gone:1": {Tag: tax.TagSpam}}
	}); err != nil {
		t.Fatal(err)
	}
	p := e.review()
	if len(p.Stale) != 1 || p.Stale[0].TxID != "gone:1" || p.Stale[0].Tag != tax.TagSpam {
		t.Fatalf("stale: %+v", p.Stale)
	}
}

func renderReview(t *testing.T, p templates.ReviewPage) string {
	t.Helper()
	var sb strings.Builder
	if err := templates.ReviewPanel(p).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func TestReviewGroupCap(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var txs []core.Tx
	for i := range 51 {
		txs = append(txs, reviewRecv(strconv.Itoa(i), "hot", "SOL", 1, at.Add(time.Duration(i)*time.Minute)))
	}
	e.upsertTxs(txs...)
	p := e.review()
	g := p.Groups[0]
	if g.Count != 51 || len(g.Items) != 50 || g.Items[0].TxID != "hot:50" {
		t.Fatalf("count %d items %d", g.Count, len(g.Items))
	}
	if html := renderReview(t, p); !strings.Contains(html, "+1 more") {
		t.Error("missing +1 more line")
	}
}

func TestReviewTaggedGroup(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	e.upsertTxs(reviewRecv("a", "hot", "SOL", 1, at), reviewRecv("b", "hot", "SOL", 2, at))
	if err := e.taxSet.Update(func(s *tax.Settings) {
		s.Tags = map[string]tax.TagEntry{"hot:a": {Tag: tax.TagAirdrop, Note: "n"}}
	}); err != nil {
		t.Fatal(err)
	}
	p := e.review()
	var issue, tagged *templates.ReviewGroup
	for i, g := range p.Groups {
		switch g.Kind {
		case tax.IssueUnmatchedReceive:
			issue = &p.Groups[i]
		case templates.ReviewTagged:
			tagged = &p.Groups[i]
		}
	}
	if issue == nil || issue.Count != 1 || issue.Items[0].TxID != "hot:b" {
		t.Fatalf("issue group: %+v", issue)
	}
	if tagged == nil || tagged.Title != "Tagged" || len(tagged.Items) != 1 || tagged.Items[0].TxID != "hot:a" || tagged.Items[0].Tag != "airdrop" {
		t.Fatalf("tagged group: %+v", tagged)
	}
	html := renderReview(t, p)
	i := strings.Index(html, "Tagged")
	if i < 0 || strings.Contains(html[strings.LastIndex(html[:i], "<details"):i], " open") {
		t.Error("Tagged group must be collapsed")
	}
}

func TestReviewPriceKey(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	ten := 10.0
	e.upsertTxs(
		core.Tx{ID: "hot:legs", SourceID: "hot", ExternalID: "legs", Time: at, Kind: core.TxReceive, Status: core.TxConfirmed, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: 1, USD: &ten},
			{AssetKey: "spl:Q", Symbol: "Q", Amount: 2},
		}},
		core.Tx{ID: "hot:fee", SourceID: "hot", ExternalID: "fee", Time: at.Add(time.Hour), Kind: core.TxSend, Status: core.TxConfirmed,
			Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -1, USD: &ten}},
			Fee:  &core.Leg{AssetKey: "ETH", Symbol: "ETH", Amount: -0.1}},
	)
	keys := map[string]string{}
	for _, g := range e.review().Groups {
		if g.Kind == tax.IssueMissingPrice {
			for _, it := range g.Items {
				keys[it.TxID+"/"+it.Asset] = it.PriceKey
			}
		}
	}
	if keys["hot:legs/spl:Q"] != tax.PriceKey("hot:legs", 1) || keys["hot:fee/ETH"] != tax.FeePriceKey("hot:fee") {
		t.Fatalf("price keys: %v", keys)
	}
}

func TestReviewActivityUnavailable(t *testing.T) {
	e := newEnv(t, "")
	if err := e.taxSet.Update(func(s *tax.Settings) {
		s.Tags = map[string]tax.TagEntry{"hot:a": {Tag: tax.TagSpam}}
	}); err != nil {
		t.Fatal(err)
	}
	d := e.deps
	d.Now, d.Log = time.Now, slog.Default()
	d.Activity = nil
	p := (&Server{Deps: d}).reviewPage(context.Background())
	if p.TagsChecked || len(p.Stale) != 0 || len(p.Groups) != 0 {
		t.Fatalf("nil activity: %+v", p)
	}
	if !strings.Contains(renderReview(t, p), "Activity couldn&#39;t be loaded; tags not checked") &&
		!strings.Contains(renderReview(t, p), "Activity couldn't be loaded; tags not checked") {
		t.Error("note missing")
	}
	e.db.Close() // activity reads now fail
	d.Activity = e.act
	p = (&Server{Deps: d}).reviewPage(context.Background())
	if p.TagsChecked || len(p.Stale) != 0 {
		t.Fatalf("failing activity: %+v", p)
	}
}

// ---- review actions -----------------------------------------------------------------------

func (e *env) reviewPost(path string, vals url.Values) string {
	e.t.Helper()
	body, hdr := form(vals)
	resp, out := e.do("POST", path, body, hdr)
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, out)
	}
	return out
}

func (e *env) taxBackups() int {
	e.t.Helper()
	files, _ := os.ReadDir(filepath.Join(e.dataDir, "backups"))
	n := 0
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "tax-") {
			n++
		}
	}
	return n
}

func reviewGroupOf(p templates.ReviewPage, kind tax.IssueKind, asset string) *templates.ReviewGroup {
	for i, g := range p.Groups {
		if g.Kind == kind && (asset == "" || g.Asset == asset) {
			return &p.Groups[i]
		}
	}
	return nil
}

func TestTagSavesAndRecomputes(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(reviewRecv("a", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	out := e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:a"}, "tag": {"income"}, "note": {"salary"}})
	if got := e.taxSet.Get().Tags["hot:a"]; got.Tag != tax.TagIncome || got.Note != "salary" {
		t.Fatalf("saved: %+v", got)
	}
	if strings.Contains(out, "Unmatched receives") || !strings.Contains(out, `id="review-panel"`) || !strings.Contains(out, `id="pnl"`) {
		t.Errorf("response: %s", out)
	}
	e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:a"}, "tag": {""}})
	if _, ok := e.taxSet.Get().Tags["hot:a"]; ok {
		t.Error("empty tag must remove it")
	}
}

func TestTagBulkOneWrite(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var txs []core.Tx
	for i := range 500 {
		txs = append(txs, reviewRecv(strconv.Itoa(i), "hot", "spl:SPAM", 1, at.Add(time.Duration(i)*time.Minute)))
	}
	e.upsertTxs(txs...)
	if err := e.taxSet.Update(func(s *tax.Settings) { s.Prices = map[string]float64{"x#0": 1} }); err != nil {
		t.Fatal(err)
	}
	before := e.taxBackups()
	out := e.reviewPost("/reports/tax/tag-bulk", url.Values{"kind": {string(tax.IssueUnmatchedReceive)}, "asset": {"spl:SPAM"}, "tag": {"spam"}})
	if got := len(e.taxSet.Get().Tags); got != 500 {
		t.Fatalf("tagged %d", got)
	}
	if after := e.taxBackups(); after-before != 1 {
		t.Errorf("backups %d -> %d, want exactly one write", before, after)
	}
	if reviewGroupOf(e.review(), tax.IssueUnmatchedReceive, "spl:SPAM") != nil || strings.Contains(out, "Unmatched receives") {
		t.Error("group should be gone")
	}
}

func TestTagWrongDirection(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(reviewRecv("a", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	out := e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:a"}, "tag": {"gift"}})
	if !strings.Contains(out, "apply to a receive") {
		t.Errorf("no error: %s", out)
	}
	if len(e.taxSet.Get().Tags) != 0 {
		t.Error("nothing may be saved")
	}
	out = e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:a"}, "tag": {"bogus"}})
	if !strings.Contains(out, "apply to a receive") || len(e.taxSet.Get().Tags) != 0 {
		t.Errorf("unknown tag: %s", out)
	}
}

func TestPriceOverride(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(reviewRecv("a", "hot", "spl:Q", 2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	if err := e.taxSet.Update(func(s *tax.Settings) { s.Tags = map[string]tax.TagEntry{"hot:a": {Tag: tax.TagIncome}} }); err != nil {
		t.Fatal(err)
	}
	if reviewGroupOf(e.review(), tax.IssueMissingPrice, "") == nil {
		t.Fatalf("setup: no missing price: %+v", e.review().Groups)
	}
	out := e.reviewPost("/reports/tax/price", url.Values{"key": {"hot:a#0"}, "usd": {"12.5"}})
	if e.taxSet.Get().Prices["hot:a#0"] != 12.5 {
		t.Fatalf("prices: %v", e.taxSet.Get().Prices)
	}
	if reviewGroupOf(e.review(), tax.IssueMissingPrice, "") != nil || strings.Contains(out, "Missing prices") {
		t.Error("missing price should be resolved")
	}
	out = e.reviewPost("/reports/tax/price", url.Values{"key": {"hot:a#0"}, "usd": {"abc"}})
	if !strings.Contains(out, "enter a number") || e.taxSet.Get().Prices["hot:a#0"] != 12.5 {
		t.Errorf("non-numeric: %s", out)
	}
	e.reviewPost("/reports/tax/price", url.Values{"key": {"hot:a#0"}, "usd": {""}})
	if _, ok := e.taxSet.Get().Prices["hot:a#0"]; ok {
		t.Error("empty usd must remove the override")
	}
}

func parkedEnv(e *env) {
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol wallet", Holdings: []manual.Holding{{Asset: "SOL", Amount: 6}}})
	e.upsertTxs(core.Tx{ID: "sol:dep", SourceID: "sol", ExternalID: "dep", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Kind: core.TxPosition, Status: core.TxConfirmed, Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -4}}})
}

func TestWriteOffSaves(t *testing.T) {
	e := newEnv(t, "")
	parkedEnv(e)
	e.reviewPost("/reports/tax/writeoff", url.Values{"wallet": {"sol"}, "asset": {"SOL"}, "amount": {"1.5"},
		"date": {"2026-05-01"}, "proceeds": {"0"}, "note": {"liquidated"}})
	w := e.taxSet.Get().WriteOffs
	if len(w) != 1 || w[0].Wallet != "sol" || w[0].Asset != "SOL" || w[0].Amount != 1.5 || w[0].Note != "liquidated" ||
		!w[0].Date.Equal(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("write-offs: %+v", w)
	}
	if p := e.review().Parked; len(p) != 1 || p[0].Amount != 2.5 {
		t.Fatalf("parked: %+v", p)
	}
	if p := e.review(); len(p.WriteOffs) != 1 {
		t.Errorf("write-off rows: %+v", p.WriteOffs)
	}
	out := e.reviewPost("/reports/tax/writeoff", url.Values{"wallet": {"sol"}, "asset": {"SOL"}, "amount": {"x"}, "date": {"2026-05-01"}})
	if !strings.Contains(out, "enter a number") || len(e.taxSet.Get().WriteOffs) != 1 {
		t.Errorf("bad amount: %s", out)
	}
	e.reviewPost("/reports/tax/writeoff/0/delete", nil)
	if len(e.taxSet.Get().WriteOffs) != 0 {
		t.Error("delete failed")
	}
}

func TestWriteOffInClosedYearRefused(t *testing.T) {
	e := newEnv(t, "")
	e.closeYear(2026, tax.Summary{})
	out := e.reviewPost("/reports/tax/writeoff", url.Values{"wallet": {"sol"}, "asset": {"SOL"}, "amount": {"1"}, "date": {"2026-05-01"}, "proceeds": {"0"}})
	if !strings.Contains(out, "2026 is closed; reopen it to change it") || len(e.taxSet.Get().WriteOffs) != 0 {
		t.Errorf("out: %s", out)
	}
}

func TestTagInClosedYearRefused(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(reviewRecv("a", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	e.closeYear(2026, tax.Summary{})
	out := e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:a"}, "tag": {"income"}})
	if !strings.Contains(out, "2026 is closed; reopen it to change it") || len(e.taxSet.Get().Tags) != 0 {
		t.Errorf("out: %s", out)
	}
}

func TestStaleTagDelete(t *testing.T) {
	e := newEnv(t, "")
	if err := e.taxSet.Update(func(s *tax.Settings) { s.Tags = map[string]tax.TagEntry{"gone:1": {Tag: tax.TagSpam}} }); err != nil {
		t.Fatal(err)
	}
	e.reviewPost("/reports/tax/tag/"+url.PathEscape("gone:1")+"/delete", nil)
	if len(e.taxSet.Get().Tags) != 0 {
		t.Error("stale tag not removed")
	}
}

func TestReviewEmptyStateNeedsTagsChecked(t *testing.T) {
	if html := renderReview(t, templates.ReviewPage{OpenYear: 2026}); strings.Contains(html, "Nothing needs review") {
		t.Error("must not claim nothing needs review when tags were not checked")
	}
	if html := renderReview(t, templates.ReviewPage{OpenYear: 2026, TagsChecked: true}); !strings.Contains(html, "Nothing needs review") {
		t.Error("empty state missing")
	}
}

func TestTagUsesItemAsset(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(core.Tx{ID: "hot:swap", SourceID: "hot", ExternalID: "swap", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Kind: core.TxOther, Status: core.TxConfirmed, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: -1}, {AssetKey: "spl:SPAM", Symbol: "SPAM", Amount: 5}}})
	out := e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:swap"}, "asset": {"spl:SPAM"}, "tag": {"airdrop"}})
	if strings.Contains(out, "doesn&#39;t apply") || e.taxSet.Get().Tags["hot:swap"].Tag != tax.TagAirdrop {
		t.Errorf("airdrop on the SPAM receive: %s", out)
	}
	out = e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:swap"}, "asset": {"SOL"}, "tag": {"income"}})
	if !strings.Contains(out, "apply to a send") {
		t.Errorf("income on the SOL send: %s", out)
	}
}

func TestTagPositionGainOnPositionMove(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(
		core.Tx{ID: "sol:wd", SourceID: "sol", ExternalID: "wd", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			Kind: core.TxPosition, Status: core.TxConfirmed, Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: 5}}},
		reviewRecv("plain", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	e.reviewPost("/reports/tax/tag", url.Values{"tx": {"sol:wd"}, "tag": {"position-gain"}})
	if e.taxSet.Get().Tags["sol:wd"].Tag != tax.TagPositionGain {
		t.Error("position-gain must be allowed on a position move")
	}
	out := e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:plain"}, "tag": {"position-gain"}})
	if !strings.Contains(out, "apply to a receive") || e.taxSet.Get().Tags["hot:plain"].Tag != "" {
		t.Errorf("plain receive: %s", out)
	}
}

func TestBulkRefusalCountsItems(t *testing.T) {
	e := newEnv(t, "")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	e.upsertTxs(reviewRecv("1", "hot", "spl:A", 1, at), reviewRecv("2", "hot", "spl:A", 1, at))
	out := e.reviewPost("/reports/tax/tag-bulk", url.Values{"kind": {string(tax.IssueUnmatchedReceive)}, "asset": {"spl:A"}, "tag": {"sale"}})
	if !strings.Contains(out, "apply to 2 of these transactions") || len(e.taxSet.Get().Tags) != 0 {
		t.Errorf("out: %s", out)
	}
}

func TestPriceRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.upsertTxs(reviewRecv("a", "hot", "spl:Q", 2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	for _, key := range []string{"nohash", "ghost:1#0", "#0"} {
		out := e.reviewPost("/reports/tax/price", url.Values{"key": {key}, "usd": {"1"}})
		if !strings.Contains(out, "unknown transaction") || len(e.taxSet.Get().Prices) != 0 {
			t.Errorf("key %q: %s", key, out)
		}
	}
	e.closeYear(2026, tax.Summary{})
	out := e.reviewPost("/reports/tax/price", url.Values{"key": {"hot:a#0"}, "usd": {"1"}})
	if !strings.Contains(out, "2026 is closed; reopen it to change it") || len(e.taxSet.Get().Prices) != 0 {
		t.Errorf("closed year: %s", out)
	}
}

func TestNumberParsing(t *testing.T) {
	for in, want := range map[string]bool{"1234.5": true, "1,234.5": true, "1,234,567": true, "1,5": false, "12,34": false, "": false, "NaN": false, "Inf": false, "x": false} {
		if _, ok := parseNumber(in); ok != want {
			t.Errorf("parseNumber(%q) ok=%v", in, ok)
		}
	}
	e := newEnv(t, "")
	parkedEnv(e)
	for _, amt := range []string{"-1", "0", "NaN", "1,5"} {
		out := e.reviewPost("/reports/tax/writeoff", url.Values{"wallet": {"sol"}, "asset": {"SOL"}, "amount": {amt}, "date": {"2026-05-01"}})
		if !strings.Contains(out, "enter a number") || len(e.taxSet.Get().WriteOffs) != 0 {
			t.Errorf("amount %q: %s", amt, out)
		}
	}
	out := e.reviewPost("/reports/tax/writeoff", url.Values{"wallet": {"sol"}, "asset": {"SOL"}, "amount": {"1"}, "date": {"2026-05-01"}, "proceeds": {"-5"}})
	if !strings.Contains(out, "enter a number") {
		t.Errorf("negative proceeds: %s", out)
	}
}

func TestWriteOffDeleteBadIndex(t *testing.T) {
	e := newEnv(t, "")
	parkedEnv(e)
	e.reviewPost("/reports/tax/writeoff", url.Values{"wallet": {"sol"}, "asset": {"SOL"}, "amount": {"1"}, "date": {"2026-05-01"}})
	for _, i := range []string{"5", "-1", "x"} {
		out := e.reviewPost("/reports/tax/writeoff/"+i+"/delete", nil)
		if !strings.Contains(out, "no longer exists") || len(e.taxSet.Get().WriteOffs) != 1 {
			t.Errorf("index %s: %s", i, out)
		}
	}
	e.closeYear(2026, tax.Summary{})
	out := e.reviewPost("/reports/tax/writeoff/0/delete", nil)
	if !strings.Contains(out, "2026 is closed") || len(e.taxSet.Get().WriteOffs) != 1 {
		t.Errorf("closed delete: %s", out)
	}
}

func TestRemoveStalePriceOverride(t *testing.T) {
	e := newEnv(t, "")
	if err := e.taxSet.Update(func(s *tax.Settings) { s.Prices = map[string]float64{"gone:1#0": 3} }); err != nil {
		t.Fatal(err)
	}
	e.reviewPost("/reports/tax/price", url.Values{"key": {"gone:1#0"}, "usd": {""}})
	if len(e.taxSet.Get().Prices) != 0 {
		t.Errorf("stale override not removed: %v", e.taxSet.Get().Prices)
	}
	out := e.reviewPost("/reports/tax/price", url.Values{"key": {"gone:1#0"}, "usd": {"2"}})
	if !strings.Contains(out, "unknown transaction") {
		t.Errorf("setting on unknown tx: %s", out)
	}
}
