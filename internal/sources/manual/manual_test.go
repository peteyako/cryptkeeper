package manual

import (
	"context"
	"testing"
)

func TestParseAndFetch(t *testing.T) {
	hs, err := ParseLines("btc 0.25 owed by Sam\n\n# comment\nSPL:JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN 1,000\neth 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 3 || hs[0].Asset != "BTC" || hs[0].Note != "owed by Sam" || hs[1].Amount != 1000 {
		t.Fatalf("%+v", hs)
	}
	if hs[1].Asset != "spl:JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN" {
		t.Fatalf("mint case must be preserved: %q", hs[1].Asset)
	}
	res, _ := New("mom", hs).Fetch(context.Background())
	if len(res.Holdings) != 3 || res.Holdings[1].Mint == "" || res.Holdings[0].WalletID != "mom" {
		t.Fatalf("%+v", res.Holdings)
	}
	back, err := ParseLines(FormatLines(hs))
	if err != nil || len(back) != 3 || back[0] != hs[0] {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "BTC", "BTC abc", "BTC -1"} {
		if _, err := ParseLines(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}
