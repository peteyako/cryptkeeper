package costbasis

import (
	"reflect"
	"strings"
	"testing"

	"crypto-portfolio/internal/core"
)

func parseKr(t *testing.T, name string, opts ...ParseOptions) []core.Tx {
	t.Helper()
	txs, err := ParseKrakenLedgersCSV(openFixture(t, name), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return txs
}

func byRef(t *testing.T, txs []core.Tx, ref string) core.Tx {
	t.Helper()
	for _, tx := range txs {
		if tx.ExternalID == ref {
			return tx
		}
	}
	t.Fatalf("no tx with refid %s", ref)
	return core.Tx{}
}

func wantFee(t *testing.T, tx core.Tx, asset string, amount float64) {
	t.Helper()
	if tx.Fee == nil {
		t.Fatalf("%s: fee nil, want %s %v", tx.ExternalID, asset, amount)
	}
	if tx.Fee.AssetKey != asset {
		t.Errorf("fee asset = %q, want %q", tx.Fee.AssetKey, asset)
	}
	near(t, "fee amount", tx.Fee.Amount, amount)
}

func TestKrakenLedgersGroupingKindsAndValuation(t *testing.T) {
	txs := parseKr(t, "kraken_ledgers.csv")
	if len(txs) != 9 {
		t.Fatalf("got %d txs, want 9 (one per refid)", len(txs))
	}
	for i := 1; i < len(txs); i++ {
		if txs[i].Time.Before(txs[i-1].Time) {
			t.Fatal("not sorted by time")
		}
	}
	for _, tx := range txs {
		if tx.SourceID != "kraken-csv" || tx.ID != "kraken-csv:"+tx.ExternalID || tx.Status != core.TxConfirmed {
			t.Errorf("bad envelope %+v", tx)
		}
	}

	// Fiat deposit: receive, ZUSD normalized, valued 1:1.
	dep := byRef(t, txs, "TAAAAA-AAAAA-AAAAA1")
	if dep.Kind != core.TxReceive || len(dep.Legs) != 1 {
		t.Fatalf("deposit = %+v", dep)
	}
	wantLeg(t, dep.Legs[0], "USD", 2000, usd(2000))
	if dep.Fee != nil {
		t.Errorf("zero fee should be nil: %+v", dep.Fee)
	}

	// USD-quoted buy: two legs in ledger order, BTC valued at USD paid + fee.
	buy := byRef(t, txs, "TBBBBB-BBBBB-BBBBB1")
	if buy.Kind != core.TxTrade || len(buy.Legs) != 2 {
		t.Fatalf("buy = %+v", buy)
	}
	wantLeg(t, buy.Legs[0], "BTC", 0.05, usd(1503.90))
	wantLeg(t, buy.Legs[1], "USD", -1500, usd(-1500))
	wantFee(t, buy, "USD", -3.9)
	if got := buy.Time.Format("2006-01-02 15:04:05"); got != "2021-02-01 10:30:15" || buy.Time.Location().String() != "UTC" {
		t.Errorf("time = %v", buy.Time)
	}

	// EUR-quoted spend + receive becomes a trade with no USD values.
	eur := byRef(t, txs, "TCCCCC-CCCCC-CCCCC1")
	if eur.Kind != core.TxTrade || len(eur.Legs) != 2 {
		t.Fatalf("eur trade = %+v", eur)
	}
	wantLeg(t, eur.Legs[0], "EUR", -100, nil)
	wantLeg(t, eur.Legs[1], "ETH", 0.06, nil)
	wantFee(t, eur, "EUR", -1.5)
	if eur.Time.Nanosecond() == 0 {
		t.Error("fractional seconds dropped")
	}

	// Staking income: reward, DOT28.S normalized to DOT, no USD yet.
	stake := byRef(t, txs, "TDDDDD-DDDDD-DDDDD1")
	if stake.Kind != core.TxReward || len(stake.Legs) != 1 {
		t.Fatalf("staking = %+v", stake)
	}
	wantLeg(t, stake.Legs[0], "DOT", 0.01, nil)

	// Spot -> staking move: a transfer; both legs normalize to ETH.
	alloc := byRef(t, txs, "TEEEEE-EEEEE-EEEEE1")
	if alloc.Kind != core.TxTransfer || len(alloc.Legs) != 2 {
		t.Fatalf("allocation = %+v", alloc)
	}
	wantLeg(t, alloc.Legs[0], "ETH", -0.06, nil)
	wantLeg(t, alloc.Legs[1], "ETH", 0.06, nil)

	// Sell: BTC valued at USD received minus the fee.
	sell := byRef(t, txs, "TFFFFF-FFFFF-FFFFF1")
	wantLeg(t, sell.Legs[0], "BTC", -0.01, usd(-598.44))
	wantLeg(t, sell.Legs[1], "USD", 600, usd(600))
	wantFee(t, sell, "USD", -1.56)

	// Withdrawal: send, fee in the same asset.
	wd := byRef(t, txs, "TGGGGG-GGGGG-GGGGG1")
	if wd.Kind != core.TxSend || len(wd.Legs) != 1 {
		t.Fatalf("withdrawal = %+v", wd)
	}
	wantLeg(t, wd.Legs[0], "BTC", -0.03, nil)
	wantFee(t, wd, "BTC", -0.0005)

	// Earn reward.
	er := byRef(t, txs, "THHHHH-HHHHH-HHHHH1")
	if er.Kind != core.TxReward {
		t.Fatalf("earn reward = %+v", er)
	}
	wantLeg(t, er.Legs[0], "DOT", 0.02, nil)

	// Earn allocation of a stablecoin: transfer, both legs kept.
	ea := byRef(t, txs, "TIIIII-IIIII-IIIII1")
	if ea.Kind != core.TxTransfer || len(ea.Legs) != 2 {
		t.Fatalf("earn allocation = %+v", ea)
	}
}

func TestKrakenLedgersMissingWalletColumn(t *testing.T) {
	txs := parseKr(t, "kraken_nowallet.csv", ParseOptions{SourceID: "kraken-main"})
	if len(txs) != 1 {
		t.Fatalf("txs = %+v", txs)
	}
	tx := txs[0]
	if tx.SourceID != "kraken-main" || tx.ID != "kraken-main:TJJJJJ-JJJJJ-JJJJJ1" {
		t.Errorf("ids = %q %q", tx.SourceID, tx.ID)
	}
	// USDC (stable) quote: ETH is valued from it, fee is in the base asset (no adjustment).
	wantLeg(t, tx.Legs[0], "USDC", -250, usd(-250))
	wantLeg(t, tx.Legs[1], "ETH", 0.1, usd(250))
	wantFee(t, tx, "ETH", -0.0002)
}

func TestKrakenLedgersIdempotent(t *testing.T) {
	a := parseKr(t, "kraken_ledgers.csv")
	b := parseKr(t, "kraken_ledgers.csv")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two parses differ")
	}
}

func TestKrakenLedgersAssetNormalization(t *testing.T) {
	raw := `"txid","refid","time","type","subtype","aclass","asset","wallet","amount","fee","balance"
"L1","R1","2022-01-01 00:00:00","deposit","","currency","XXBT","spot / main","1","0","1"
"L2","R2","2022-01-02 00:00:00","deposit","","currency","XBT.M","spot / main","1","0","1"
"L3","R3","2022-01-03 00:00:00","deposit","","currency","XDG","spot / main","10","0","10"
"L4","R4","2022-01-04 00:00:00","deposit","","currency","USD.HOLD","spot / main","10","0","10"
`
	txs, err := ParseKrakenLedgersCSV(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"BTC", "BTC", "DOGE", "USD"}
	for i, tx := range txs {
		if tx.Legs[0].AssetKey != want[i] {
			t.Errorf("tx %d asset = %q, want %q", i, tx.Legs[0].AssetKey, want[i])
		}
	}
}

func TestKrakenLedgersOtherTypes(t *testing.T) {
	raw := `"txid","refid","time","type","subtype","aclass","asset","wallet","amount","fee","balance"
"L1","R1","2022-01-01 00:00:00","transfer","airdrop","currency","FLR","spot / main","12","0","12"
"L2","R2","2022-01-02 00:00:00","transfer","","currency","XXBT","spot / main","0.1","0","0.1"
"L3","R3","2022-01-03 00:00:00","earn","autoallocation","currency","ETH","earn / bonded","-0.001","0","0"
"L4","R4","2022-01-04 00:00:00","staking","","currency","SOL03.S","spot / staking","0.2","0","0.2"
"L5","R5","2022-01-05 00:00:00","adjustment","","currency","XXBT","spot / main","0.001","0","0.1"
"L6","R6","2022-01-06 00:00:00","receive","","currency","XXBT","spot / main","0.5","0","0.5"
"L7","R7","2022-01-07 00:00:00","trade","","currency","XXBT","spot / main","0","0","0.5"
`
	txs, err := ParseKrakenLedgersCSV(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]core.TxKind{}
	for _, tx := range txs {
		got[tx.ExternalID] = tx.Kind
	}
	want := map[string]core.TxKind{
		"R1": core.TxReward,   // airdrop, income at FMV
		"R2": core.TxReceive,  // plain incoming transfer, unknown basis
		"R3": core.TxTransfer, // allocation
		"R4": core.TxReward,
		"R5": core.TxOther,
		"R6": core.TxReceive, // lone receive leg
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kinds = %v\nwant    %v", got, want)
	}
}

func TestKrakenLedgersErrors(t *testing.T) {
	hdr := `"txid","refid","time","type","subtype","aclass","asset","wallet","amount","fee","balance"` + "\n"
	cases := map[string]string{
		"empty":        "",
		"no refid col": `"txid","time","type","asset","amount"` + "\n" + `"a","2022-01-01 00:00:00","trade","XXBT","1"` + "\n",
		"bad time":     hdr + `"L1","R1","soon","deposit","","currency","XXBT","w","1","0","1"` + "\n",
		"bad amount":   hdr + `"L1","R1","2022-01-01 00:00:00","deposit","","currency","XXBT","w","lots","0","1"` + "\n",
		"bad fee":      hdr + `"L1","R1","2022-01-01 00:00:00","deposit","","currency","XXBT","w","1","x","1"` + "\n",
		"no asset":     hdr + `"L1","R1","2022-01-01 00:00:00","deposit","","currency","","w","1","0","1"` + "\n",
		"no refid":     hdr + `"","","2022-01-01 00:00:00","deposit","","currency","XXBT","w","1","0","1"` + "\n",
	}
	for name, raw := range cases {
		if _, err := ParseKrakenLedgersCSV(strings.NewReader(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := ParseKrakenLedgersCSV(strings.NewReader(hdr + `"L1","R1","soon","deposit","","currency","XXBT","w","1","0","1"` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the line: %v", err)
	}
}

func TestKrakenCSVIntoEngine(t *testing.T) {
	res := Compute(parseKr(t, "kraken_ledgers.csv"), Manual{})
	btc := res.Assets["BTC"]
	// buy 0.05 for 1503.90 (avg 30078); sell 0.01 for 598.44 -> realized 598.44-300.78;
	// withdrawal of 0.03 and its 0.0005 fee only remove units.
	near(t, "realized", btc.RealizedByYear[2021], 598.44-300.78)
	near(t, "amount", btc.Amount, 0.05-0.01-0.03-0.0005)
	near(t, "avg", btc.AvgCost, 30078)
	if _, ok := res.Assets["USD"]; ok {
		t.Error("fiat tracked")
	}
	// EUR-quoted ETH has unknown basis; the allocation transfer changed nothing.
	eth := res.Assets["ETH"]
	near(t, "eth unknown", eth.UnknownBasisUnits, 0.06)
	near(t, "eth amount", eth.Amount, 0)
	// Staking reward without a price is unknown basis until valued.
	near(t, "dot unknown", res.Assets["DOT"].UnknownBasisUnits, 0.03)
}
