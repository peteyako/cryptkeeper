# Tax M4: review queue, position write-offs, closing a year — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- A "Needs review" view where the user tags transactions, overrides prices and writes off closed positions.
- A way to close a tax year once it's filed, and to reopen it.

**Architecture:**
- Engine extensions in `internal/tax`: an exclusive end time `Input.To`, `WriteOff` entries, the `position-gain` tag, and parked balances exposed on `Result`.
- `tax.yaml` gains `writeOffs`.
- Server handlers under `/reports/tax/*` write tags, prices and write-offs through `SettingsStore.Update` and invalidate the tax cache.
- Closing runs `Compute` with `To` = the next 1 January and saves a `tax.Year` with `Source: "cryptkeep"`.

**Tech Stack:** Go 1.26, templ + Datastar SSE.

**Spec:** `docs/superpowers/specs/2026-10-16-koinly-tax-design.md` §1 (tags, position moves), §3 (closing a year), §4 ("Needs review" tab).

## Global Constraints

- **Tags that apply by direction** (from M1, unchanged):
  - receive: `purchase income airdrop gift-received own-wallet position spam`
  - send: `sale gift lost own-wallet position spam`
  - New: `position-gain` applies to receives only.
- **`position-gain`** on a position withdrawal: the excess over parked becomes a disposal row (`CostUSD 0`, `ProceedsUSD` = the excess's market value, `ShortTerm`, `Note ""`) plus a lot at market value. No `Income` row and no `IssuePositionExcess`.
- **Write-off:** `WriteOff{Wallet, Asset string; Amount float64; Date time.Time; ProceedsUSD float64; Note string}`.
  - At `Date`, it disposes `min(Amount, parked)` units FIFO from that wallet's lots, with proceeds `ProceedsUSD` pro-rata.
  - Its rows have `TxID "writeoff:<index>"` and `Note ""`. Parked is reduced by the same amount.
  - Any amount above parked raises `IssueWriteOffExceedsParked` (new IssueKind `"writeoff-exceeds-parked"`).
- **Closed-year immutability:** tags, price overrides and write-offs dated before the open-year start are refused with `"<year> is closed; reopen it to change it"`.
- **Closing year `Y`** is allowed only when `now` (tax timezone) is on or after 1 January of `Y+1`, and `Y` is the open year.
- **Reopening** applies only to the latest closed year, and is refused for a `Source: "koinly"` year while any later year is closed.
- Do NOT `git add`, `git commit`, `git stash` or switch branches.
- Gate: `task generate && task vet && task test`.

## Review Focus

1. **Bulk-tagging 500 spam airdrops.** One request tags every unmatched receive of one asset, and doesn't write `tax.yaml` 500 times. Test: Task 3 `TestTagBulkOneWrite`.
2. **A tag on a tx that no longer exists** (the source was removed). It is ignored by the engine and listed as stale in the review view, with a remove button. Test: Task 3 `TestStaleTagListed`.
3. **Closing while issues are open.** Allowed after a confirmation that shows the count; the saved year records `OpenIssues` (new `Year` field, `yaml:"openIssues,omitempty"`). Test: Task 4 `TestCloseYearRecordsOpenIssues`.
4. **A write-off dated in a closed year.** Refused. Test: Task 3 `TestWriteOffInClosedYearRefused`.
5. **Activity after Dec 31 while closing.** It must not leak into the closed year's disposals or closing lots. Test: Task 1 `TestComputeToExcludesLater`.

---

### Task 1: Engine — `To`, write-offs, `position-gain`, parked balances

**Files:**
- Modify: `internal/tax/types.go`, `internal/tax/engine.go`, `internal/tax/position.go`, `internal/tax/tags.go`, `internal/tax/settings.go` (`WriteOffs []WriteOff yaml:"writeOffs,omitempty"`, with validation: amount > 0, proceeds ≥ 0 and finite, wallet and asset non-empty, date set)
- Test: `internal/tax/writeoff_test.go`, `internal/tax/engine_test.go`

**Interfaces:**
- Produces:
  - `Input.To time.Time`: txs, manual lots and write-offs at or after `To` are ignored. A zero value means no end.
  - `Input.WriteOffs []WriteOff`.
  - `const TagPositionGain Tag = "position-gain"`.
  - `tax.Parked`, `Input.OpeningParked` and `Result.Parked` already exist (M2 Task 1).
  - `IssueWriteOffExceedsParked`.

- [ ] **Step 1: Write failing tests**

```go
func TestComputeToExcludesLater(t *testing.T)     // To = 2027-01-01 NY; a sale on 2027-01-02 is absent; closing lots still hold the units
func TestWriteOffDisposesParked(t *testing.T)     // lot SOL 2 @300; position deposit −2; WriteOff{2, Proceeds 50} on 2026-06-01 → disposal cost 300 proceeds 50 gain −250 TxID "writeoff:0"; Parked empty; lots empty
func TestWriteOffExceedsParked(t *testing.T)      // parked 1, write-off 1.5 → disposes 1; IssueWriteOffExceedsParked amount 0.5
func TestPositionGainTag(t *testing.T)            // deposit 1000 JUP, withdraw +1050 (USD 52.5) tagged position-gain → disposal {50, cost 0, proceeds 2.5}; lot 50 @2.5; no Income; no IssuePositionExcess
func TestParkedExposed(t *testing.T)              // deposit JUP −1000 → Result.Parked == [{sol JUP 1000}]
func TestSettingsWriteOffValidation(t *testing.T) // amount 0 → error; proceeds NaN → error
```

- [ ] **Step 2: Run to verify they fail.** `go test ./internal/tax/ -run 'TestComputeTo|TestWriteOff|TestPositionGain|TestParked|TestSettingsWriteOff' -v` → FAIL.
- [ ] **Step 3: Implement.** Write-offs are applied in the same time-ordered merge as manual lots, before txs at the same instant.
- [ ] **Step 4: Run to verify they pass.** Same command; then `go test ./internal/tax/...`. Expected: PASS.

---

### Task 2: Review view model and template

**Files:**
- Create: `internal/server/review.go` (builds the view model), `web/templates/review.templ`, `web/templates/review_view.go`
- Modify: `web/templates/models.go`
- Test: `internal/server/review_test.go`

**Interfaces:**
- Consumes: `taxResult`, the `SettingsStore`, activity (to show tx details), `s.sourceLabels()`.
- Produces:
  - `templates.ReviewPage`, with:
    - `Groups []ReviewGroup{Kind tax.IssueKind; Title string; Asset, Symbol string; Count int; Items []ReviewItem}`
    - `Parked []ParkedRow{Wallet, WalletLabel, Asset, Symbol string; Amount float64}`
    - `Stale []StaleTag{TxID string; Tag tax.Tag}`
    - `OpenYear int`
  - `ReviewItem{TxID, WalletLabel, Time, Direction ("in"|"out"), Asset, Symbol, Amount, USD *float64, Explorer, Tag, Note, PriceKey string; TagOptions []string}`.
  - The component `templates.ReviewPanel(p ReviewPage)` has id `review-panel`.
  - Group titles:
    - `"Unmatched receives"`
    - `"Unmatched sends"`
    - `"Missing prices"`
    - `"Missing cost basis"`
    - `"Position returned more than went in"`
    - `"Unpaired transfers"`
    - `"Write-off exceeds parked"`
  - Groups are per `(kind, asset)`, sorted by count descending. Items are listed newest first, capped at 50 per group with a "+N more" line.
  - Already-tagged txs appear under a collapsed `"Tagged"` group, so the user can change a tag.
- Function: `func (s *Server) reviewPage(ctx context.Context) templates.ReviewPage`.

- [ ] **Step 1: Write failing tests**

```go
func TestReviewGroupsByKindAndAsset(t *testing.T) // 3 unmatched receives of spl:A and 1 of SOL → two groups, spl:A first with Count 3
func TestReviewTagOptionsByDirection(t *testing.T) // receive item TagOptions == receive tags + position-gain only when the tx is a position move; send item == send tags
func TestReviewParkedRows(t *testing.T)           // a position deposit → one Parked row with the wallet label
func TestStaleTagListed(t *testing.T)             // tax.yaml tag for an unknown tx id → Stale has it
```

- [ ] **Step 2–4:** Run `go test ./internal/server/ -run 'TestReview|TestStaleTag' -v` → FAIL; implement; run → PASS.

---

### Task 3: Review actions — tag, bulk tag, price, write-off

**Files:**
- Create: `internal/server/review_handlers.go`
- Modify: `internal/server/server.go` (routes)
- Test: `internal/server/review_test.go`

**Interfaces:**
- **Routes** (POST, SSE; each reply re-renders `ReviewPanel` and the Tax summary tiles; each calls `invalidateTax()`):
  - `/reports/tax/tag`: fields `tx`, `tag` (empty removes it), `note`.
  - `/reports/tax/tag-bulk`: fields `kind`, `asset`, `tag`. It tags every item currently in that group, in one `SettingsStore.Update`.
  - `/reports/tax/price`: fields `key`, `usd` (empty removes it).
  - `/reports/tax/writeoff`: fields `wallet`, `asset`, `amount`, `date` (YYYY-MM-DD, tax timezone, 12:00), `proceeds`, `note`.
  - `/reports/tax/writeoff/{i}/delete`.
  - `/reports/tax/tag/{tx}/delete` (for stale tags).
- **Validation:**
  - A tag not allowed for the tx's direction → `"tag <x> doesn't apply to a <direction>"`.
  - A closed year → the Global Constraints message.
  - Non-numeric input → `"enter a number"`.

- [ ] **Step 1: Write failing tests**

```go
func TestTagSavesAndRecomputes(t *testing.T)     // tag an unmatched receive as income → tax.yaml has it; the response no longer lists it under Unmatched receives
func TestTagBulkOneWrite(t *testing.T)           // 500 unmatched receives of spl:SPAM; bulk spam → one backup file created (one write), group gone
func TestTagWrongDirection(t *testing.T)         // tag gift on a receive → error text, nothing saved
func TestPriceOverride(t *testing.T)             // price key "<tx>#0" = 12.5 → missing-price item resolved
func TestWriteOffSaves(t *testing.T)             // write-off form → tax.yaml writeOffs[0]; Parked row reduced
func TestWriteOffInClosedYearRefused(t *testing.T) // 2026 closed; write-off dated 2026-05-01 → message "2026 is closed; reopen it to change it"
func TestTagInClosedYearRefused(t *testing.T)    // tx dated in the closed year → refused
```

- [ ] **Step 2–4:** Run `go test ./internal/server/ -run 'TestTag|TestPriceOverride|TestWriteOff' -v` → FAIL; implement; run → PASS.

---

### Task 4: Close and reopen a year

**Files:**
- Modify: `internal/tax/years.go` (`Year.OpenIssues int yaml:"openIssues,omitempty"`), `internal/server/review_handlers.go` (or a new `internal/server/close_handlers.go`), `internal/server/server.go`, `web/templates/review.templ` (the close card)
- Test: `internal/server/close_test.go`

**Interfaces:**
- **Routes:**
  - `POST /reports/tax/close`: field `year`, plus `confirm=1` on the second step. The first step replies with a confirmation card: "Close <Y>? <N> review items are still open. Closed years can't be edited.", with Confirm and Cancel.
  - `POST /reports/tax/reopen`: field `year`, with the same two-step confirmation.
- **Close** computes `tax.Compute` with `To` = Jan 1 `Y+1` (tax timezone), then saves `tax.Year{Year: Y, Source: "cryptkeep", ClosedAt: now, Summary: tax.Summarize(...), Disposals, Income, GiftsLost, ClosingLots: result.Lots, ClosingParked: result.Parked, OpenIssues: len(issues in Y)}`. Disposals and income are filtered to year `Y` (all are in `Y`, given `From`/`To`).
- **Refusals:** `"<Y> hasn't ended yet"`, `"close <open year> first"` (when `Y` isn't the open year), and `"only <latest> can be reopened"`.

- [ ] **Step 1: Write failing tests**

```go
func TestCloseYearRequiresYearEnd(t *testing.T)      // now 2026-10-16 → close 2026 refused "2026 hasn't ended yet"
func TestCloseYearWritesYear(t *testing.T)           // now 2027-02-01; activity in 2026 and 2027 → year 2026 saved with only 2026 disposals; ClosingLots = lots as of 2027-01-01; open year becomes 2027
func TestCloseYearRecordsOpenIssues(t *testing.T)    // 2 unmatched receives in 2026 → OpenIssues 2; confirmation text shows "2 review items"
func TestReopenLatestOnly(t *testing.T)              // 2025 (koinly) + 2026 closed → reopen 2025 refused; reopen 2026 ok
```

- [ ] **Step 2–4:** Run `go test ./internal/server/ -run 'TestCloseYear|TestReopen' -v` → FAIL; implement; then `task generate && task vet && task test` → PASS.
