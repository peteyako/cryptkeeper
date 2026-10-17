package report

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/tax"
)

func ny(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func lines(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }

func sampleGains(t *testing.T) YearReport {
	loc := ny(t)
	y := tax.Year{Year: 2026, Source: "cryptkeep", Disposals: []tax.Disposal{{
		Sold: time.Date(2026, 3, 2, 15, 4, 0, 0, loc), Acquired: time.Date(2025, 1, 1, 0, 0, 0, 0, loc), Various: true,
		Asset: "BTC", Amount: 0.5, CostUSD: 1000, ProceedsUSD: 1500, GainUSD: 500, Wallet: "cb", Term: tax.LongTerm,
	}}}
	return FromClosed(y, Resolver{
		Symbol:      func(string) (string, string) { return "BTC", "Bitcoin" },
		WalletLabel: func(string) string { return "Coinbase" },
	}, loc)
}

func TestGainsHeaderExact(t *testing.T) {
	var b strings.Builder
	if err := WriteGains(&b, sampleGains(t)); err != nil {
		t.Fatal(err)
	}
	l := lines(b.String())
	if l[0] != "Capital gains report 2026" || l[1] != "" ||
		l[2] != "Date Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period" {
		t.Errorf("head = %q", l[:3])
	}
}

func TestGainsRowFormat(t *testing.T) {
	var b strings.Builder
	_ = WriteGains(&b, sampleGains(t))
	want := "03/02/2026 15:04,VARIOUS,BTC,0.50000000,1000.00,1500.00,500.00,,Coinbase,Long term"
	if got := lines(b.String())[3]; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestGainsNonVariousAndShort(t *testing.T) {
	loc := ny(t)
	r := YearReport{Year: 2026, Loc: loc, Rows: []Row{{Disposal: tax.Disposal{
		Sold: time.Date(2026, 3, 2, 15, 4, 0, 0, loc), Acquired: time.Date(2026, 1, 5, 9, 30, 0, 0, loc),
		Amount: 1, Term: tax.ShortTerm, Note: "Fee", GainUSD: -0.004,
	}, Symbol: "SOL", WalletLabel: "W"}}}
	var b strings.Builder
	_ = WriteGains(&b, r)
	want := "03/02/2026 15:04,01/05/2026 09:30,SOL,1.00000000,0.00,0.00,0.00,Fee,W,Short term"
	if got := lines(b.String())[3]; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestIncomeTypeMapping(t *testing.T) {
	loc := time.UTC
	r := YearReport{Year: 2026, Loc: loc, Income: []tax.Income{
		{Date: time.Date(2026, 1, 2, 3, 4, 0, 0, loc), Asset: "SOL", Amount: 1.5, ValueUSD: 120, Type: "reward", Wallet: "w"},
		{Date: time.Date(2026, 1, 2, 3, 4, 0, 0, loc), Asset: "SOL", Amount: 1, ValueUSD: 10, Type: "position", Wallet: tax.Untracked},
	}}
	var b strings.Builder
	if err := WriteIncome(&b, r, Resolver{Symbol: func(string) (string, string) { return "SOL", "Solana" }}); err != nil {
		t.Fatal(err)
	}
	l := lines(b.String())
	if l[0] != "Income report 2026" || l[2] != "Date,Asset,Amount,Value (USD),Type,Description,Wallet Name" {
		t.Errorf("head %q", l[:3])
	}
	if l[3] != "01/02/2026 03:04,SOL,1.50000000,120.00,Reward,,w" {
		t.Errorf("row %q", l[3])
	}
	if !strings.Contains(l[4], ",Other income,,Untracked (from Koinly)") {
		t.Errorf("row %q", l[4])
	}
}

func TestHoldingsTotalRow(t *testing.T) {
	r := YearReport{Year: 2026, Loc: time.UTC, Holdings: []HoldingRow{
		{AssetKey: "SOL", Symbol: "SOL", Name: "Solana", Quantity: 2, CostUSD: 200, ValueUSD: 400, Price: 200, Priced: true},
		{AssetKey: "BTC", Symbol: "BTC", Name: "Bitcoin", Quantity: 0.5, CostUSD: 50, ValueUSD: 50, Price: 100, Priced: true},
	}}
	var b strings.Builder
	if err := WriteHoldings(&b, r); err != nil {
		t.Fatal(err)
	}
	l := lines(b.String())
	if l[0] != "Balances as at 12/31/2026 23:59" || l[2] != "Asset,Quantity,Cost (USD),Value (USD),Description" {
		t.Errorf("head %q", l[:3])
	}
	if l[3] != "SOL (Solana),2.00000000,200.00,400.00,@ $200.00 per SOL" {
		t.Errorf("row %q", l[3])
	}
	if last := l[len(l)-1]; last != "Total,-,250.00,450.00,-" {
		t.Errorf("last %q", last)
	}
}

func TestOtherGainsHeaderOnly(t *testing.T) {
	var b strings.Builder
	if err := WriteOtherGains(&b, sampleGains(t)); err != nil {
		t.Fatal(err)
	}
	want := "Other gains report 2026\n\nDate,Asset,Amount,Value (USD),Type,Wallet Name\n"
	if b.String() != want {
		t.Errorf("got %q", b.String())
	}
}

func TestOtherGainsRows(t *testing.T) {
	r := sampleGains(t)
	r.Rows = append(r.Rows, Row{Disposal: tax.Disposal{
		Sold: time.Date(2026, 4, 1, 10, 0, 0, 0, r.Loc), Amount: 3, ProceedsUSD: 30, Source: tax.SourcePositionGain,
	}, Symbol: "JUP", WalletLabel: "Sol"})
	var b strings.Builder
	_ = WriteOtherGains(&b, r)
	l := lines(b.String())
	if len(l) != 4 || l[3] != "04/01/2026 10:00,JUP,3.00000000,30.00,Position gain,Sol" {
		t.Errorf("got %q", l)
	}
}

func TestCSVQuotesLabels(t *testing.T) {
	r := sampleGains(t)
	r.Rows[0].WalletLabel = "Cold, Ledger"
	var b strings.Builder
	_ = WriteGains(&b, r)
	if !strings.Contains(b.String(), `,"Cold, Ledger",`) {
		t.Errorf("not quoted: %q", b.String())
	}
	cr := csv.NewReader(strings.NewReader(strings.SplitN(b.String(), "\n\n", 2)[1]))
	recs, err := cr.ReadAll()
	if err != nil || len(recs[1]) != 10 {
		t.Errorf("parse: %v %v", err, recs)
	}
}

func f64(v float64) *float64 { return &v }

func TestTransactionsKinds(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, loc)
	txs := []core.Tx{
		{ID: "cb:1", SourceID: "cb", Time: at, Kind: core.TxSwap, TxHash: "h1", Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: -2, USD: f64(-300)},
			{AssetKey: "USDC", Symbol: "USDC", Amount: 300, USD: f64(300)},
		}, Fee: &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.001, USD: f64(-0.15)}},
		{ID: "sol:2", SourceID: "sol", Time: at.Add(time.Hour), Kind: core.TxReward, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: 1, USD: f64(150)}}},
		{ID: "sol:3", SourceID: "sol", Time: time.Date(2025, 5, 6, 0, 0, 0, 0, loc), Kind: core.TxSend, Legs: []core.Leg{
			{AssetKey: "SOL", Symbol: "SOL", Amount: -1}}},
	}
	res := tax.Result{
		Disposals: []tax.Disposal{{TxID: "cb:1", CostUSD: 100, ProceedsUSD: 300, GainUSD: 200}, {TxID: "cb:1", Note: "Fee", CostUSD: 0.1, ProceedsUSD: 0.15, GainUSD: 0.05}},
		Income:    []tax.Income{{TxID: "sol:2", Type: "reward"}},
	}
	rs := Resolver{WalletLabel: func(id string) string { return map[string]string{"cb": "Coinbase", "sol": "Cold, Ledger"}[id] }}
	var b strings.Builder
	if err := WriteTransactions(&b, 2026, txs, res, rs, loc); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	l := lines(s)
	if l[0] != "Transaction report 2026" || l[2] != "Date,Type,Tag,Sending Wallet,Sent Amount,Sent Currency,Sent Cost Basis,Receiving Wallet,Received Amount,Received Currency,Received Cost Basis,Fee Amount,Fee Currency,Gain (USD),Net Value (USD),Fee Value (USD),TxSrc,TxDest,TxHash,Description" {
		t.Fatalf("head %q", l[:3])
	}
	if len(l) != 5 {
		t.Fatalf("want 2 data rows, got %q", l)
	}
	want1 := "2026-05-06 07:08:09 UTC,exchange,,Coinbase,2.00000000,SOL,100.00,Coinbase,300.00000000,USDC,300.00,0.00100000,SOL,200.05,300.00,0.15,,,h1,"
	if l[3] != want1 {
		t.Errorf("swap\n got %q\nwant %q", l[3], want1)
	}
	want2 := "2026-05-06 08:08:09 UTC,crypto_deposit,Reward,,,,,\"Cold, Ledger\",1.00000000,SOL,150.00,,,,150.00,,,,,"
	if l[4] != want2 {
		t.Errorf("reward\n got %q\nwant %q", l[4], want2)
	}
}

func TestPositionGainSplit(t *testing.T) {
	r := sampleGains(t)
	r.Rows = append(r.Rows, Row{Disposal: tax.Disposal{
		Sold: time.Date(2026, 4, 1, 10, 0, 0, 0, r.Loc), Amount: 3, ProceedsUSD: 30, Source: tax.SourcePositionGain,
	}, Symbol: "JUP", WalletLabel: "Sol", Box: "I"})
	var g, o strings.Builder
	_ = WriteGains(&g, r)
	_ = WriteOtherGains(&o, r)
	if strings.Contains(g.String(), "JUP") || !strings.Contains(o.String(), "JUP") {
		t.Errorf("gains %q other %q", g.String(), o.String())
	}
	n := 0
	for _, p := range Form8949(r) {
		n += p.Total.Count
	}
	if n != 2 {
		t.Errorf("8949 must include all rows")
	}
}

func TestTransactionsTransferPair(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, loc)
	out := core.Tx{ID: "a:1", SourceID: "a", Time: at, Kind: core.TxTransfer, PeerID: "b:1", PeerSource: "b", TxHash: "h",
		Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -2}},
		Fee:  &core.Leg{AssetKey: "SOL", Amount: -0.01, USD: f64(-1)}}
	in := core.Tx{ID: "b:1", SourceID: "b", Time: at, Kind: core.TxTransfer, PeerID: "a:1", PeerSource: "a",
		Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: 1.99}}}
	rs := Resolver{WalletLabel: func(id string) string { return strings.ToUpper(id) }}
	for _, txs := range [][]core.Tx{{out, in}, {in, out}} {
		var b strings.Builder
		if err := WriteTransactions(&b, 2026, txs, tax.Result{}, rs, loc); err != nil {
			t.Fatal(err)
		}
		l := lines(b.String())
		if len(l) != 4 {
			t.Fatalf("want one row, got %q", l)
		}
		want := "2026-05-06 07:08:09 UTC,transfer,,A,2.00000000,SOL,,B,1.99000000,SOL,,0.01000000,SOL,,,1.00,,,h,"
		if l[3] != want {
			t.Errorf("got %q want %q", l[3], want)
		}
	}
	var b strings.Builder
	_ = WriteTransactions(&b, 2026, []core.Tx{in}, tax.Result{}, rs, loc)
	if l := lines(b.String()); len(l) != 4 || !strings.Contains(l[3], ",transfer,,,,,,A,1.99000000,SOL") && !strings.Contains(l[3], ",transfer,,,,,,B,1.99000000,SOL") {
		t.Errorf("lone side %q", l)
	}
}

func TestTransactionsTagsAndInjection(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, loc)
	mk := func(id string, k core.TxKind, amt float64, note string) core.Tx {
		return core.Tx{ID: id, SourceID: "w", Time: at, Kind: k, Note: note, Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: amt}}}
	}
	txs := []core.Tx{mk("1", core.TxReceive, 1, "=HYPERLINK(x)"), mk("2", core.TxReceive, 1, ""), mk("3", core.TxSend, -1, ""), mk("4", core.TxSend, -1, "")}
	res := tax.Result{
		Income:    []tax.Income{{TxID: "1", Type: "income"}, {TxID: "2", Type: "airdrop"}},
		GiftsLost: []tax.Disposal{{TxID: "3", Note: "gift"}, {TxID: "4", Note: "lost"}},
	}
	var b strings.Builder
	_ = WriteTransactions(&b, 2026, txs, res, Resolver{}, loc)
	l := lines(b.String())
	for i, tag := range []string{"Income", "Airdrop", "Gift", "Lost"} {
		if f := strings.Split(l[3+i], ","); f[2] != tag {
			t.Errorf("row %d tag %q want %s", i, f[2], tag)
		}
	}
	if !strings.HasSuffix(l[3], ",'=HYPERLINK(x)") {
		t.Errorf("injection not guarded: %q", l[3])
	}
}

func TestHoldingsTotalWithUnpriced(t *testing.T) {
	r := YearReport{Year: 2026, Holdings: []HoldingRow{
		{Symbol: "A", Name: "A", Quantity: 1, CostUSD: 10, ValueUSD: 20, Price: 20, Priced: true},
		{Symbol: "B", Name: "B", Quantity: 1, CostUSD: 5},
	}}
	var b strings.Builder
	_ = WriteHoldings(&b, r)
	l := lines(b.String())
	if l[4] != "B (B),1.00000000,5.00,0.00,-" || l[5] != "Total,-,15.00,20.00,-" {
		t.Errorf("got %q", l[4:])
	}
}

func TestTransactionsStraddlingPair(t *testing.T) {
	loc := time.UTC
	rs := Resolver{WalletLabel: func(id string) string { return strings.ToUpper(id) }}
	pair := func(sendAt, recvAt time.Time) []core.Tx {
		return []core.Tx{
			{ID: "a:1", SourceID: "a", Time: sendAt, Kind: core.TxTransfer, PeerID: "b:1", Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -1}}},
			{ID: "b:1", SourceID: "b", Time: recvAt, Kind: core.TxTransfer, PeerID: "a:1", Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: 1}}},
		}
	}
	rows := func(txs []core.Tx, year int) []string {
		var b strings.Builder
		if err := WriteTransactions(&b, year, txs, tax.Result{}, rs, loc); err != nil {
			t.Fatal(err)
		}
		return lines(b.String())[3:]
	}
	d := func(y, m, dd, h, min int) time.Time { return time.Date(y, time.Month(m), dd, h, min, 0, 0, loc) }
	for name, txs := range map[string][]core.Tx{
		"send first":    pair(d(2026, 12, 31, 23, 58), d(2027, 1, 1, 0, 3)),
		"receive first": pair(d(2027, 1, 1, 0, 2), d(2026, 12, 31, 23, 55)),
	} {
		if got := rows(txs, 2026); len(got) != 1 || !strings.Contains(got[0], ",A,1.00000000,SOL") || !strings.Contains(got[0], ",B,1.00000000,SOL") {
			t.Errorf("%s 2026: %q", name, got)
		}
		if got := rows(txs, 2027); len(got) != 0 {
			t.Errorf("%s 2027: %q", name, got)
		}
	}
}

func TestCSVFormulaGuardOnLabelsAndSymbols(t *testing.T) {
	loc := time.UTC
	r := YearReport{Year: 2026, Loc: loc,
		Rows:     []Row{{Disposal: tax.Disposal{Sold: time.Now()}, Symbol: "=SYM", WalletLabel: "@wal"}},
		Income:   []tax.Income{{Date: time.Now(), Wallet: "w", Asset: "A"}},
		Holdings: []HoldingRow{{Symbol: "+H", Name: "-N"}},
	}
	rs := Resolver{Symbol: func(string) (string, string) { return "=X", "" }, WalletLabel: func(string) string { return "-lbl" }}
	var g, o, i, h strings.Builder
	_ = WriteGains(&g, r)
	_ = WriteOtherGains(&o, r)
	_ = WriteIncome(&i, r, rs)
	_ = WriteHoldings(&h, r)
	for _, c := range []struct{ out, want string }{{g.String(), "'=SYM"}, {g.String(), "'@wal"}, {i.String(), "'=X"}, {i.String(), "'-lbl"}, {h.String(), "'+H (-N)"}} {
		if !strings.Contains(c.out, c.want) {
			t.Errorf("missing %q in %q", c.want, c.out)
		}
	}
}
