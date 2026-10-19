package solana

import (
	"context"
	"testing"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
)

// TestPublicRPCIsPaced: the public RPC gets a pace under its limits (100 requests per 10 s per
// IP, 40 per method); a private RPC (an API key's) is left alone.
func TestPublicRPCIsPaced(t *testing.T) {
	s, err := New(Config{ID: "w", Address: testOwner}, nil, NewMemoryMetaCache())
	if err != nil {
		t.Fatal(err)
	}
	if got := httpx.Pace("api.mainnet-beta.solana.com"); got != publicRPCPace {
		t.Errorf("host pace = %v, want %v", got, publicRPCPace)
	}
	if s.rpc.methodPace != publicRPCMethodPace {
		t.Errorf("method pace = %v, want %v", s.rpc.methodPace, publicRPCMethodPace)
	}
	p, err := New(Config{ID: "w", Address: testOwner, RPCURL: "https://mainnet.helius-rpc.com/?api-key=x"}, nil, NewMemoryMetaCache())
	if err != nil {
		t.Fatal(err)
	}
	if p.rpc.methodPace != 0 || httpx.Pace("mainnet.helius-rpc.com") != 0 {
		t.Error("a private RPC was paced")
	}
}

// TestActivityStopsCleanlyBeforeDeadline: a call whose time is nearly up starts no more
// requests and returns what it has without an error, so the sync keeps its progress and the
// next call carries on.
func TestActivityStopsCleanlyBeforeDeadline(t *testing.T) {
	l, s := newLedger(t, testOwner)
	bob := wallet("bob")
	for i := range 5 {
		l.addSOLSend(testOwner, bob, 1000+uint64(i), int64(1_700_000_000+i*10))
	}
	ctx, cancel := context.WithTimeout(context.Background(), activityReserve/2)
	defer cancel()
	res, err := s.Activity(ctx, core.ActivityRequest{Budget: 40})
	if err != nil || res.State == nil || res.BackfillDone {
		t.Fatalf("near the deadline: err %v, state %v, done %v; want a clean, unfinished result", err, res.State != nil, res.BackfillDone)
	}
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State, Budget: 40})
	if err != nil || len(res.Txs) != 5 {
		t.Fatalf("next call: err %v, %d txs, want the 5 sends", err, len(res.Txs))
	}
}
