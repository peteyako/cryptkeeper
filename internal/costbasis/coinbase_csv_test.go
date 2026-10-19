package costbasis

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/tax"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func parseCB(t *testing.T, name string, opts ...ParseOptions) []core.Tx {
	t.Helper()
	txs, err := ParseCoinbaseCSV(openFixture(t, name), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return txs
}

func byNote(t *testing.T, txs []core.Tx, time string) core.Tx {
	t.Helper()
	for _, tx := range txs {
		if tx.Time.Format("2006-01-02T15:04:05Z") == time {
			return tx
		}
	}
	t.Fatalf("no tx at %s", time)
	return core.Tx{}
}

func wantLeg(t *testing.T, l core.Leg, asset string, amount float64, usd *float64) {
	t.Helper()
	if l.AssetKey != asset || l.Symbol != asset {
		t.Errorf("leg asset = %q/%q, want %q", l.AssetKey, l.Symbol, asset)
	}
	near(t, asset+" amount", l.Amount, amount)
	switch {
	case usd == nil && l.USD != nil:
		t.Errorf("%s usd = %v, want nil", asset, *l.USD)
	case usd != nil && l.USD == nil:
		t.Errorf("%s usd = nil, want %v", asset, *usd)
	case usd != nil:
		near(t, asset+" usd", *l.USD, *usd)
	}
}

func TestCoinbaseOldHeaderWithPreamble(t *testing.T) {
	var skipped []string
	txs := parseCB(t, "coinbase_old.csv", ParseOptions{OnSkip: func(line int, reason string) {
		skipped = append(skipped, reason)
	}})
	if len(txs) != 14 {
		t.Fatalf("got %d txs, want 14", len(txs))
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want fiat Deposit and Pro Deposit", skipped)
	}
	for i := 1; i < len(txs); i++ {
		if txs[i].Time.Before(txs[i-1].Time) {
			t.Fatal("not sorted by time")
		}
	}
	for _, tx := range txs {
		if tx.SourceID != "coinbase-csv" || tx.Status != core.TxConfirmed || tx.ExternalID == "" ||
			tx.ID != core.TxID("coinbase-csv", tx.ExternalID) || tx.Time.Location() != time.UTC {
			t.Errorf("bad envelope: %+v", tx)
		}
	}

	// Buy: one positive leg, USD = total incl. fees.
	buy := byNote(t, txs, "2021-03-10T09:15:30Z")
	if buy.Kind != core.TxTrade || buy.Note != "buy" || len(buy.Legs) != 1 || buy.Fee != nil {
		t.Fatalf("buy = %+v", buy)
	}
	wantLeg(t, buy.Legs[0], "BTC", 0.01, usd(580))

	// Quoted money with a thousands separator.
	buy2 := byNote(t, txs, "2021-04-02T18:22:10Z")
	wantLeg(t, buy2.Legs[0], "ETH", 1, usd(1990))

	// Sell: negative leg, negative USD = proceeds (Total).
	sell := byNote(t, txs, "2021-06-11T12:00:00Z")
	if sell.Kind != core.TxTrade || sell.Note != "sell" || len(sell.Legs) != 1 {
		t.Fatalf("sell = %+v", sell)
	}
	wantLeg(t, sell.Legs[0], "BTC", -0.005, usd(-181.30))

	// Convert: two legs valued from Subtotal.
	conv := byNote(t, txs, "2021-05-20T00:00:05Z")
	if conv.Kind != core.TxTrade || conv.Note != "convert" || len(conv.Legs) != 2 {
		t.Fatalf("convert = %+v", conv)
	}
	wantLeg(t, conv.Legs[0], "ETH", -0.5, usd(-1200))
	wantLeg(t, conv.Legs[1], "USDC", 1200, usd(1200))

	// Convert notes with a thousands separator in the received quantity.
	conv2 := byNote(t, txs, "2022-03-02T09:00:00Z")
	wantLeg(t, conv2.Legs[0], "BTC", -0.001, usd(-43))
	wantLeg(t, conv2.Legs[1], "DOGE", 1000.50, usd(43))

	// Send / Receive carry no USD (a transfer is not a priced event) and parse the counterparty.
	send := byNote(t, txs, "2021-08-15T10:00:00Z")
	if send.Kind != core.TxSend || len(send.Legs) != 1 || send.Counterparty != "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa" {
		t.Fatalf("send = %+v", send)
	}
	wantLeg(t, send.Legs[0], "BTC", -0.002, nil)
	recv := byNote(t, txs, "2021-09-01T10:00:00Z")
	if recv.Kind != core.TxReceive || recv.Counterparty != "an external account" {
		t.Fatalf("receive = %+v", recv)
	}
	wantLeg(t, recv.Legs[0], "ETH", 0.1, nil)

	// Every reward type: positive leg valued at quantity x price at transaction.
	rewards := map[string]struct {
		asset string
		qty   float64
		usd   float64
		note  string
	}{
		"2021-07-01T08:00:00Z": {"ETH", 0.001, 2.30, "staking income"},
		"2021-07-02T08:00:00Z": {"XLM", 10, 2.50, "coinbase earn"},
		"2021-10-01T10:00:00Z": {"USDC", 1.25, 1.25, "rewards income"},
		"2021-11-01T10:00:00Z": {"CGLD", 1, 4, "learning reward"},
		"2021-12-01T10:00:00Z": {"ALGO", 2, 3, "inflation reward"},
	}
	for ts, want := range rewards {
		r := byNote(t, txs, ts)
		if r.Kind != core.TxReward || len(r.Legs) != 1 || r.Note != want.note {
			t.Errorf("%s: %+v", ts, r)
			continue
		}
		wantLeg(t, r.Legs[0], want.asset, want.qty, usd(want.usd))
	}

	// Advanced Trade Buy/Sell behave like Buy/Sell.
	atb := byNote(t, txs, "2022-01-05T09:00:00Z")
	wantLeg(t, atb.Legs[0], "SOL", 2, usd(301.80))
	ats := byNote(t, txs, "2022-02-05T09:00:00Z")
	wantLeg(t, ats.Legs[0], "SOL", -1, usd(-109.34))
	if atb.Note != "buy" || ats.Note != "sell" {
		t.Errorf("notes %q %q", atb.Note, ats.Note)
	}
}

func TestCoinbaseNewHeaderWithIDsAndNonUSD(t *testing.T) {
	txs := parseCB(t, "coinbase_new.csv")
	if len(txs) != 7 { // fiat Withdrawal is skipped
		t.Fatalf("got %d txs, want 7", len(txs))
	}
	buy := byNote(t, txs, "2023-04-12T14:03:22Z")
	if buy.ExternalID != "64350a3f9d1c2b0001a1b2c1" || buy.ID != "coinbase-csv:64350a3f9d1c2b0001a1b2c1" {
		t.Errorf("ids = %q %q", buy.ExternalID, buy.ID)
	}
	wantLeg(t, buy.Legs[0], "BTC", 0.02, usd(615))

	// EUR-priced rows have no USD value: the valuation step fills it later.
	eur := byNote(t, txs, "2023-04-13T09:00:00Z")
	wantLeg(t, eur.Legs[0], "ETH", 1, nil)

	stake := byNote(t, txs, "2023-05-01T00:00:00Z")
	if stake.Kind != core.TxReward {
		t.Errorf("kind = %s", stake.Kind)
	}
	wantLeg(t, stake.Legs[0], "ETH", 0.0005, usd(0.95))

	conv := byNote(t, txs, "2023-06-02T12:30:00Z")
	wantLeg(t, conv.Legs[0], "BTC", -0.005, usd(-135))
	wantLeg(t, conv.Legs[1], "ADA", 2048.25, usd(135))

	sell := byNote(t, txs, "2023-06-01T12:30:00Z")
	wantLeg(t, sell.Legs[0], "BTC", -0.01, usd(-266.40))
}

func TestCoinbaseSourceIDOption(t *testing.T) {
	txs := parseCB(t, "coinbase_new.csv", ParseOptions{SourceID: "coinbase-main"})
	for _, tx := range txs {
		if tx.SourceID != "coinbase-main" || tx.ID != "coinbase-main:"+tx.ExternalID {
			t.Fatalf("tx = %+v", tx)
		}
	}
}

func TestCoinbaseExternalIDsAreIdempotent(t *testing.T) {
	a := parseCB(t, "coinbase_old.csv")
	b := parseCB(t, "coinbase_old.csv")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two parses of the same file differ")
	}
	seen := map[string]bool{}
	for _, tx := range a {
		if seen[tx.ExternalID] {
			t.Fatalf("duplicate external id %s", tx.ExternalID)
		}
		seen[tx.ExternalID] = true
	}
}

const cbHeader = "Timestamp,Transaction Type,Asset,Quantity Transacted,Spot Price Currency,Spot Price at Transaction,Subtotal,Total (inclusive of fees and/or spread),Fees and/or Spread,Notes\n"

func TestCoinbaseHashIDsIgnoreTimestampFormatAndKeepIdenticalRowsDistinct(t *testing.T) {
	iso := cbHeader + "2021-03-10T09:15:30Z,Buy,BTC,0.01,USD,\"$57,000.00\",$570.00,$580.00,$10.00,Bought\n"
	spaced := cbHeader + "2021-03-10 09:15:30 UTC,Buy,BTC,0.01000000,USD,$57000,$570,$580.00,$10,Bought\n"
	a, err := ParseCoinbaseCSV(strings.NewReader(iso))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseCoinbaseCSV(strings.NewReader(spaced))
	if err != nil {
		t.Fatal(err)
	}
	if a[0].ExternalID != b[0].ExternalID {
		t.Errorf("same economic row hashed differently: %s vs %s", a[0].ExternalID, b[0].ExternalID)
	}

	// Two genuinely identical rows must both survive (distinct ids), stable across parses.
	dup := cbHeader + strings.Repeat("2021-03-10T09:15:30Z,Buy,BTC,0.01,USD,$57000,$570.00,$580.00,$10.00,Bought\n", 2)
	d1, _ := ParseCoinbaseCSV(strings.NewReader(dup))
	d2, _ := ParseCoinbaseCSV(strings.NewReader(dup))
	if len(d1) != 2 || d1[0].ExternalID == d1[1].ExternalID {
		t.Fatalf("identical rows collapsed: %+v", d1)
	}
	if !reflect.DeepEqual(d1, d2) {
		t.Error("ids not stable")
	}
}

func TestCoinbaseTolerances(t *testing.T) {
	// BOM, CRLF line endings, shuffled column order, reordered preamble, lower-case header.
	raw := "\ufeffTransactions\r\nUser,Jane,abc\r\n\r\n" +
		"notes,total (inclusive of fees and/or spread),transaction type,asset,timestamp,quantity transacted,spot price currency,spot price at transaction\r\n" +
		"Bought,\"$1,234.50\",Buy,eth,2021-03-10T09:15:30Z,1.5,USD,$800\r\n"
	txs, err := ParseCoinbaseCSV(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Fatalf("txs = %+v", txs)
	}
	wantLeg(t, txs[0].Legs[0], "ETH", 1.5, usd(1234.50))
}

func TestCoinbaseMoneyParsing(t *testing.T) {
	cases := map[string]struct {
		v  float64
		ok bool
	}{
		"$1,234.56":  {1234.56, true},
		"1234.56":    {1234.56, true},
		"-$5.00":     {-5, true},
		"$-5.00":     {-5, true},
		"($5.00)":    {-5, true},
		" $0.00 ":    {0, true},
		"$1,000,000": {1000000, true},
		"":           {0, false},
		"-":          {0, false},
		"N/A":        {0, false},
		"$":          {0, false},
	}
	for in, want := range cases {
		v, ok := parseMoney(in)
		if ok != want.ok || (ok && v != want.v) {
			t.Errorf("parseMoney(%q) = %v,%v want %v,%v", in, v, ok, want.v, want.ok)
		}
	}
}

func TestCoinbaseErrors(t *testing.T) {
	cases := map[string]string{
		"no header":          "Transactions\nUser,Jane,abc\n",
		"empty":              "",
		"bad timestamp":      cbHeader + "yesterday,Buy,BTC,1,USD,$1,$1,$1,$0,x\n",
		"bad quantity":       cbHeader + "2021-03-10T09:15:30Z,Buy,BTC,lots,USD,$1,$1,$1,$0,x\n",
		"missing asset":      cbHeader + "2021-03-10T09:15:30Z,Buy,,1,USD,$1,$1,$1,$0,x\n",
		"unparsable convert": cbHeader + "2021-03-10T09:15:30Z,Convert,ETH,1,USD,$1,$1,$1,$0,something odd\n",
	}
	for name, raw := range cases {
		if _, err := ParseCoinbaseCSV(strings.NewReader(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := ParseCoinbaseCSV(strings.NewReader(cbHeader + "\n2021-03-10T09:15:30Z,Buy,BTC,lots,USD,$1,$1,$1,$0,x\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should name the line: %v", err)
	}
}

func TestCoinbaseUnknownTypesAreSkippedNotFatal(t *testing.T) {
	raw := cbHeader +
		"2021-03-10T09:15:30Z,Mystery Event,BTC,1,USD,$1,$1,$1,$0,x\n" +
		"2021-03-11T09:15:30Z,Buy,BTC,1,USD,$1,$1,$1,$0,x\n"
	var reasons []string
	txs, err := ParseCoinbaseCSV(strings.NewReader(raw), ParseOptions{OnSkip: func(line int, reason string) {
		reasons = append(reasons, reason)
	}})
	if err != nil || len(txs) != 1 {
		t.Fatalf("txs=%d err=%v", len(txs), err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "Mystery Event") {
		t.Errorf("reasons = %v", reasons)
	}
}

// TestCoinbaseCSVIntoEngine feeds the parsed history to the tax engine: one FIFO wallet,
// coinbase-csv, replayed from the start (no closed year).
func TestCoinbaseCSVIntoEngine(t *testing.T) {
	res := tax.Compute(tax.Input{Loc: time.UTC, Txs: parseCB(t, "coinbase_old.csv")})
	// BTC: buy 0.01 for 580 -> sell 0.005 for 181.30 (cost 290) -> send 0.002 with no value
	// (cost 116, proceeds 0) -> convert 0.001 for 43 (cost 58).
	btc := gains(res, "BTC")
	near(t, "btc 2021", btc[2021], (181.30-290)+(0-116))
	near(t, "btc 2022", btc[2022], 43-58)
	amount, cost, _ := res.Holding("BTC", nil)
	near(t, "btc left", amount, 0.002)
	near(t, "btc cost left", cost, 116)
	if !hasIssue(res, tax.IssueUnmatchedSend, "BTC") || !hasIssue(res, tax.IssueMissingPrice, "BTC") {
		t.Errorf("the unvalued send is up for review: %+v", res.Issues)
	}
	// ETH: buy 1 for 1990, convert 0.5 for 1200 (cost 995); staking 0.001 at 2.30; a
	// receive of 0.1 with no value is a lot at cost 0.
	near(t, "eth 2021", gains(res, "ETH")[2021], 1200-995)
	amount, cost, _ = res.Holding("ETH", nil)
	near(t, "eth left", amount, 0.601)
	near(t, "eth cost left", cost, 995+2.30)
	if !hasIssue(res, tax.IssueUnmatchedReceive, "ETH") {
		t.Errorf("the receive is up for review: %+v", res.Issues)
	}
	_, usdc, _ := res.Holding("USDC", nil)
	near(t, "usdc basis", usdc, 1200+1.25)
	_, doge, _ := res.Holding("DOGE", nil)
	near(t, "doge basis", doge, 43)
	near(t, "sol 2022", gains(res, "SOL")[2022], 109.34-150.90)
	// Staking, Earn, Rewards, Learning and Inflation rewards are income at market value.
	var income float64
	for _, in := range res.Income {
		income += in.ValueUSD
	}
	if len(res.Income) != 5 {
		t.Errorf("%d income entries, want 5", len(res.Income))
	}
	near(t, "income", income, 2.30+2.50+1.25+4+3)
}
