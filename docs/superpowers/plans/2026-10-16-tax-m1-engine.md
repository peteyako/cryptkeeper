# Tax M1: per-wallet FIFO lot engine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pure, fully tested `internal/tax` package that turns opening lots plus activity into per-wallet FIFO lots, disposals (short/long term), income, gift/lost removals and review issues.

**Architecture:**
- `Compute(Input) Result` replays activity oldest-first over one FIFO queue per `(wallet, asset)`, where wallet = `core.Tx.SourceID`.
- Valuation, the queue, transfers, tags and positions each live in their own file.
- Nothing is wired into the server yet. **This differs from the spec's milestone 1:** the dashboard switch and the deletion of `avgcost.go` move to M3, next to the Koinly import. Without opening lots, FIFO would show no basis for anything held before 2026, which is a regression.

**Tech Stack:** Go 1.26 standard library only. Uses `crypto-portfolio/internal/core` types.

**Spec:** `docs/superpowers/specs/2026-10-16-koinly-tax-design.md` (§1 is this milestone).

## Global Constraints

- Package `crypto-portfolio/internal/tax`. No new dependencies.
- Amounts and USD are `float64`, as in `internal/costbasis`. Lots at or below `dust = 1e-12` are dropped.
- Wallet = `core.Tx.SourceID`. The tax-only pseudo-wallet is the constant `Untracked = "untracked"`.
- Fiat legs (`USD EUR GBP CAD JPY AUD CHF`) carry value only; they are never lots. Stablecoins (`core.Pegged`) are tracked as assets.
- `Leg.USD` is signed like `Amount`. Always use its absolute value.
- **Holding period:** long-term only when the sale's calendar date (in `Input.Loc`) is after the acquisition date's one-year anniversary. A sale on the anniversary itself is short-term.
- Disposal `Note` values are exactly `""`, `"Fee"` and `"Missing cost basis"`. `GiftsLost` notes are exactly `"Gift"` and `"Lost"`. These match Koinly's report.
- When uncertain, err toward short-term and an `Issue`. Never invent long-term.
- Package doc: "Not tax advice: an informational estimate."
- Focused runs: `go test ./internal/tax/ -run <Name> -v`. Full gate: `task vet` and `task test`.

## Review Focus

1. **Transfer receive time-stamped before its send.** The matcher allows the receive up to 10 minutes earlier. The pair must still move lots once. Test: Task 5 `TestTransferReceiveBeforeSend`.
2. **Year boundary in the tax timezone.** A tx at `2026-01-01T03:00Z` is 2025 in New York and must be ignored when `From` is 2026-01-01 New York. Test: Task 3 `TestFromUsesTaxZone`.
3. **Same asset on both sides of one tx** (a SOL rent refund inside a swap). Legs must be netted per asset before treatment. Test: Task 3 `TestSameAssetLegsNetted`.
4. **Float residue after a full disposal.** No phantom 1e-17 lots may remain. Test: Task 1 `TestQueueDustResidue`.
5. **Ties at the same timestamp.** Results must be deterministic: order by `Time`, then `ID`. Test: Task 3 `TestSameTimeOrderedByID`.

---

### Task 1: Types, FIFO queue, holding period

**Files:**
- Create: `internal/tax/types.go`, `internal/tax/queue.go`
- Test: `internal/tax/helpers_test.go`, `internal/tax/queue_test.go`

**Interfaces:**
- Produces (in `types.go`, exported, with `yaml` and `json` tags in camelCase like `costbasis/manual.go`):
  - `const Untracked = "untracked"`
  - `type Term string`: `ShortTerm = "short"`, `LongTerm = "long"`
  - `type Tag string`: `TagPurchase "purchase"`, `TagIncome "income"`, `TagAirdrop "airdrop"`, `TagGiftReceived "gift-received"`, `TagOwnWallet "own-wallet"`, `TagPosition "position"`, `TagSpam "spam"`, `TagSale "sale"`, `TagGift "gift"`, `TagLost "lost"`
  - Lot flags: `FlagMissingBasis = "missing-basis"`, `FlagTransferDate = "transfer-date"`
  - `type Lot struct { Wallet, Asset string; Amount, CostUSD float64; Acquired time.Time; Various bool; TxID, Flag string }`. `Various` means "acquired on various dates up to `Acquired`" and prints as VARIOUS.
  - `type Disposal struct { Sold, Acquired time.Time; Various bool; Asset string; Amount, CostUSD, ProceedsUSD, GainUSD float64; Wallet string; Term Term; Note, TxID string }`
  - `type Income struct { Date time.Time; Asset string; Amount, ValueUSD float64; Type, Wallet, TxID string }`. `Type` is `"reward"`, `"income"`, `"airdrop"` or `"position"`.
  - `type IssueKind string`: `IssueUnmatchedReceive "unmatched-receive"`, `IssueUnmatchedSend "unmatched-send"`, `IssueMissingPrice "missing-price"`, `IssueMissingBasis "missing-basis"`, `IssuePositionExcess "position-excess"`, `IssueTransferUnpaired "transfer-unpaired"`
  - `type Issue struct { Kind IssueKind; TxID, Wallet, Asset string; Amount float64; Time time.Time }`
- Produces (in `queue.go`, unexported):
  - `type queue struct` with `add(l Lot)` (keeps order by `Acquired`, stable for equal times), `take(amount float64) (taken []Lot, short float64)` (FIFO; splits a lot with cost pro-rata; drops residue ≤ dust), and `total() float64`
  - `termOf(acquired, sold time.Time, loc *time.Location) Term`

- [ ] **Step 1: Write test helpers and failing tests**

`helpers_test.go` mirrors `internal/costbasis/helpers_test.go`, adding a wallet: `usd(v) *float64`, `at(y, m, d, h int) time.Time` (UTC), `lg(asset, amount, usd)`, `mk(id, wallet string, t time.Time, kind core.TxKind, legs ...core.Leg) core.Tx` (Status confirmed, `ID = core.TxID(wallet, id)`), and `near(t, name, got, want)`.

```go
func TestQueueFIFOByAcquiredDate(t *testing.T) {
	var q queue
	q.add(Lot{Asset: "SOL", Amount: 1, CostUSD: 100, Acquired: at(2025, 6, 1, 0)})
	q.add(Lot{Asset: "SOL", Amount: 1, CostUSD: 50, Acquired: at(2025, 1, 1, 0)})
	taken, short := q.take(1.5)
	// want taken[0] = 1 @ 50 (2025-01-01), taken[1] = 0.5 @ 50 (2025-06-01); short 0
	// want q.total() = 0.5 and the remaining lot's CostUSD = 50
}
func TestQueueTakeShortfall(t *testing.T) // 1 @ 100, take 1.25 → one taken lot of 1 @ 100, short 0.25, total 0
func TestQueueDustResidue(t *testing.T)   // add 0.1 and 0.2, take 0.3 → total() == 0 and no lots left
func TestTermBoundary(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	acq := time.Date(2025, 3, 10, 3, 0, 0, 0, time.UTC) // 2025-03-09 in NY
	// sold 2026-03-10T02:00Z (2026-03-09 22:00 NY, the anniversary) → ShortTerm
	// sold 2026-03-10T05:00Z (2026-03-10 01:00 NY)                   → LongTerm
	// a Various lot with Acquired 2024-12-31, sold 2026-01-02        → LongTerm
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -v`
Expected: FAIL (undefined: queue, Lot, termOf).

- [ ] **Step 3: Implement `types.go` and `queue.go`**

`termOf` converts both times to `loc` (nil means UTC), truncates to calendar dates, and returns `LongTerm` iff `soldDate.After(acqDate.AddDate(1, 0, 0))`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tax/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tax/
git commit -m "tax: lot types, FIFO queue, holding period"
```

---

### Task 2: Leg valuation and the trade fill rule

**Files:**
- Create: `internal/tax/value.go`
- Test: `internal/tax/value_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
  - `func PriceKey(txID string, leg int) string` returns `"<txID>#<leg>"`.
  - `func FeePriceKey(txID string) string` returns `"<txID>#fee"`.
  - Both are exported, because M4 writes overrides under these keys.
  - Unexported `type valued struct { core.Leg; idx int; usd float64; known bool }`.
  - `valueLeg(l core.Leg, key string, prices map[string]float64) (usd float64, known bool)`. The order is: override, then `|Leg.USD|` when finite and non-zero, then `|Amount|` for `core.Pegged`, else unknown.
  - `fillTrade(neg, pos []*valued)`: copy the rule from `costbasis.sideValue` / `fillSide` (`internal/costbasis/avgcost.go:306-335`). When a side has exactly one unvalued leg and the other side is fully valued, that leg takes the other side's total minus its own side's known legs. `avgcost.go` is deleted in M3, so the copy is temporary duplication.

- [ ] **Step 1: Write failing tests**

```go
func TestValueOverrideWins(t *testing.T)    // Leg.USD −200, prices[PriceKey("w:a",0)] = 150 → 150, known
func TestValueAbsOfSignedUSD(t *testing.T)  // Leg SOL −1, USD −200 → 200
func TestValuePegged(t *testing.T)          // USDC +25, no USD → 25, known
func TestValueUnknown(t *testing.T)         // spl:X +100, no USD → 0, unknown
func TestFillOneUnvaluedLeg(t *testing.T)   // neg: SOL −1 (unknown); pos: USDC +180 → SOL valued 180
func TestFillTwoUnvaluedNoop(t *testing.T)  // neg: spl:A, spl:B both unknown; pos: USDC +50 → both stay unknown
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -run 'TestValue|TestFill' -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement `value.go`.**

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tax/ -run 'TestValue|TestFill' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tax/value.go internal/tax/value_test.go
git commit -m "tax: leg valuation with overrides and trade fill rule"
```

---

### Task 3: `Compute` for trades, fees, opening and manual lots

**Files:**
- Create: `internal/tax/engine.go` (with the package doc comment)
- Test: `internal/tax/engine_test.go`

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces:

```go
type Input struct {
	From    time.Time          // start of the first open tax year; earlier txs are ignored
	Loc     *time.Location     // tax timezone; nil = UTC
	Opening []Lot              // closing lots of the last closed year
	Manual  []Lot              // manual acquisitions, applied at Acquired (zero → From)
	Txs     []core.Tx
	Tags    map[string]Tag     // by Tx.ID
	Prices  map[string]float64 // PriceKey / FeePriceKey → USD
	Spam    map[string]bool    // asset keys ignored entirely
}
type Result struct {
	Lots      []Lot      // open lots, sorted by Wallet, Asset, Acquired
	Disposals []Disposal // sorted by Sold, then TxID
	Income    []Income
	GiftsLost []Disposal
	Issues    []Issue
}
func Compute(in Input) Result
```

- **Processing order:**
  - Opening lots first.
  - Then txs with `!Time.Before(From)`, sorted by `(Time, ID)`.
  - Manual lots are inserted before the first tx whose `Time` is after their `Acquired`.
- **Per tx:**
  - Failed: only the fee.
  - Otherwise: net legs per asset (a netted leg keeps the index of that asset's first leg, for `PriceKey`), value them and apply the fill rule *including fiat legs*, then drop fiat and spam legs, apply the kind rule, then the fee.
- **This task implements:**
  - `trade` / `swap`: disposals per consumed lot piece, with proceeds pro-rata by amount; acquisitions at the valued cost.
  - The fee (`Tx.Fee`, and kind `fee` legs): a disposal at market value with Note `"Fee"`.
  - A shortfall: a disposal row with `CostUSD 0`, `Acquired = Sold`, `ShortTerm`, Note `"Missing cost basis"`, plus `IssueMissingBasis`.
  - An unknown value: 0, plus `IssueMissingPrice` for each unvalued leg.
  - `stake`: fee only.

- [ ] **Step 1: Write failing tests**

All txs are in wallet `"sol"` unless named otherwise.

```go
func TestTradeFIFOAndTerms(t *testing.T) {
	// Opening: SOL 10 cost 1000 acq 2025-02-01; SOL 10 cost 2000 acq 2025-08-01
	// 2026-03-01 swap: SOL −15 (USD −3000), USDC +3000
	// Disposals: {10 SOL, cost 1000, proceeds 2000, gain 1000, LongTerm},
	//            {5 SOL, cost 1000, proceeds 1000, gain 0, ShortTerm}
	// Lots: SOL 5 cost 1000 acq 2025-08-01; USDC 3000 cost 3000 acq 2026-03-01
}
func TestFeeIsDisposal(t *testing.T)        // swap with Fee SOL −0.01 USD −2 → extra disposal Note "Fee", amount 0.01, proceeds 2
func TestFailedTxOnlyFee(t *testing.T)      // Status failed, legs SOL −1/USDC +200, fee SOL −0.00001 → one Fee disposal; USDC lot absent
func TestMissingCostBasis(t *testing.T)     // lot SOL 1; sell 2 for $400 → rows {1, proceeds 200}, {1, cost 0, proceeds 200, Note "Missing cost basis", Acquired==Sold, ShortTerm}; one IssueMissingBasis
func TestMissingPrice(t *testing.T)         // swap spl:X −100 / spl:Y +50, no USD → proceeds 0, spl:Y lot cost 0; two IssueMissingPrice
func TestFromUsesTaxZone(t *testing.T)      // Loc NY, From 2026-01-01 00:00 NY; a buy at 2026-01-01T03:00Z is ignored; one at T06:00Z makes a lot
func TestManualLotAppliedAtDate(t *testing.T) // Manual BTC 1 @ 50000 acq 2026-02-01; sell 0.5 on 2026-01-15 → Missing cost basis; sell 0.5 on 2026-03-01 → cost 25000
func TestFiatAndSpamIgnored(t *testing.T)   // trade USD −100 (no USD field) / BTC +0.001 (no USD) → BTC lot cost 100 via the fill rule, no USD lot; Spam{"spl:SPAM"} receive → no lot, no issue
func TestSameAssetLegsNetted(t *testing.T)  // swap SOL −1 ($200), SOL +0.002 ($0.4), JUP +500 ($199.6) → one SOL disposal of 0.998, proceeds 199.6
func TestSameTimeOrderedByID(t *testing.T)  // buy "b" and sell "a" at the same instant: "a" (sell) runs first → Missing cost basis
func TestStakeIsNoop(t *testing.T)          // stake tx SOL −5 with Fee → only the Fee disposal; SOL lot amount unchanged except the fee
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -run 'TestTrade|TestFee|TestFailed|TestMissing|TestFrom|TestManual|TestFiat|TestSame|TestStake' -v`
Expected: FAIL (undefined: Compute, Input).

- [ ] **Step 3: Implement `engine.go`.**

Keep one `map[string]map[string]*queue` (wallet → asset → queue). Each kind gets its own small method on an unexported `engine` struct, so Tasks 4–7 can add methods without growing `Compute`.

- [ ] **Step 4: Run to verify it passes**

Same command. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tax/engine.go internal/tax/engine_test.go
git commit -m "tax: Compute for trades, fees, opening and manual lots"
```

---

### Task 4: Untagged receive, send, reward, other

**Files:**
- Modify: `internal/tax/engine.go`
- Test: `internal/tax/movement_test.go`

**Interfaces:**
- Consumes: Task 3 `engine`.
- Produces: kind handling for `receive`, `send`, `reward`, `other` (and unknown kinds treated as `other`) when the tx has no tag and no `PeerID`.

- [ ] **Step 1: Write failing tests**

```go
func TestUnmatchedReceiveIsPurchase(t *testing.T) // receive SOL +2 USD 300 → lot 2 @ 300 acq tx time; IssueUnmatchedReceive; no Income
func TestUnmatchedSendIsSale(t *testing.T)        // lot SOL 2 @ 300; send SOL −1 USD −200 → disposal proceeds 200 cost 150 gain 50; IssueUnmatchedSend
func TestRewardIsIncomeAndLot(t *testing.T)       // reward SOL +0.1 USD 15 → Income{Type "reward", ValueUSD 15}; lot 0.1 @ 15; no issue
func TestOtherKindBySign(t *testing.T)            // kind other: +legs act like receive, −legs like send (each with its issue)
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -run 'TestUnmatched|TestReward|TestOther' -v`
Expected: FAIL.

- [ ] **Step 3: Implement the kind handling in `engine.go`.**

- [ ] **Step 4: Run to verify it passes**

Same command. Expected: PASS, and the Task 3 tests still pass (`go test ./internal/tax/`).

- [ ] **Step 5: Commit**

```bash
git add internal/tax/
git commit -m "tax: receive, send, reward and other kinds"
```

---

### Task 5: Transfers move lots between wallets

**Files:**
- Create: `internal/tax/transfer.go`
- Test: `internal/tax/transfer_test.go`

**Interfaces:**
- Consumes: the Task 3 `engine` and `queue.take`/`add`.
- Produces: `(e *engine) transfer(tx core.Tx)`, called for any tx with a `PeerID`. Behavior:
  - On the first side seen, look up the peer in an `ID → Tx` index built from `Input.Txs`, then process the pair once (remember both IDs).
  - Sender = the side whose legs are net negative.
  - Lots move FIFO with their `Acquired`, `Various` and cost unchanged.
  - When sent > received, the difference is a fee disposal from the sender (Note `"Fee"`, valued at the sender leg's per-unit USD).
  - When received > sent, the extra is an acquisition at market value.
  - A sender shortfall becomes receiver lots with `CostUSD 0`, `Acquired` = the transfer time, and `Flag FlagMissingBasis`, plus `IssueMissingBasis`.
  - A peer that isn't in the input (it falls before `From`): skip that side and raise `IssueTransferUnpaired`.
  - Each side's `Tx.Fee` is still disposed by the normal fee path.

- [ ] **Step 1: Write failing tests**

```go
func TestTransferMovesLotsKeepingDates(t *testing.T) {
	// Opening wallet "a": BTC 0.5 @ 10000 acq 2024-06-01; BTC 0.5 @ 30000 acq 2025-09-01
	// 2026-02-01 send in "a": BTC −0.6 (USD −60000), Fee BTC −0.0001 (USD −10), PeerID → recv
	// 2026-02-01 recv in "b": BTC +0.6, PeerID → send
	// Lots "b": 0.5 @ 10000 acq 2024-06-01; 0.1 @ 6000 acq 2025-09-01
	// Lots "a": 0.3999 @ 23994 acq 2025-09-01  (0.4 @ 24000 left after the move, then the fee)
	// Disposals: exactly one, Note "Fee", amount 0.0001, cost 6, proceeds 10, gain 4, ShortTerm
}
func TestTransferReceivedLessIsFee(t *testing.T)       // "a" holds SOL 1 @ 100; send −1 ($200), recv +0.99 → Fee disposal 0.01, cost 1, proceeds 2; "b" lot 0.99 @ 99
func TestTransferShortfallMissingBasis(t *testing.T)   // "a" holds BTC 0.2; transfer 0.5 → "b": 0.2 with original dates + 0.3 cost 0 FlagMissingBasis; one IssueMissingBasis
func TestTransferReceiveBeforeSend(t *testing.T)       // recv 5 min before send → same lots as the normal case; no double move
func TestTransferPeerMissing(t *testing.T)             // recv with PeerID not in Txs → no lot; IssueTransferUnpaired
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -run TestTransfer -v`
Expected: FAIL.

- [ ] **Step 3: Implement `transfer.go` and route `PeerID != ""` txs to it from `engine.go`.**

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tax/ -v`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/tax/transfer.go internal/tax/transfer_test.go internal/tax/engine.go
git commit -m "tax: transfers carry lots between wallets"
```

---

### Task 6: Tags

**Files:**
- Create: `internal/tax/tags.go`
- Test: `internal/tax/tags_test.go`

**Interfaces:**
- Consumes: `Input.Tags`, `Input.Prices`, the Task 4 movement handling.
- Produces: tag dispatch for `receive`, `send` and `other` txs. Tags on trade, swap, transfer, reward and stake txs are ignored. A tag that doesn't fit the direction (for example `gift` on a receive) falls back to the default treatment. Tagged txs never raise the unmatched-receive or unmatched-send issue.

| Tag | Effect |
| --- | --- |
| `purchase` / `sale` | The default treatment, with the issue cleared. |
| `income`, `airdrop` | `Income{Type: "income" \| "airdrop"}` at market value, plus a lot at market value. |
| `gift-received`, `own-wallet` (receive) | A lot with cost = the price override if set, else 0 with `FlagMissingBasis` and `IssueMissingBasis`. No income. |
| `own-wallet` (send) | Lots move FIFO to `Untracked`, keeping their dates. No disposal. |
| `gift`, `lost` | Lots are removed. A `GiftsLost` row per piece with Note `"Gift"` / `"Lost"`, `ProceedsUSD 0`, `GainUSD 0`. |
| `spam` | The tx is ignored entirely, including its fee. |
| `position` | Handled in Task 7. |

- [ ] **Step 1: Write failing tests** (one per row above). Use exact values in this style:

```go
func TestTagIncome(t *testing.T)            // receive JUP +100 USD 80, tag income → Income{Type "income", 80}; lot cost 80; no issues
func TestTagGiftReceivedOverride(t *testing.T) // receive BTC +0.01 USD 900, tag gift-received, Prices[PriceKey(id,0)] = 400 → lot cost 400; no issue
func TestTagGiftReceivedNoBasis(t *testing.T)  // same without override → lot cost 0 FlagMissingBasis; one IssueMissingBasis
func TestTagOwnWalletSend(t *testing.T)     // lot SOL 3 acq 2025-05-01; send −1 tagged own-wallet → Untracked lot 1 acq 2025-05-01; no disposal
func TestTagGiftAndLost(t *testing.T)       // GiftsLost notes "Gift"/"Lost", proceeds 0, gain 0; lots reduced; Disposals empty
func TestTagSpamIgnoresFee(t *testing.T)    // spam-tagged send with Fee → no disposals at all
func TestTagSaleClearsIssue(t *testing.T)   // untagged send → IssueUnmatchedSend; tagged sale → same disposal, no issue
func TestTagWrongDirectionFallsBack(t *testing.T) // receive tagged gift → treated as purchase (lot at FMV); no unmatched issue
func TestTagIgnoredOnTrade(t *testing.T)    // swap tagged spam → still disposes and acquires
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -run TestTag -v`
Expected: FAIL.

- [ ] **Step 3: Implement `tags.go`.**

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tax/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tax/tags.go internal/tax/tags_test.go internal/tax/engine.go
git commit -m "tax: per-transaction tags"
```

---

### Task 7: Position moves (JUP lock, perps collateral, LP)

**Files:**
- Create: `internal/tax/position.go`
- Test: `internal/tax/position_test.go`

**Interfaces:**
- Consumes: `TagPosition` from Task 6.
- Produces: `(e *engine) position(tx core.Tx)`. The engine keeps a `parked map[wallet]map[asset]float64`.
  - A negative leg (deposit) adds to `parked`. Lots are unchanged and there is no disposal.
  - A positive leg (withdrawal) up to `parked` reduces `parked`, with no change to lots.
  - Any excess becomes a lot at market value, plus `Income{Type: "position"}` at market value, plus `IssuePositionExcess`.
  - The fee is still disposed.
  - M2 adds a `core` kind for auto-detected position moves and routes it to this same method.

- [ ] **Step 1: Write failing tests**

```go
func TestPositionDepositKeepsLots(t *testing.T)    // lot JUP 1000 @ 500; send −1000 tagged position, Fee SOL −0.000005 → JUP lot unchanged; only the Fee disposal
func TestPositionWithdrawWithinParked(t *testing.T) // then a receive of +1000 tagged position → no lot change, no income, no issue
func TestPositionWithdrawExcess(t *testing.T)      // deposit 1000, withdraw +1050 (USD 52.5 for the leg) → lot 50 @ 2.5; Income{Type "position", ValueUSD 2.5}; one IssuePositionExcess
```

The excess value is the leg's USD pro-rata: 52.5 × 50/1050 = 2.5.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tax/ -run TestPosition -v`
Expected: FAIL.

- [ ] **Step 3: Implement `position.go` and route `TagPosition` to it.**

- [ ] **Step 4: Run the full gate**

Run: `go test ./internal/tax/ -v && task vet && task test`
Expected: all PASS; `task vet` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/tax/
git commit -m "tax: non-taxable position moves"
```

---

## Later milestones (separate plans, written when M1 lands)

- **M2: Solana inputs.** Staking-reward fetcher (`getInflationReward`), the position-move `core` kind with program-ID detection, CoinGecko-by-mint prices in the activity `Valuer`.
- **M3: Koinly import and switch-over.**
  - Parsers, mapping, roll-back, reconcile, back-fill.
  - `data/tax.yaml` store and `data/tax/<year>.yaml` closed-year store; backup fields; sync horizon.
  - **Then** the dashboard, wallet tiles and Reports P&L move to `tax.Compute`, `avgcost.go` is deleted, and `costbasis.yaml` lots are migrated.
- **M4: Review queue and close/reopen year.**
- **M5: Reports.** Tax view, Koinly CSVs, TurboTax CSV and TXF, 8949 / Schedule D, CPA package. The 8949 box is derived at report time from the wallet type plus the `tax.yaml` override, after checking the rules against the IRS instructions. That is why `Disposal` has no box field in M1.
- **M6: Demo data and docs.**
