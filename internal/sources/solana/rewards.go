package solana

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"time"

	"crypto-portfolio/internal/core"
)

// Native staking rewards.
//
// Inflation rewards are credited to stake accounts at epoch boundaries without a transaction, so
// the signature scan never sees them. After the signature scan, Activity asks getInflationReward
// about the owner's stake accounts (those whose withdraw authority is the owner), one completed
// epoch per request, and emits every non-zero reward as a TxReward.
//
// Cursors (activityState): every epoch from RewardOldest to RewardNewest has been fetched, or
// skipped because the RPC refused it for good. A call first fetches the epochs completed since
// RewardNewest, oldest first, then backfills below RewardOldest until an epoch's rewards were
// credited before Since or the RPC refuses older epochs (RewardDone). The first call starts at
// the newest completed epoch. Epoch 0 is the unset value and is never fetched (it predates
// inflation on mainnet).
//
// A reward is credited at the start of the next epoch. Its time is the block time of the first
// account's effectiveSlot, shared by all accounts of that epoch: with partitioned rewards the
// accounts are credited within minutes of each other, and one block time keeps an epoch at two
// requests at most. An epoch in which no account earned anything is dated, when backfill needs
// it for Since, by the block time of the next epoch's first slot instead; otherwise accounts
// that started staking after Since would walk every empty epoch down to the floor.

// rewardsRefusedWarning is logged when the RPC refuses older epochs (public RPCs keep a short
// ledger). ActivityResult has no warnings, so this goes to the log.
const rewardsRefusedWarning = "Staking rewards unavailable on this RPC for older epochs"

// inflationReward is one non-null entry of a getInflationReward result.
type inflationReward struct {
	EffectiveSlot uint64 `json:"effectiveSlot"`
	Amount        uint64 `json:"amount"`
}

// epochInfo is the part of getEpochInfo we read.
type epochInfo struct {
	Epoch        uint64 `json:"epoch"`
	AbsoluteSlot uint64 `json:"absoluteSlot"`
	SlotIndex    uint64 `json:"slotIndex"`
	SlotsInEpoch uint64 `json:"slotsInEpoch"`
}

// startSlot is the first slot of epoch e (at most the current epoch), counting back whole epochs
// of SlotsInEpoch slots from the current one. That holds on mainnet, which has no warm-up epochs.
// ok is false when the numbers do not add up.
func (i epochInfo) startSlot(e uint64) (slot uint64, ok bool) {
	if e > i.Epoch || i.SlotsInEpoch == 0 || i.SlotIndex > i.AbsoluteSlot {
		return 0, false
	}
	cur, back := i.AbsoluteSlot-i.SlotIndex, i.Epoch-e
	if back > cur/i.SlotsInEpoch {
		return 0, false
	}
	return cur - back*i.SlotsInEpoch, true
}

type epochEnd int

const (
	epochFetched epochEnd = iota // every entry handled (possibly none)
	epochStopped                 // out of budget, cancelled or a transport failure (recorded by fail)
	epochRetry                   // the node is not ready for it now: try again on a later call
	epochRefused                 // the node cannot serve this epoch
)

// rewards fetches staking rewards within the budget left after the signature scan.
func (r *activityRun) rewards(ctx context.Context) []core.Tx {
	if !r.s.cfg.IncludeStake || !r.take() {
		return nil
	}
	_, keys, err := r.s.stakeAccounts(ctx)
	if err != nil {
		// Most public RPCs refuse getProgramAccounts. Fetch already warns about it, and it must not
		// turn into an activity error on every call. A cancellation is reported by finish.
		return nil
	}
	r.stakeKnown, r.hasStake = true, len(keys) > 0
	if len(keys) == 0 || !r.take() {
		return nil
	}
	var info epochInfo
	if err := r.s.rpc.call(ctx, "getEpochInfo", nil, &info); err != nil {
		r.fail(fmt.Errorf("getEpochInfo: %w", err))
		return nil
	}
	if info.Epoch < 2 {
		return nil // no completed epoch past 0
	}
	top := info.Epoch - 1
	st := r.st
	var txs []core.Tx

	// New epochs, oldest first, so RewardNewest only moves over handled epochs. Running out of
	// budget or a "not now" answer stops the scan until the next call, and backfill waits until it
	// gets through. A refusal is retried only for the epoch that just ended (its rewards may still
	// be being paid out); an older refused epoch is skipped rather than blocking every later one.
	from := st.RewardNewest + 1
	if st.RewardNewest == 0 {
		from = top
	}
	for e := from; e <= top; e++ {
		got, _, end, err := r.rewardEpoch(ctx, keys, e)
		switch {
		case end == epochStopped:
			return txs
		case end == epochRetry:
			slog.Info("Staking rewards not available right now; retrying on the next sync", "source", r.s.cfg.ID, "epoch", e, "err", err)
			return txs
		case end == epochRefused && e == top:
			slog.Warn("Staking rewards for the latest epoch unavailable; retrying on the next sync", "source", r.s.cfg.ID, "epoch", e, "err", err)
			return txs
		case end == epochRefused:
			slog.Warn("Staking rewards unavailable on this RPC for an epoch; skipped", "source", r.s.cfg.ID, "epoch", e, "err", err)
		default:
			txs = append(txs, got...)
		}
		st.RewardNewest = e
		if st.RewardOldest == 0 {
			st.RewardOldest = e
		}
	}

	// Backfill, newest first.
	for st.RewardOldest > 0 && !st.RewardDone {
		if st.RewardOldest == 1 {
			st.RewardDone = true
			break
		}
		e := st.RewardOldest - 1
		got, at, end, err := r.rewardEpoch(ctx, keys, e)
		switch end {
		case epochStopped:
			return txs
		case epochRetry:
			slog.Info("Staking rewards not available right now; retrying on the next sync", "source", r.s.cfg.ID, "epoch", e, "err", err)
			return txs
		case epochRefused:
			slog.Warn(rewardsRefusedWarning, "source", r.s.cfg.ID, "epoch", e, "err", err)
			st.RewardDone = true
			return txs
		}
		if at == 0 && r.since > 0 {
			// Nobody earned anything in e: date it by the start of e+1, when its rewards would have
			// been credited. rewardEpoch kept a request free for this.
			if slot, ok := info.startSlot(e + 1); ok {
				at = r.slotTime(ctx, slot)
			}
		}
		if r.since > 0 && at != 0 && at < r.since {
			st.RewardDone = true // credited before Since: this epoch and everything older is out of range
			break
		}
		txs = append(txs, got...)
		st.RewardOldest = e
	}
	return txs
}

// rewardEpoch fetches one epoch for keys: getInflationReward, then getBlockTime of the first
// non-null entry. at is that block time (0 when no account has an entry). An epoch is only
// started when two requests fit in the budget, so running out never costs a refetch; when the
// second is not needed for the reward time it is left for dating the epoch.
func (r *activityRun) rewardEpoch(ctx context.Context, keys []string, epoch uint64) (txs []core.Tx, at int64, end epochEnd, err error) {
	if r.left-r.floor < 2 || !r.take() {
		return nil, 0, epochStopped, nil
	}
	var entries []*inflationReward
	if err := r.s.rpc.call(ctx, "getInflationReward", []any{keys, map[string]any{"epoch": epoch}}, &entries); err != nil {
		return r.rewardFailure(fmt.Errorf("getInflationReward epoch %d: %w", epoch, err))
	}
	if len(entries) != len(keys) {
		r.fail(fmt.Errorf("getInflationReward epoch %d: %d entries for %d accounts", epoch, len(entries), len(keys)))
		return nil, 0, epochStopped, nil
	}
	first := slices.IndexFunc(entries, func(e *inflationReward) bool { return e != nil })
	if first < 0 {
		r.steps++
		return nil, 0, epochFetched, nil
	}
	if !r.take() {
		return nil, 0, epochStopped, nil
	}
	slot := entries[first].EffectiveSlot
	var bt *int64
	if err := r.s.rpc.call(ctx, "getBlockTime", []any{slot}, &bt); err != nil {
		return r.rewardFailure(fmt.Errorf("getBlockTime slot %d: %w", slot, err))
	}
	if bt == nil {
		return nil, 0, epochRefused, fmt.Errorf("getBlockTime slot %d: no block time", slot)
	}
	for i, e := range entries {
		if e != nil && e.Amount > 0 {
			txs = append(txs, r.rewardTx(keys[i], epoch, e.Amount, *bt))
		}
	}
	r.steps++
	return txs, *bt, epochFetched, nil
}

// slotTime is the block time of slot, best effort: 0 when the slot was skipped, the node has no
// time for it, or the budget is gone, so an undated epoch just lets the walk go on to the next
// one. A transport failure ends the run, as for every other request.
func (r *activityRun) slotTime(ctx context.Context, slot uint64) int64 {
	if !r.take() {
		return 0
	}
	var bt *int64
	if err := r.s.rpc.call(ctx, "getBlockTime", []any{slot}, &bt); err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			r.fail(fmt.Errorf("getBlockTime slot %d: %w", slot, err))
		}
		return 0
	}
	if bt == nil {
		return 0
	}
	return *bt
}

// rewardFailure sorts a failed reward request: anything but a JSON-RPC error object ends the run
// (like a failed signature request); error objects say whether the epoch is worth asking again.
func (r *activityRun) rewardFailure(err error) ([]core.Tx, int64, epochEnd, error) {
	var re *rpcError
	switch {
	case !errors.As(err, &re):
		r.fail(err)
		return nil, 0, epochStopped, err
	case rewardRetryable(re.Code):
		return nil, 0, epochRetry, err
	}
	return nil, 0, epochRefused, err
}

// rewardRetryable recognises the JSON-RPC errors that mean "not now" rather than "never": node
// unhealthy (-32005), block status not available yet (-32014), minimum context slot not reached
// (-32016), epoch rewards still being paid out (-32017), long-term storage unreachable (-32019).
func rewardRetryable(code int) bool {
	switch code {
	case -32005, -32014, -32016, -32017, -32019:
		return true
	}
	return false
}

// rewardsPending reports whether older reward epochs are still to be fetched. When this call did
// not list the stake accounts, a backfill already under way counts as pending.
func (r *activityRun) rewardsPending() bool {
	if !r.s.cfg.IncludeStake || r.st.RewardDone {
		return false
	}
	if r.stakeKnown {
		return r.hasStake
	}
	return r.st.RewardNewest > 0
}

func (r *activityRun) rewardTx(stake string, epoch, lamports uint64, at int64) core.Tx {
	ext := "reward:" + stake + ":" + strconv.FormatUint(epoch, 10)
	return core.Tx{
		ID:         core.TxID(r.s.ID(), ext),
		SourceID:   r.s.ID(),
		ExternalID: ext,
		Time:       time.Unix(at, 0).UTC(),
		Kind:       core.TxReward,
		Status:     core.TxConfirmed,
		Legs:       []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: legAmount(new(big.Int).SetUint64(lamports), lamportsDecimals)}},
		Note:       fmt.Sprintf("Staking reward (epoch %d)", epoch),
	}
}
