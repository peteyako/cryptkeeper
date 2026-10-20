package solana

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/sources/solana/defi"
)

// Activity is incremental: every call spends at most req.Budget RPC requests and leaves cursors
// that resume exactly where it stopped.
//
// What is watched. A wallet's history is not one signature list. A transfer *into* an existing
// token account never references the owner, so it is missing from getSignaturesForAddress(owner).
// Activity therefore watches the owner AND each of its token accounts (listed once per call,
// both token programs) and merges the lists, fetching every transaction at most once.
//
// Cursors. For each watched address the chain of signatures (newest to oldest) is covered by:
//
//   - Newest: the newest signature handled. Everything newer is "new" and found with `until`.
//   - Oldest: backfill cursor; everything older is fetched with `before`, down to req.Since.
//   - Gaps: ranges between handled signatures that a call had to leave unfinished (budget ran
//     out while working through a long run of new signatures). Each is {Before, Until}: the
//     unhandled signatures older than Before and newer than Until.
//
// Signatures are always handled newest first, and a cursor only ever moves to a signature that
// has been handled, so stopping at any point loses nothing: the next call lists from the cursor
// and continues. Order of work per call: new signatures of every account (least recently checked
// account first, so a wallet with many token accounts is covered round-robin), then the gaps,
// then backfill. While gaps or backfill are pending, checking for new signatures is held to half
// of the remaining budget, so a large number of token accounts cannot starve them.
//
// A signature reached through several accounts is fetched once; a bounded set of recently
// handled signatures carries that across calls. If a lagging account meets a signature already
// evicted from that set the transaction is emitted again with the same ID, which consumers must
// treat as an upsert.
//
// Staking rewards have no transaction. With IncludeStake, whatever budget the signatures leave
// goes to a reward scan per epoch with its own cursors (rewards.go).

var _ core.ActivitySource = (*Source)(nil)

const (
	defaultActivityBudget = 40
	// activityReserve: a call stops starting requests this close to its deadline, so a slow
	// RPC ends it cleanly instead of cutting it off.
	activityReserve = 10 * time.Second
	// maxSeenSigs bounds the dedupe set carried in the state (~95 bytes per signature).
	maxSeenSigs = 1000
	// maxWatchedTokenAccts bounds how many token accounts are watched. Wallets drowning in spam
	// airdrops can own thousands; accounts holding a balance are watched first.
	maxWatchedTokenAccts = 300
	// maxGaps bounds the unfinished ranges kept per account; beyond it they are merged into one
	// (re-listing a few handled signatures, which the dedupe set then skips).
	maxGaps = 16
	// notReadyWindow is how long a listed signature whose transaction the node cannot serve yet
	// is retried before it is given up on (pruned history).
	notReadyWindow = 10 * time.Minute

	activityStateVersion = 1
	solscanTxURL         = "https://solscan.io/tx/"
	systemProgramID      = "11111111111111111111111111111111"
	// solAssetKey is native SOL's asset key and symbol on activity legs.
	solAssetKey = "SOL"
)

var (
	// sigPageLimit is the page size of getSignaturesForAddress (the RPC maximum is 1000).
	// A variable so tests can use small pages.
	sigPageLimit = 100
	// activityNow is the clock for the not-ready window; replaced in tests.
	activityNow = time.Now
)

// activityState is the opaque state handed back and forth through core.ActivityRequest.State.
type activityState struct {
	V     int                    `json:"v"`
	Owner string                 `json:"owner"`
	Vault string                 `json:"vault,omitempty"` // the Jupiter vault watched with the owner
	Since int64                  `json:"since,omitempty"` // earliest unix time backfilled to (0 = whole history)
	Seq   uint64                 `json:"seq"`             // call counter, for round-robin
	Accts map[string]*acctCursor `json:"accts"`
	Seen  []string               `json:"seen,omitempty"` // recently handled signatures, oldest first

	// Staking-reward cursors (see rewards.go). 0 = no epoch fetched yet.
	RewardNewest uint64 `json:"rewardNewest,omitempty"` // newest epoch fetched
	RewardOldest uint64 `json:"rewardOldest,omitempty"` // oldest epoch fetched
	RewardDone   bool   `json:"rewardDone,omitempty"`   // backfill reached Since, or the RPC refused older epochs
}

type acctCursor struct {
	Newest string `json:"newest,omitempty"`
	Oldest string `json:"oldest,omitempty"`
	Done   bool   `json:"done,omitempty"` // backfill reached req.Since or the start of history
	Gaps   []gap  `json:"gaps,omitempty"` // unfinished ranges, newest first
	Chk    uint64 `json:"chk,omitempty"`  // Seq of the last new-signature check
}

// gap is a range of signatures still to be handled: older than Before, newer than Until.
// Before is always a handled signature; Until == "" means "all the way back".
type gap struct {
	Before string `json:"before"`
	Until  string `json:"until"`
}

// loadActivityState decodes req.State. Anything unusable (first call, corrupt, another wallet's,
// a different version) starts over: re-reading history is safe because tx IDs are stable.
func (s *Source) loadActivityState(raw []byte) (st *activityState, fresh bool) {
	if len(raw) > 0 {
		var got activityState
		if json.Unmarshal(raw, &got) == nil && got.V == activityStateVersion && got.Owner == s.cfg.Address && got.Vault == s.cfg.JupiterVault && got.Accts != nil {
			for k, a := range got.Accts {
				if a == nil {
					delete(got.Accts, k)
				}
			}
			return &got, false
		}
	}
	return &activityState{V: activityStateVersion, Owner: s.cfg.Address, Vault: s.cfg.JupiterVault, Accts: map[string]*acctCursor{}}, true
}

func (st *activityState) cursor(addr string) *acctCursor {
	a := st.Accts[addr]
	if a == nil {
		a = &acctCursor{}
		st.Accts[addr] = a
	}
	return a
}

// sigEntry is one element of a getSignaturesForAddress result.
type sigEntry struct {
	Signature string          `json:"signature"`
	Slot      int64           `json:"slot"`
	BlockTime *int64          `json:"blockTime"`
	Err       json.RawMessage `json:"err"`
}

// Activity lists transactions for the owner address. See the package comment above for the
// cursor design. Errors: a failure with no progress at all is returned; once a call made
// progress, a later RPC failure only ends it early (the next call hits the same failure first
// and reports it). A cancelled context always returns its error together with the consistent
// partial result.
func (s *Source) Activity(ctx context.Context, req core.ActivityRequest) (core.ActivityResult, error) {
	budget := req.Budget
	if budget <= 0 {
		budget = defaultActivityBudget
	}
	st, fresh := s.loadActivityState(req.State)
	st.Seq++
	var since int64
	if !req.Since.IsZero() {
		since = req.Since.Unix()
	}
	switch {
	case fresh:
		st.Since = since
	case since < st.Since:
		// Asked to go further back than before: reopen backfill from where each account stopped.
		for _, a := range st.Accts {
			a.Done = false
		}
		st.RewardDone = false
		st.Since = since
	}

	r := &activityRun{s: s, ctx: ctx, st: st, since: since, left: budget, seen: make(map[string]bool, len(st.Seen))}
	for _, sig := range st.Seen {
		r.seen[sig] = true
	}

	watched, listed := r.discover()
	if listed {
		// Forget accounts that are gone (closed) once nothing is left to fetch for them.
		for addr, a := range st.Accts {
			if !watched[addr] && a.Done && len(a.Gaps) == 0 {
				delete(st.Accts, addr)
			}
		}
	}

	// New signatures first, then unfinished gaps, then backfill. While older work is pending,
	// checking for new signatures may use at most half of what is left, so a wallet with more
	// token accounts than the budget covers cannot starve it.
	for _, a := range st.Accts {
		if !a.Done || len(a.Gaps) > 0 {
			r.floor = (r.left + 1) / 2
			break
		}
	}
	byRecency := r.byRecency()
	for _, addr := range byRecency {
		if !r.canSpend() {
			break
		}
		r.topScan(addr, st.Accts[addr])
	}
	r.floor = 0
	for _, addr := range byRecency {
		if !r.canSpend() {
			break
		}
		r.fillGaps(addr, st.Accts[addr])
	}
	for _, addr := range r.byPriority() {
		if !r.canSpend() {
			break
		}
		r.backfill(addr, st.Accts[addr])
	}
	r.txs = append(r.txs, r.rewards(ctx)...)
	return r.finish(listed)
}

// activityRun is the working state of one Activity call.
type activityRun struct {
	s     *Source
	ctx   context.Context
	st    *activityState
	since int64

	left  int // RPC requests left in the budget
	floor int // requests that stay in reserve (new-signature work leaves them for backfill)

	seen     map[string]bool // handled signatures: carried over from the state plus this call's
	newSigs  []string        // handled this call, in order
	txs      []core.Tx
	steps    int // signatures handled and reward epochs fetched (progress)
	err      error
	stopped  bool // transport-level failure or cancellation: nothing more can be done
	notReady bool // a listed transaction could not be fetched yet

	stakeKnown   bool // the stake accounts were listed this call
	hasStake     bool // ... and there is at least one
	stakeRefused bool // the RPC refused to list them for good
}

func (r *activityRun) canSpend() bool { return !r.stopped && r.left > r.floor }

// take spends one RPC request; false means the budget (above the reserve) is gone, the run
// has stopped, or the call's time is nearly up: then it ends like an exhausted budget, with
// its progress and no error, rather than running into its deadline.
func (r *activityRun) take() bool {
	if !r.canSpend() || r.ctx.Err() != nil {
		return false
	}
	if dl, ok := r.ctx.Deadline(); ok && time.Until(dl) < activityReserve {
		return false
	}
	r.left--
	return true
}

// fail records an RPC failure. JSON-RPC error objects are definitive answers about one request
// and only end that scan; anything else (HTTP 429, outage, timeout) ends the whole run.
func (r *activityRun) fail(err error) {
	if cerr := r.ctx.Err(); cerr != nil {
		r.err, r.stopped = cerr, true
		return
	}
	if r.err == nil {
		r.err = err
	}
	var re *rpcError
	if !errors.As(err, &re) {
		r.stopped = true
	}
}

// discover registers the owner (and the Jupiter vault) and their token accounts. Listing them
// costs two requests per owner (one per token program) and happens once per call. If it cannot
// be done the accounts known from earlier calls are still worked on, but backfill is not
// reported complete.
func (r *activityRun) discover() (watched map[string]bool, listed bool) {
	owners := r.s.owners()
	watched = map[string]bool{}
	for _, o := range owners {
		watched[o] = true
		r.st.cursor(o)
	}
	known := func() (map[string]bool, bool) {
		for addr := range r.st.Accts {
			watched[addr] = true
		}
		return watched, false
	}
	if r.left < 2*len(owners) || r.ctx.Err() != nil {
		return known()
	}
	var refs []tokenAcctRef
	for _, o := range owners {
		r.left -= 2
		ts, err := r.s.tokenAccountsOf(r.ctx, o)
		if err != nil {
			r.fail(err)
			return known()
		}
		refs = append(refs, ts.addrs...)
	}
	slices.SortStableFunc(refs, func(a, b tokenAcctRef) int {
		return cmp.Compare(boolInt(a.Empty), boolInt(b.Empty)) // holding accounts first
	})
	for _, ref := range refs {
		if len(watched)-len(owners) >= maxWatchedTokenAccts {
			break
		}
		if ref.Pubkey == "" || watched[ref.Pubkey] {
			continue
		}
		watched[ref.Pubkey] = true
		r.st.cursor(ref.Pubkey)
	}
	return watched, true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// byRecency orders accounts for new-signature checks: the owner always first, then the token
// accounts least recently checked.
func (r *activityRun) byRecency() []string {
	owner := r.s.cfg.Address
	addrs := slices.Collect(maps.Keys(r.st.Accts))
	slices.SortFunc(addrs, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(boolInt(b == owner), boolInt(a == owner)),
			cmp.Compare(r.st.Accts[a].Chk, r.st.Accts[b].Chk),
			cmp.Compare(a, b),
		)
	})
	return addrs
}

// byPriority orders backfill: the owner first, then the token accounts by address.
func (r *activityRun) byPriority() []string {
	owner := r.s.cfg.Address
	addrs := r.byRecency()
	slices.SortStableFunc(addrs, func(a, b string) int {
		return cmp.Or(cmp.Compare(boolInt(b == owner), boolInt(a == owner)), cmp.Compare(a, b))
	})
	return addrs
}

// topScan handles the signatures newer than the account's Newest. Whatever it cannot finish
// becomes a gap, so the next call resumes below the last handled signature.
func (r *activityRun) topScan(addr string, a *acctCursor) {
	if a.Newest == "" && !a.Done {
		return // never scanned: backfill starts from the top
	}
	a.Chk = r.st.Seq
	until, cursor := a.Newest, ""
	end := r.scanRange(addr, "", until, func(e sigEntry) {
		if cursor == "" {
			a.Newest = e.Signature
		}
		cursor = e.Signature
	})
	if end == scanStopped && cursor != "" {
		a.Gaps = append([]gap{{Before: cursor, Until: until}}, a.Gaps...)
		if len(a.Gaps) > maxGaps {
			a.Gaps = []gap{{Before: a.Gaps[0].Before, Until: a.Gaps[len(a.Gaps)-1].Until}}
		}
	}
}

// fillGaps continues unfinished ranges, newest first.
func (r *activityRun) fillGaps(addr string, a *acctCursor) {
	for len(a.Gaps) > 0 && r.canSpend() {
		g := &a.Gaps[0]
		if r.scanRange(addr, g.Before, g.Until, func(e sigEntry) { g.Before = e.Signature }) != scanComplete {
			return
		}
		a.Gaps = a.Gaps[1:]
	}
}

// backfill walks the account's history from Oldest downwards until req.Since or the start.
func (r *activityRun) backfill(addr string, a *acctCursor) {
	if a.Done {
		return
	}
	end := r.scanRange(addr, a.Oldest, "", func(e sigEntry) {
		if a.Newest == "" {
			a.Newest = e.Signature
		}
		a.Oldest = e.Signature
	})
	if end == scanComplete {
		a.Done = true
	}
}

type scanEnd int

const (
	// scanComplete: the whole range is handled (start of history, the `until` bound, or a
	// signature older than Since was reached).
	scanComplete scanEnd = iota
	// scanStopped: out of budget, or an error / unavailable transaction. The caller's cursor
	// (advanced via the callback) says where to resume.
	scanStopped
)

// scanRange handles the signatures of acct older than `before` ("" = from the newest) and newer
// than `until` ("" = unbounded), newest first, one page of RPC at a time. advance is called
// after each signature is handled; cursors must only move there.
func (r *activityRun) scanRange(acct, before, until string, advance func(sigEntry)) scanEnd {
	cursor := before
	for {
		if !r.take() {
			return scanStopped
		}
		page, err := r.listSignatures(acct, cursor, until)
		if err != nil {
			r.fail(fmt.Errorf("getSignaturesForAddress: %w", err))
			return scanStopped
		}
		for _, e := range page {
			if e.Signature == "" {
				continue
			}
			if r.since > 0 && e.BlockTime != nil && *e.BlockTime < r.since {
				return scanComplete // newest first, so everything after is older still
			}
			if !r.handle(e) {
				return scanStopped
			}
			r.steps++
			cursor = e.Signature
			advance(e)
		}
		if len(page) < sigPageLimit {
			return scanComplete
		}
	}
}

func (r *activityRun) listSignatures(acct, before, until string) ([]sigEntry, error) {
	opts := map[string]any{"limit": sigPageLimit, "commitment": "confirmed"}
	if before != "" {
		opts["before"] = before
	}
	if until != "" {
		opts["until"] = until
	}
	var out []sigEntry
	if err := r.s.rpc.call(r.ctx, "getSignaturesForAddress", []any{acct, opts}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// handle fetches and classifies one signature unless it was handled already (through another
// account). It reports false when scanning must stop before this signature: no budget, an RPC
// failure, or a transaction the node cannot serve yet.
func (r *activityRun) handle(e sigEntry) bool {
	if r.seen[e.Signature] {
		return true
	}
	if !r.take() {
		return false
	}
	var tx *rpcTx
	params := []any{e.Signature, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}
	if err := r.s.rpc.call(r.ctx, "getTransaction", params, &tx); err != nil {
		if !txUnavailable(err) {
			r.fail(fmt.Errorf("getTransaction: %w", err))
			return false
		}
		tx = nil
	}
	switch {
	case tx != nil:
		if t, ok := r.s.classify(e, tx); ok {
			r.txs = append(r.txs, t)
		}
	case e.BlockTime != nil && activityNow().Sub(time.Unix(*e.BlockTime, 0)) <= notReadyWindow:
		// Listed but not served yet (a lagging node behind a load balancer): retry next call.
		r.notReady = true
		return false
	default:
		// Old enough that the node will never serve it (pruned): skip it for good.
	}
	r.seen[e.Signature] = true
	r.newSigs = append(r.newSigs, e.Signature)
	return true
}

// txUnavailable recognises the JSON-RPC errors a node answers for a transaction it no longer
// (or never) has: block cleaned up, block not available, slot skipped, missing in long-term
// storage.
func txUnavailable(err error) bool {
	var re *rpcError
	if !errors.As(err, &re) {
		return false
	}
	switch re.Code {
	case -32001, -32004, -32007, -32009:
		return true
	}
	return false
}

func (r *activityRun) finish(listed bool) (core.ActivityResult, error) {
	r.st.Seen = append(r.st.Seen, r.newSigs...)
	if over := len(r.st.Seen) - maxSeenSigs; over > 0 {
		r.st.Seen = slices.Clone(r.st.Seen[over:])
	}
	done := listed && !r.notReady && !r.rewardsPending()
	for _, a := range r.st.Accts {
		if !a.Done || len(a.Gaps) > 0 {
			done = false
		}
	}
	slices.SortStableFunc(r.txs, func(a, b core.Tx) int {
		return cmp.Or(b.Time.Compare(a.Time), cmp.Compare(a.ID, b.ID))
	})
	state, err := json.Marshal(r.st)
	if err != nil {
		return core.ActivityResult{}, fmt.Errorf("solana activity state: %w", err)
	}
	res := core.ActivityResult{Txs: r.txs, State: state, BackfillDone: done}
	if cerr := r.ctx.Err(); cerr != nil {
		r.err = cerr
	}
	if r.err != nil && (r.ctx.Err() != nil || r.steps == 0) {
		return res, r.err
	}
	return res, nil
}

// --- parsed transactions -----------------------------------------------------------------

// rpcTx is the part of a jsonParsed getTransaction result we read.
type rpcTx struct {
	BlockTime   *int64 `json:"blockTime"`
	Transaction struct {
		Message struct {
			AccountKeys  []acctKey       `json:"accountKeys"`
			Instructions []txInstruction `json:"instructions"`
		} `json:"message"`
	} `json:"transaction"`
	Meta *struct {
		Err               json.RawMessage `json:"err"`
		Fee               uint64          `json:"fee"`
		PreBalances       []uint64        `json:"preBalances"`
		PostBalances      []uint64        `json:"postBalances"`
		PreTokenBalances  []tokenBalance  `json:"preTokenBalances"`
		PostTokenBalances []tokenBalance  `json:"postTokenBalances"`
		InnerInstructions []struct {
			Instructions []txInstruction `json:"instructions"`
		} `json:"innerInstructions"`
	} `json:"meta"`
}

// acctKey is an entry of message.accountKeys: an object with "pubkey" and "signer" in
// jsonParsed, a bare string in the other encodings.
type acctKey struct {
	Pubkey string
	Signer bool
}

func (k *acctKey) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &k.Pubkey)
	}
	var o struct {
		Pubkey string `json:"pubkey"`
		Signer bool   `json:"signer"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	k.Pubkey, k.Signer = o.Pubkey, o.Signer
	return nil
}

type txInstruction struct {
	ProgramID string          `json:"programId"`
	Parsed    json.RawMessage `json:"parsed"` // an object for parsed programs, a string for e.g. memo
}

type parsedInstruction struct {
	Type string `json:"type"`
	Info struct {
		Source            string `json:"source"`
		Destination       string `json:"destination"`
		Authority         string `json:"authority"`
		MultisigAuthority string `json:"multisigAuthority"`
	} `json:"info"`
}

func (i txInstruction) parse() (parsedInstruction, bool) {
	var p parsedInstruction
	if len(i.Parsed) == 0 || i.Parsed[0] != '{' || json.Unmarshal(i.Parsed, &p) != nil {
		return p, false
	}
	return p, true
}

type tokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UITokenAmount struct {
		Amount         string `json:"amount"`
		Decimals       int    `json:"decimals"`
		UIAmountString string `json:"uiAmountString"`
	} `json:"uiTokenAmount"`
}

// raw is the balance in base units. The integer "amount" is exact and always present; the
// decimal uiAmountString is the fallback for a node that omits it.
func (b tokenBalance) raw() (*big.Int, bool) {
	if n, ok := new(big.Int).SetString(b.UITokenAmount.Amount, 10); ok {
		return n, true
	}
	return scaleDecimal(b.UITokenAmount.UIAmountString, b.UITokenAmount.Decimals)
}

// scaleDecimal converts a plain decimal string ("12.5") to base units for the given decimals.
func scaleDecimal(s string, decimals int) (*big.Int, bool) {
	if decimals < 0 || decimals > 36 || s == "" {
		return nil, false
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals || intPart == "" && frac == "" {
		return nil, false
	}
	digits := intPart + frac + strings.Repeat("0", decimals-len(frac))
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok || strings.ContainsAny(digits, "+-_") {
		return nil, false
	}
	if neg {
		n.Neg(n)
	}
	return n, true
}

// legAmount converts base units to a float via the exact decimal string, so 50.7 stays 50.7.
func legAmount(n *big.Int, decimals int) float64 {
	f, _ := strconv.ParseFloat(formatAmount(n, decimals), 64)
	return f
}

// classify turns a fetched transaction into an entry for the owner wallet. ok is false when
// nothing moved for the owner (no balance change and not the fee payer): such a transaction only
// references one of the owner's accounts and is noise.
func (s *Source) classify(e sigEntry, tx *rpcTx) (core.Tx, bool) {
	m := tx.Meta
	if m == nil {
		return core.Tx{}, false
	}
	owners := s.ownerSet()
	keys := tx.Transaction.Message.AccountKeys

	// Native SOL: the owner's (and the vault's) own balance change, with the fee split out
	// when one of them paid it.
	lamports := new(big.Int)
	feePayer := false
	for i, k := range keys {
		if !owners[k.Pubkey] {
			continue
		}
		if i < len(m.PreBalances) && i < len(m.PostBalances) {
			lamports.Add(lamports, new(big.Int).Sub(new(big.Int).SetUint64(m.PostBalances[i]), new(big.Int).SetUint64(m.PreBalances[i])))
		}
		feePayer = feePayer || i == 0
	}
	var fee *core.Leg
	if feePayer && m.Fee > 0 {
		feeBig := new(big.Int).SetUint64(m.Fee)
		lamports.Add(lamports, feeBig) // the fee is reported separately, not inside the SOL leg
		fee = &core.Leg{AssetKey: solAssetKey, Symbol: solAssetKey, Amount: -legAmount(feeBig, lamportsDecimals)}
	}

	// SPL tokens: sum the owner's token accounts per mint, after minus before (a missing "before"
	// is a new account, a missing "after" a closed one). wSOL is SOL.
	type tokDelta struct {
		amt *big.Int
		dec int
	}
	deltas := map[string]*tokDelta{}
	tokenOwner := map[string]string{} // token account -> owner, to name counterparties
	add := func(list []tokenBalance, sign int) {
		for _, b := range list {
			if b.AccountIndex >= 0 && b.AccountIndex < len(keys) && b.Owner != "" {
				tokenOwner[keys[b.AccountIndex].Pubkey] = b.Owner
			}
			if !owners[b.Owner] {
				continue
			}
			n, ok := b.raw()
			if !ok {
				continue
			}
			d := deltas[b.Mint]
			if d == nil {
				d = &tokDelta{amt: new(big.Int), dec: b.UITokenAmount.Decimals}
				deltas[b.Mint] = d
			}
			if sign < 0 {
				n = new(big.Int).Neg(n)
			}
			d.amt.Add(d.amt, n)
		}
	}
	add(m.PreTokenBalances, -1)
	add(m.PostTokenBalances, +1)
	if w := deltas[WrappedSOLMint]; w != nil {
		lamports.Add(lamports, w.amt)
		delete(deltas, WrappedSOLMint)
	}

	legs := []core.Leg{}
	if lamports.Sign() != 0 {
		legs = append(legs, core.Leg{AssetKey: solAssetKey, Symbol: solAssetKey, Amount: legAmount(lamports, lamportsDecimals)})
	}
	for mint, d := range deltas {
		if d.amt.Sign() == 0 {
			continue
		}
		key, sym := s.tokenLegIdentity(mint)
		legs = append(legs, core.Leg{AssetKey: key, Symbol: sym, Mint: mint, Amount: legAmount(d.amt, d.dec)})
	}
	if len(legs) == 0 && fee == nil {
		return core.Tx{}, false
	}
	// Outgoing legs first so a swap reads "from -> to"; ties in a stable order.
	slices.SortFunc(legs, func(a, b core.Leg) int {
		return cmp.Or(cmp.Compare(boolInt(a.Amount > 0), boolInt(b.Amount > 0)), cmp.Compare(a.AssetKey, b.AssetKey), cmp.Compare(a.Mint, b.Mint))
	})

	// Position programs count only when the user called them directly: Jupiter's aggregator
	// routes ordinary swaps through the perps pool, which then runs as an inner instruction.
	position := callsProgram(tx, defi.JupLockedVoterProgram) || callsProgram(tx, defi.JupPerpsProgram)
	kind := activityKind(legs, usesProgram(tx, StakeProgramID), position)
	t := core.Tx{
		ID:          core.TxID(s.ID(), e.Signature),
		SourceID:    s.ID(),
		ExternalID:  e.Signature,
		Kind:        kind,
		Status:      core.TxConfirmed,
		Legs:        legs,
		Fee:         fee,
		TxHash:      e.Signature,
		ExplorerURL: solscanTxURL + e.Signature,
	}
	if bt := cmp.Or(tx.BlockTime, e.BlockTime); bt != nil {
		t.Time = time.Unix(*bt, 0).UTC()
	}
	if !isNullJSON(m.Err) {
		t.Status = core.TxFailed
	}
	if kind == core.TxSend || kind == core.TxReceive {
		t.Counterparty = s.counterparty(tx, tokenOwner)
	}
	return t, true
}

// ownerSet is owners() as a set: the addresses whose balance changes are the wallet's own.
func (s *Source) ownerSet() map[string]bool {
	set := map[string]bool{}
	for _, o := range s.owners() {
		set[o] = true
	}
	return set
}

// rentDust is the SOL amount below which a leg is account rent (about 0.002-0.003 SOL per
// account), not a real movement; it is ignored when judging whether a position-program tx is
// one-directional.
const rentDust = 0.01

// activityKind classifies by the owner's legs. For a tx through a position program (lock, perps)
// that moves value one way only, ignoring rent-sized SOL legs, it is a position move; one that
// moved nothing (failed, or a request that only paid the fee) is a fee. Otherwise opposite signs
// mean the owner traded one asset for another whoever the counterparty is; then a stake-program
// instruction makes it a stake move; otherwise the direction of the money.
func activityKind(legs []core.Leg, stake, position bool) core.TxKind {
	pos, neg := false, false
	posSig, negSig := false, false
	for _, l := range legs {
		pos = pos || l.Amount > 0
		neg = neg || l.Amount < 0
		if l.AssetKey == solAssetKey && math.Abs(l.Amount) < rentDust {
			continue
		}
		posSig = posSig || l.Amount > 0
		negSig = negSig || l.Amount < 0
	}
	switch {
	case position && len(legs) > 0 && !(posSig && negSig):
		return core.TxPosition
	case pos && neg:
		return core.TxSwap
	case stake:
		return core.TxStake
	case pos:
		return core.TxReceive
	case neg:
		return core.TxSend
	}
	return core.TxFee
}

// tokenLegIdentity names a mint the way holdings are named: the exchange ticker for well-known
// mints, "spl:<mint>" otherwise, with the symbol from the metadata cache (never a network
// lookup here) or a shortened mint.
func (s *Source) tokenLegIdentity(mint string) (assetKey, symbol string) {
	assetKey = core.SPLKey(mint)
	if sym, _, ok := KnownMint(mint); ok {
		return sym, sym
	}
	if m, ok := s.res.cache.Get(mint); ok {
		symbol = m.Symbol
	}
	if o, ok := s.cfg.Overrides[mint]; ok && o.Symbol != "" {
		symbol = cleanText(o.Symbol, maxSymbolRunes)
	}
	if symbol == "" {
		symbol = shortKey(mint)
	}
	return assetKey, symbol
}

// callsProgram reports whether a top-level instruction runs the given program.
func callsProgram(tx *rpcTx, programID string) bool {
	return slices.ContainsFunc(tx.Transaction.Message.Instructions, func(i txInstruction) bool { return i.ProgramID == programID })
}

// usesProgram reports whether any instruction, top-level or inner, runs the given program.
func usesProgram(tx *rpcTx, programID string) bool {
	if callsProgram(tx, programID) {
		return true
	}
	for _, inner := range tx.Meta.InnerInstructions {
		if slices.ContainsFunc(inner.Instructions, func(i txInstruction) bool { return i.ProgramID == programID }) {
			return true
		}
	}
	return false
}

// counterparty names the other side of a plain transfer: the wallet on the far end of the
// top-level system and SPL-token transfer instructions that touch the owner. Token transfers
// move between token accounts, so those are mapped to their owning wallets. It is empty when
// there is no transfer or the far end is ambiguous (several different parties).
func (s *Source) counterparty(tx *rpcTx, tokenOwner map[string]string) string {
	owners := s.ownerSet()
	others := map[string]bool{}
	for _, ix := range tx.Transaction.Message.Instructions {
		p, ok := ix.parse()
		if !ok {
			continue
		}
		var from, to string
		switch {
		case ix.ProgramID == systemProgramID && (p.Type == "transfer" || p.Type == "transferWithSeed"):
			from, to = p.Info.Source, p.Info.Destination
		case (ix.ProgramID == TokenProgramID || ix.ProgramID == Token2022ProgramID) && (p.Type == "transfer" || p.Type == "transferChecked"):
			from = cmp.Or(tokenOwner[p.Info.Source], p.Info.Authority, p.Info.MultisigAuthority)
			to = cmp.Or(tokenOwner[p.Info.Destination], p.Info.Destination)
		default:
			continue
		}
		switch {
		case owners[from] && !owners[to]:
			others[to] = true
		case owners[to] && !owners[from]:
			others[from] = true
		}
	}
	if len(others) != 1 {
		return ""
	}
	for a := range others {
		return a
	}
	return ""
}

func isNullJSON(b json.RawMessage) bool {
	s := strings.TrimSpace(string(b))
	return s == "" || s == "null"
}
