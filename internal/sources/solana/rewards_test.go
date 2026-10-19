package solana

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// rewardLedger is an empty wallet (no signatures) with IncludeStake on and the chain at epoch.
// The signature scan of a call costs 3 requests (two token-account listings, one signature
// page) and the reward scan's setup 2 more (stake accounts, epoch info), so a call with budget
// 5+2n can fetch n epochs that have rewards.
func rewardLedger(t *testing.T, epoch uint64, stake ...string) (*fakeLedger, *Source) {
	t.Helper()
	l, s := newLedgerWith(t, testOwner, func(c *Config) { c.IncludeStake = true })
	l.chain.epoch = epoch
	for _, pk := range stake {
		l.chain.stake = append(l.chain.stake, fakeStakeAcct{pubkey: pk, lamports: 2_000_000_000})
	}
	return l, s
}

// epochTime is when the reward for epoch e is credited in these tests: 200,000 s per epoch.
func epochTime(e uint64) int64 { return 1_700_000_000 + int64(e-690)*200_000 }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog routes the default slog logger into a buffer for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

func rewardIDs(txs []core.Tx) []string {
	var ids []string
	for _, tx := range txs {
		if tx.Kind == core.TxReward {
			ids = append(ids, tx.ExternalID)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestRewardsNewEpochs(t *testing.T) {
	l, s := rewardLedger(t, 700, "S1")
	l.chain.setReward(698, "S1", 5_000_000, 1_700_100_000)
	l.chain.setReward(699, "S1", 6_000_000, 1_700_300_000)
	since := time.Unix(1_700_000_000, 0)

	res, used := l.run(t, s, nil, since, 9)
	if used != 9 {
		t.Errorf("used %d requests, want 9", used)
	}
	if len(res.Txs) != 2 {
		t.Fatalf("got %d txs, want 2: %+v", len(res.Txs), res.Txs)
	}
	// Newest first.
	tx := res.Txs[0]
	want := core.Tx{
		ID: "phantom:reward:S1:699", SourceID: "phantom", ExternalID: "reward:S1:699",
		Time: time.Unix(1_700_300_000, 0).UTC(), Kind: core.TxReward, Status: core.TxConfirmed,
		Note: "Staking reward (epoch 699)",
	}
	if tx.ID != want.ID || tx.SourceID != want.SourceID || tx.ExternalID != want.ExternalID ||
		!tx.Time.Equal(want.Time) || tx.Time.Location() != time.UTC || tx.Kind != want.Kind ||
		tx.Status != want.Status || tx.Note != want.Note || tx.Fee != nil || tx.TxHash != "" {
		t.Errorf("tx = %+v\nwant %+v", tx, want)
	}
	if len(tx.Legs) != 1 || tx.Legs[0] != (core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: 0.006}) {
		t.Errorf("legs = %+v", tx.Legs)
	}
	old := res.Txs[1]
	if old.ExternalID != "reward:S1:698" || old.Note != "Staking reward (epoch 698)" ||
		!old.Time.Equal(time.Unix(1_700_100_000, 0)) || len(old.Legs) != 1 || old.Legs[0].Amount != 0.005 {
		t.Errorf("older tx = %+v", old)
	}
	st := decodeState(t, res.State)
	if st.RewardNewest != 699 || st.RewardOldest != 698 || st.RewardDone {
		t.Errorf("state newest=%d oldest=%d done=%v, want 699/698/false", st.RewardNewest, st.RewardOldest, st.RewardDone)
	}
	if res.BackfillDone {
		t.Error("backfill reported done while older reward epochs are still unscanned")
	}

	// Two more epochs complete: the next call fetches exactly those, before any backfill.
	l.chain.mu.Lock()
	l.chain.epoch = 702
	l.chain.mu.Unlock()
	l.chain.setReward(700, "S1", 7_000_000, 1_700_500_000)
	l.chain.setReward(701, "S1", 8_000_000, 1_700_700_000)
	res, _ = l.run(t, s, res.State, since, 9)
	if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:700", "reward:S1:701"}) {
		t.Errorf("new epochs: %v", got)
	}
	st = decodeState(t, res.State)
	if st.RewardNewest != 701 || st.RewardOldest != 698 {
		t.Errorf("state newest=%d oldest=%d, want 701/698", st.RewardNewest, st.RewardOldest)
	}
	reqs, _ := l.chain.rewardRequests()
	for e := uint64(698); e <= 701; e++ {
		if reqs[e] != 1 {
			t.Errorf("epoch %d requested %d times, want 1", e, reqs[e])
		}
	}
	if len(reqs) != 4 {
		t.Errorf("requested epochs %v, want only 698..701", reqs)
	}
}

func TestRewardsBackfillStopsAtSince(t *testing.T) {
	// Listed unsorted: the request must name the stake accounts sorted.
	l, s := rewardLedger(t, 700, "S2", "S1")
	for e := uint64(690); e <= 699; e++ {
		l.chain.setReward(e, "S1", 1_000_000*(e-689), epochTime(e))
		if e%2 == 1 {
			l.chain.setReward(e, "S2", 500_000, epochTime(e))
		}
	}
	since := time.Unix(epochTime(695)-1, 0) // 695 is inside, 694 before

	res, _ := l.run(t, s, nil, since, 40)
	want := []string{
		"reward:S1:695", "reward:S1:696", "reward:S1:697", "reward:S1:698", "reward:S1:699",
		"reward:S2:695", "reward:S2:697", "reward:S2:699",
	}
	if got := rewardIDs(res.Txs); !slices.Equal(got, want) {
		t.Errorf("txs %v\nwant %v", got, want)
	}
	for _, tx := range res.Txs {
		if tx.ExternalID == "reward:S2:697" && (!tx.Time.Equal(time.Unix(epochTime(697), 0)) || tx.Legs[0].Amount != 0.0005) {
			t.Errorf("S2 epoch 697 = %+v", tx)
		}
	}
	st := decodeState(t, res.State)
	if !st.RewardDone || st.RewardNewest != 699 || st.RewardOldest != 695 {
		t.Errorf("state newest=%d oldest=%d done=%v, want 699/695/true", st.RewardNewest, st.RewardOldest, st.RewardDone)
	}
	if !res.BackfillDone {
		t.Error("backfill not reported done")
	}
	reqs, keys := l.chain.rewardRequests()
	for e := uint64(694); e <= 699; e++ {
		if reqs[e] != 1 {
			t.Errorf("epoch %d requested %d times, want 1", e, reqs[e])
		}
	}
	if reqs[693] != 0 {
		t.Error("epoch 693 requested after Since was crossed")
	}
	for _, k := range keys {
		if !slices.Equal(k, []string{"S1", "S2"}) {
			t.Errorf("getInflationReward addresses %v, want [S1 S2]", k)
		}
	}
	if n := l.callCount("getBlockTime"); n != 6 {
		t.Errorf("getBlockTime called %d times, want once per epoch", n)
	}

	// Done means done: the next call asks for no epoch at all.
	res, _ = l.run(t, s, res.State, since, 40)
	if len(res.Txs) != 0 || l.callCount("getInflationReward") != 6 {
		t.Errorf("after done: txs=%d getInflationReward=%d", len(res.Txs), l.callCount("getInflationReward"))
	}

	// Asked to go further back: backfill reopens below the oldest epoch fetched.
	earlier := time.Unix(epochTime(692)-1, 0)
	res, _ = l.run(t, s, res.State, earlier, 40)
	want = []string{"reward:S1:692", "reward:S1:693", "reward:S1:694", "reward:S2:693"}
	if got := rewardIDs(res.Txs); !slices.Equal(got, want) {
		t.Errorf("reopened: txs %v\nwant %v", got, want)
	}
	if st := decodeState(t, res.State); !st.RewardDone || st.RewardOldest != 692 {
		t.Errorf("reopened: oldest=%d done=%v", st.RewardOldest, st.RewardDone)
	}
}

func TestRewardsRPCErrorStopsBackfill(t *testing.T) {
	const warning = "Staking rewards unavailable on this RPC for older epochs"

	t.Run("refused older epochs end backfill", func(t *testing.T) {
		logs := captureLog(t)
		l, s := rewardLedger(t, 700, "S1")
		for e := uint64(690); e <= 699; e++ {
			l.chain.setReward(e, "S1", 1_000_000, epochTime(e))
		}
		l.chain.rewardErrBelow, l.chain.rewardErrCode = 697, -32004

		res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 40})
		if err != nil {
			t.Fatalf("Activity: %v", err)
		}
		if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:697", "reward:S1:698", "reward:S1:699"}) {
			t.Errorf("txs %v", got)
		}
		st := decodeState(t, res.State)
		if !st.RewardDone || st.RewardOldest != 697 || st.RewardNewest != 699 {
			t.Errorf("state newest=%d oldest=%d done=%v", st.RewardNewest, st.RewardOldest, st.RewardDone)
		}
		if !res.BackfillDone {
			t.Error("backfill not reported done after the RPC refused older epochs")
		}
		if !strings.Contains(logs.String(), warning) {
			t.Errorf("log %q lacks the warning", logs.String())
		}
		if strings.Contains(logs.String(), testRPCKey) {
			t.Error("log leaks the RPC key")
		}

		// No retry loop: the refused epoch is not asked for again.
		res, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State, Budget: 40})
		if err != nil || len(res.Txs) != 0 {
			t.Fatalf("second call: err=%v txs=%d", err, len(res.Txs))
		}
		if reqs, _ := l.chain.rewardRequests(); reqs[696] != 1 || reqs[695] != 0 {
			t.Errorf("requests per epoch %v", reqs)
		}
	})

	t.Run("a transient error is retried, not final", func(t *testing.T) {
		logs := captureLog(t)
		l, s := rewardLedger(t, 700, "S1")
		for e := uint64(694); e <= 699; e++ {
			l.chain.setReward(e, "S1", 1_000_000, epochTime(e))
		}
		l.chain.rewardErrBelow, l.chain.rewardErrCode = 697, -32005 // node unhealthy
		since := time.Unix(epochTime(695)-1, 0)
		res, _ := l.run(t, s, nil, since, 40)
		if st := decodeState(t, res.State); st.RewardDone || st.RewardOldest != 697 {
			t.Fatalf("oldest=%d done=%v, want 697 and not done", st.RewardOldest, st.RewardDone)
		}
		if out := logs.String(); !strings.Contains(out, "level=INFO") || strings.Contains(out, "level=WARN") {
			t.Errorf("a retryable error must be logged at Info, not Warn: %q", out)
		}
		l.chain.mu.Lock()
		l.chain.rewardErrBelow = 0
		l.chain.mu.Unlock()
		res, _ = l.run(t, s, res.State, since, 40)
		if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:695", "reward:S1:696"}) {
			t.Errorf("after recovery: %v", got)
		}
		if st := decodeState(t, res.State); !st.RewardDone {
			t.Error("not done after recovery")
		}
	})

	t.Run("a refused newest epoch is retried on the next call", func(t *testing.T) {
		captureLog(t)
		l, s := rewardLedger(t, 700, "S1")
		l.chain.setReward(699, "S1", 1_000_000, epochTime(699))
		l.chain.rewardErrBelow, l.chain.rewardErrCode = 701, -32004
		res, _ := l.run(t, s, nil, time.Time{}, 40)
		if len(res.Txs) != 0 {
			t.Fatalf("txs %+v", res.Txs)
		}
		// Every epoch is refused, so nothing older is pending: backfill is done, but the refused
		// newest epoch is not counted as fetched.
		if st := decodeState(t, res.State); !st.RewardDone || st.RewardNewest != 0 || !res.BackfillDone {
			t.Fatalf("newest=%d done=%v backfillDone=%v: a refused first epoch must stay unfetched and not keep the scan pending",
				st.RewardNewest, st.RewardDone, res.BackfillDone)
		}
		l.chain.mu.Lock()
		l.chain.rewardErrBelow = 699
		l.chain.mu.Unlock()
		res, _ = l.run(t, s, res.State, time.Time{}, 40)
		if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:699"}) {
			t.Errorf("retry: %v", got)
		}
	})
}

func TestRewardsSkipWithoutStake(t *testing.T) {
	t.Run("no stake accounts", func(t *testing.T) {
		l, s := rewardLedger(t, 700)
		l.chain.setReward(699, "S1", 1_000_000, epochTime(699))
		res, used := l.run(t, s, nil, time.Time{}, 40)
		if l.chain.callCount("getProgramAccounts") != 1 {
			t.Errorf("getProgramAccounts called %d times", l.chain.callCount("getProgramAccounts"))
		}
		if n := l.callCount("getInflationReward") + l.callCount("getEpochInfo") + l.callCount("getBlockTime"); n != 0 {
			t.Errorf("%d reward requests without stake accounts", n)
		}
		if used != 4 || len(res.Txs) != 0 || !res.BackfillDone {
			t.Errorf("used=%d txs=%d done=%v", used, len(res.Txs), res.BackfillDone)
		}
		if st := decodeState(t, res.State); st.RewardNewest != 0 || st.RewardOldest != 0 || st.RewardDone {
			t.Errorf("reward state %+v", st)
		}
	})

	t.Run("IncludeStake off", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		l.chain.epoch = 700
		l.chain.stake = []fakeStakeAcct{{pubkey: "S1", lamports: 2_000_000_000}}
		l.chain.setReward(699, "S1", 1_000_000, epochTime(699))
		res, used := l.run(t, s, nil, time.Time{}, 40)
		if n := l.callCount("getProgramAccounts") + l.callCount("getInflationReward") + l.callCount("getEpochInfo"); n != 0 {
			t.Errorf("%d stake/reward requests with IncludeStake off", n)
		}
		if used != 3 || len(res.Txs) != 0 || !res.BackfillDone {
			t.Errorf("used=%d txs=%d done=%v", used, len(res.Txs), res.BackfillDone)
		}
	})

	t.Run("stake accounts unavailable on this RPC", func(t *testing.T) {
		for _, mode := range []string{"rpc", "http"} {
			l, s := rewardLedger(t, 700, "S1")
			l.chain.stakeFail = mode
			res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 40})
			if err != nil {
				t.Fatalf("%s: a refused stake listing must not fail activity: %v", mode, err)
			}
			if l.callCount("getInflationReward") != 0 || !res.BackfillDone {
				t.Errorf("%s: getInflationReward=%d done=%v", mode, l.callCount("getInflationReward"), res.BackfillDone)
			}
		}
	})
}

func TestRewardsIdempotent(t *testing.T) {
	l, s := rewardLedger(t, 700, "S1")
	for e := uint64(690); e <= 699; e++ {
		l.chain.setReward(e, "S1", 1_000_000+e, epochTime(e))
	}
	since := time.Unix(epochTime(693)-1, 0)

	// A budget that cannot finish an epoch does not start one.
	res, _ := l.run(t, s, nil, since, 6)
	if n := l.callCount("getInflationReward"); n != 0 {
		t.Fatalf("getInflationReward called %d times with no room to finish an epoch", n)
	}

	// One epoch per call.
	txs, state, calls := l.drain(t, s, res.State, since, 7)
	if calls != 8 {
		t.Errorf("drained in %d calls, want 8 (699..693, then 692 crosses Since)", calls)
	}
	assertNoDupes(t, txs)
	want := []string{"reward:S1:693", "reward:S1:694", "reward:S1:695", "reward:S1:696", "reward:S1:697", "reward:S1:698", "reward:S1:699"}
	if got := rewardIDs(txs); !slices.Equal(got, want) {
		t.Errorf("txs %v\nwant %v", got, want)
	}
	reqs, _ := l.chain.rewardRequests()
	for e, n := range reqs {
		if n != 1 {
			t.Errorf("epoch %d requested %d times", e, n)
		}
	}
	if reqs[691] != 0 {
		t.Error("epoch 691 requested")
	}

	// A new epoch: only it is fetched; with nothing new, nothing is.
	l.chain.mu.Lock()
	l.chain.epoch = 701
	l.chain.mu.Unlock()
	l.chain.setReward(700, "S1", 2_000_000, epochTime(700))
	res, _ = l.run(t, s, state, since, 7)
	if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:700"}) {
		t.Errorf("new epoch: %v", got)
	}
	before := l.callCount("getInflationReward")
	res, _ = l.run(t, s, res.State, since, 7)
	if len(res.Txs) != 0 || l.callCount("getInflationReward") != before {
		t.Errorf("idle call: txs=%d getInflationReward %d -> %d", len(res.Txs), before, l.callCount("getInflationReward"))
	}
	reqs, _ = l.chain.rewardRequests()
	for e, n := range reqs {
		if n != 1 {
			t.Errorf("epoch %d requested %d times", e, n)
		}
	}
}

func TestRewardsBackfillDatesEmptyEpochs(t *testing.T) {
	// The stake account only started earning in 697, after Since: no reward entry is older than
	// Since, so the walk through the empty epochs below must be dated by the epoch boundaries.
	for _, skip := range []bool{false, true} {
		name := "every boundary has a block time"
		if skip {
			name = "a skipped boundary slot does not stop the walk"
		}
		t.Run(name, func(t *testing.T) {
			l, s := rewardLedger(t, 700, "S1")
			for e := uint64(697); e <= 699; e++ {
				l.chain.setReward(e, "S1", 1_000_000, epochTime(e))
			}
			for e := uint64(690); e <= 696; e++ {
				l.chain.setRewardTime(e, epochTime(e))
			}
			if skip {
				l.chain.skipSlot(rewardSlot(696)) // the first slot of epoch 697
			}
			since := time.Unix(epochTime(695)-1, 0)

			res, _ := l.run(t, s, nil, since, 40)
			if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:697", "reward:S1:698", "reward:S1:699"}) {
				t.Errorf("txs %v", got)
			}
			st := decodeState(t, res.State)
			if !st.RewardDone || st.RewardOldest != 695 {
				t.Errorf("oldest=%d done=%v, want 695 and done", st.RewardOldest, st.RewardDone)
			}
			if !res.BackfillDone {
				t.Error("backfill not reported done")
			}
			reqs, _ := l.chain.rewardRequests()
			for e := uint64(694); e <= 699; e++ {
				if reqs[e] != 1 {
					t.Errorf("epoch %d requested %d times, want 1", e, reqs[e])
				}
			}
			if len(reqs) != 6 {
				t.Errorf("requested epochs %v, want only 694..699", reqs)
			}
			// Three reward times plus one boundary probe for each empty epoch 696, 695, 694.
			if n := l.callCount("getBlockTime"); n != 6 {
				t.Errorf("getBlockTime called %d times, want 6", n)
			}
		})
	}
}

func TestRewardsRefusedNewEpochIsAGap(t *testing.T) {
	logs := captureLog(t)
	l, s := rewardLedger(t, 700, "S1")
	l.chain.setReward(699, "S1", 1_000_000, epochTime(699))
	l.chain.setRewardTime(698, epochTime(698))
	since := time.Unix(epochTime(699)-1, 0)
	res, _ := l.run(t, s, nil, since, 40)
	if st := decodeState(t, res.State); st.RewardNewest != 699 || !st.RewardDone {
		t.Fatalf("setup: newest=%d done=%v", st.RewardNewest, st.RewardDone)
	}

	setEpoch := func(e uint64) {
		l.chain.mu.Lock()
		l.chain.epoch = e
		l.chain.mu.Unlock()
	}

	// Four epochs later, the node refuses 701 for good: it is skipped, not retried forever.
	setEpoch(704)
	for e := uint64(700); e <= 703; e++ {
		l.chain.setReward(e, "S1", 1_000_000, epochTime(e))
	}
	l.chain.setRewardErr(701, -32004)
	res, _ = l.run(t, s, res.State, since, 40)
	if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:700", "reward:S1:702", "reward:S1:703"}) {
		t.Errorf("txs %v", got)
	}
	if st := decodeState(t, res.State); st.RewardNewest != 703 {
		t.Errorf("newest=%d, want 703", st.RewardNewest)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "epoch=701") {
		t.Errorf("the skipped epoch is not logged as a warning: %q", out)
	}
	res, _ = l.run(t, s, res.State, since, 40)
	if reqs, _ := l.chain.rewardRequests(); reqs[701] != 1 {
		t.Errorf("epoch 701 requested %d times, want 1", reqs[701])
	}

	// The epoch that just ended is different: a refusal there is retried.
	setEpoch(705)
	l.chain.setReward(704, "S1", 1_000_000, epochTime(704))
	l.chain.setRewardErr(704, -32004)
	res, _ = l.run(t, s, res.State, since, 40)
	if st := decodeState(t, res.State); len(res.Txs) != 0 || st.RewardNewest != 703 {
		t.Fatalf("refused newest epoch: txs=%d newest=%d, want none and 703", len(res.Txs), st.RewardNewest)
	}
	l.chain.setRewardErr(704, 0)
	res, _ = l.run(t, s, res.State, since, 40)
	if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:704"}) {
		t.Errorf("retry: %v", got)
	}
}

func TestRewardsUndatedEffectiveSlotUsesNextEpochStart(t *testing.T) {
	captureLog(t)
	l, s := rewardLedger(t, 700, "S1")
	l.chain.setReward(699, "S1", 1_000_000, epochTime(699))
	since := time.Unix(epochTime(699)-1, 0)
	res, _ := l.run(t, s, nil, since, 40)

	l.chain.mu.Lock()
	l.chain.epoch = 703
	l.chain.mu.Unlock()
	for e := uint64(700); e <= 702; e++ {
		l.chain.setReward(e, "S1", 2_000_000, epochTime(e))
	}
	l.chain.undatedReward(700) // older than the latest epoch: refusing it would drop it for good
	l.chain.undatedReward(702) // the latest
	res, _ = l.run(t, s, res.State, since, 40)
	if got := rewardIDs(res.Txs); !slices.Equal(got, []string{"reward:S1:700", "reward:S1:701", "reward:S1:702"}) {
		t.Fatalf("txs %v", got)
	}
	for _, tx := range res.Txs {
		e := map[string]uint64{"reward:S1:700": 700, "reward:S1:702": 702}[tx.ExternalID]
		if e != 0 && !tx.Time.Equal(time.Unix(epochTime(e), 0)) {
			t.Errorf("%s dated %v, want the start of epoch %d (%v)", tx.ExternalID, tx.Time, e+1, time.Unix(epochTime(e), 0).UTC())
		}
	}
	if st := decodeState(t, res.State); st.RewardNewest != 702 {
		t.Errorf("newest=%d, want 702", st.RewardNewest)
	}
}

func TestRewardsRefusingEveryEpochIsDone(t *testing.T) {
	logs := captureLog(t)
	l, s := rewardLedger(t, 700, "S1")
	for e := uint64(690); e <= 699; e++ {
		l.chain.setReward(e, "S1", 1_000_000, epochTime(e))
	}
	since := time.Unix(epochTime(690)-1, 0)
	res, _ := l.run(t, s, nil, since, 9) // 699 and 698 fit; older epochs stay pending
	if st := decodeState(t, res.State); st.RewardOldest != 698 || st.RewardDone || res.BackfillDone {
		t.Fatalf("setup: oldest=%d done=%v backfillDone=%v", st.RewardOldest, st.RewardDone, res.BackfillDone)
	}

	// From now on the node refuses every epoch, the one that just ended included.
	l.chain.mu.Lock()
	l.chain.epoch = 701
	l.chain.rewardErrBelow, l.chain.rewardErrCode = 702, -32004
	l.chain.mu.Unlock()
	l.chain.setReward(700, "S1", 1_000_000, epochTime(700))
	for i := 0; i < 3; i++ {
		res, _ = l.run(t, s, res.State, since, 40)
		if !res.BackfillDone {
			t.Fatalf("call %d: backfill still pending although the RPC refuses every epoch", i)
		}
	}
	if st := decodeState(t, res.State); st.RewardNewest != 699 || !st.RewardDone {
		t.Errorf("newest=%d done=%v, want 699 (the refused latest epoch is retried) and done", st.RewardNewest, st.RewardDone)
	}
	if n := strings.Count(logs.String(), "latest epoch unavailable"); n != 1 {
		t.Errorf("the refused latest epoch was logged %d times, want once: %q", n, logs.String())
	}
}

func TestRewardsStakeListingRefusedMidScan(t *testing.T) {
	for _, c := range []struct {
		mode string
		done bool
	}{
		{"rpc", true},   // JSON-RPC error object: refused for good
		{"http", true},  // HTTP 403: refused for good
		{"down", false}, // HTTP 503: try again later
	} {
		t.Run(c.mode, func(t *testing.T) {
			captureLog(t)
			l, s := rewardLedger(t, 700, "S1")
			for e := uint64(690); e <= 699; e++ {
				l.chain.setReward(e, "S1", 1_000_000, epochTime(e))
			}
			since := time.Unix(epochTime(690)-1, 0)
			res, _ := l.run(t, s, nil, since, 9)
			if res.BackfillDone {
				t.Fatal("setup: backfill done too early")
			}
			l.chain.mu.Lock()
			l.chain.stakeFail = c.mode
			l.chain.mu.Unlock()
			res, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State, Since: since, Budget: 40})
			if err != nil {
				t.Fatalf("a failed stake listing must not fail activity: %v", err)
			}
			if res.BackfillDone != c.done {
				t.Errorf("backfillDone=%v, want %v", res.BackfillDone, c.done)
			}
		})
	}
}

func TestEpochInfoStartSlot(t *testing.T) {
	info := epochInfo{Epoch: 700, AbsoluteSlot: 700*432_000 + 1000, SlotIndex: 1000, SlotsInEpoch: 432_000}
	for _, c := range []struct {
		info epochInfo
		e    uint64
		want uint64
		ok   bool
	}{
		{info, 700, 700 * 432_000, true},
		{info, 695, 695 * 432_000, true},
		{info, 0, 0, true},
		{info, 701, 0, false}, // the future
		{epochInfo{Epoch: 700, AbsoluteSlot: 1000, SlotIndex: 1000, SlotsInEpoch: 432_000}, 699, 0, false}, // would go below slot 0
		{epochInfo{Epoch: 700, AbsoluteSlot: 1000, SlotIndex: 2000, SlotsInEpoch: 432_000}, 700, 0, false},
		{epochInfo{Epoch: 700, AbsoluteSlot: 1000, SlotIndex: 0}, 699, 0, false},
	} {
		got, ok := c.info.startSlot(c.e)
		if got != c.want || ok != c.ok {
			t.Errorf("%+v.startSlot(%d) = %d, %v; want %d, %v", c.info, c.e, got, ok, c.want, c.ok)
		}
	}
}
