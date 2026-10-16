# Tax M2: Solana inputs (position moves, staking rewards, token prices) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the tax engine the Solana activity it needs:
- JUP lock and Jupiter perps moves become a non-taxable `position` kind.
- Native staking rewards become `reward` txs.
- SPL tokens without a Coinbase price get historical USD values from CoinGecko by mint.

**Architecture:**
- New `core.TxPosition` kind, emitted by the Solana classifier when a tx uses a position program. `tax.Compute` routes it to the M1 `position` method.
- The Solana source adds a reward scan to `Activity` (`getInflationReward` per epoch for the owner's stake accounts), with its own cursor in the source's activity state.
- `prices.Service` gains `HistoricalMint`, backed by the CoinGecko `/coins/solana/contract/{mint}/market_chart/range` endpoint and the existing `price_history` table.
- The activity valuer falls back to it by `Leg.Mint`. A revalue pass fills legs that were stored unvalued.
- The CoinGecko demo key is set in Settings and stored sealed in the SQLite `kv` table. The `COINGECKO_API_KEY` env var overrides it.

**Tech Stack:** Go 1.26, templ + Datastar (settings card), modernc sqlite (existing), `httptest` fakes.

**Spec:** `docs/superpowers/specs/2026-10-16-koinly-tax-design.md` (§1 forward rules: position move, native staking reward, leg without a USD value; Milestone 2).

## Global Constraints

- Position programs (use the existing exported constants where they exist):
  - `defi.JupLockedVoterProgram` (`voTpe3tHQ7AjQHMapgSue2HJFAh2cGsdokqN3XqmVSj`)
  - `defi.JupPerpsProgram` (`PERPHjGBqRHArX4DySjwM6UJHiR3sWAatqfdBS2qQJu`)
  - the Jupiter locker `CVMdMd79no569tjc5Sq7kzz8isbfCcFyBS5TLGsrZ5dN` (new exported constant `defi.JupLockerProgram`)
- LP programs (Orca, Raydium) are **not** auto-detected. The user doesn't use LPs, and those programs also route ordinary swaps. LP moves can be tagged `position` in the review queue (M4).
- `rentDust = 0.05` SOL: SOL legs smaller than this in absolute value are ignored when deciding whether a position-program tx is one-directional.
- Staking reward tx: `ExternalID = "reward:<stakePubkey>:<epoch>"`, `Kind = core.TxReward`, one leg `SOL +lamports/1e9`, `Time` = block time of the reward's `effectiveSlot`, `Status` confirmed, `Note = "Staking reward (epoch <N>)"`.
- CoinGecko:
  - Base `https://api.coingecko.com/api/v3`, path `/coins/solana/contract/{mint}/market_chart/range?vs_currency=usd&from=<unix>&to=<unix>`.
  - Header `x-cg-demo-api-key: <key>`, sent only when a key is set.
  - Response `{"prices": [[ms, usd], ...]}`.
  - The demo plan covers only the past 365 days; days older than that are "unknown" without a request.
  - Prices are stored in `price_history` with `asset = "spl:<mint>"` and `day = YYYY-MM-DD`, the daily point nearest 00:00 UTC.
- Key storage: kv key `prices.coingeckoKey` holds `box.Seal(key)`. Env `COINGECKO_API_KEY` wins when set. Settings shows the key only masked (`secrets.Mask`).
- No new third-party dependencies. Tests use `httptest` fakes in the style of `internal/prices/fakes_test.go` and `internal/sources/solana/activity_test.go`.
- Do NOT `git add`, `git commit`, `git stash` or switch branches. The controller snapshots the working tree.
- Focused runs: `go test ./internal/<pkg>/ -run <Name> -v`. Gate: `task generate && task vet && task test` (not `task check`: its git-diff drift step fails on uncommitted regenerated templates by design).

## Review Focus

1. **A Jupiter swap that routes through an AMM.** It must stay `swap`, because only the position programs above trigger `position`. Test: Task 2 `TestClassifySwapNotPosition`.
2. **An RPC that rejects `getInflationReward` for old epochs** (public RPC, `-32004`). Reward backfill must stop for that run without failing the whole activity sync, and must not loop forever. Test: Task 3 `TestRewardsRPCErrorStopsBackfill`.
3. **A token CoinGecko doesn't list (404).** It must be negative-cached: no request on the next valuation of the same mint within 6h. Test: Task 4 `TestHistoricalMintUnknownCached`.
4. **No API key configured.** Requests go out without the header and still work against the fake. Settings shows "not set". Tests: Task 4 `TestHistoricalMintNoKeyHeader`, Task 6 `TestSettingsPricesNotSet`.
5. **A tx that was already stored with an unvalued SPL leg before the key was set.** The revalue pass must fill it on the next sync without touching legs that already have a USD value. Test: Task 5 `TestRevalueFillsOnlyMissing`.

---

### Task 1: `core.TxPosition` and its consumers

**Files:**
- Modify: `internal/core/activity.go` (kind const block), `internal/activity/transfers.go:177-184` (`transferCapable`), `web/templates/activity_view.go` (`TxGlyph`, the title switch, `actKinds`), `internal/costbasis/avgcost.go:204-208`, `internal/tax/engine.go` (dispatch)
- Test: `internal/activity/transfers_test.go`, `web/templates/activity_view_test.go`, `internal/tax/position_test.go`

**Interfaces:**
- Produces: `core.TxPosition TxKind = "position"` with the comment `// moved into or out of one of the user's own positions (locked JUP, perps collateral)`.
- `transferCapable(core.TxPosition) == false`.
- The activity view shows glyph `⇄`, tone `neutral`, title `"Position"` (or `t.Note` when set), and a filter option `{Value: "position", Label: "Positions"}` after "Staking".
- The average-cost engine treats `TxPosition` like `TxStake`: ignored, because it is replaced in M3.
- `tax.Compute` routes `Kind == core.TxPosition` (with no tag override) to the existing `position` method, exactly as `TagPosition` does.

- [ ] **Step 1: Write failing tests**

```go
// internal/activity/transfers_test.go
func TestPositionNeverMatchedAsTransfer(t *testing.T) // a position send of USDC −100 in "a" and a receive of USDC +100 in "b" at the same time → MatchTransfers returns no changes
// web/templates/activity_view_test.go
func TestPositionKindView(t *testing.T) // TxGlyph(position tx) == ("⇄","neutral"); title == "Position"; with Note "Locked JUP" → title "Locked JUP"; actKinds contains Value "position"
// internal/tax/position_test.go
func TestPositionKindRoutes(t *testing.T) // Kind core.TxPosition, JUP −1000, no tag → JUP lot unchanged, no disposal, parked 1000
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/activity/ ./web/templates/ ./internal/tax/ -run 'TestPositionNeverMatched|TestPositionKindView|TestPositionKindRoutes' -v`
Expected: FAIL (undefined: core.TxPosition).

- [ ] **Step 3: Implement the const and the four consumers.**

- [ ] **Step 4: Run to verify they pass**

Run the same command, then `go test ./internal/...`. Expected: PASS.

---

### Task 2: The Solana classifier emits `position`

**Files:**
- Modify: `internal/sources/solana/defi/jupiter.go` (add `JupLockerProgram`), `internal/sources/solana/activity.go` (`activityKind` at :743 and its call at :716)
- Test: `internal/sources/solana/activity_test.go`

**Interfaces:**
- Produces: `func activityKind(legs []core.Leg, stake, position bool) core.TxKind`. The call site passes `position = usesProgram(tx, defi.JupLockedVoterProgram) || usesProgram(tx, defi.JupLockerProgram) || usesProgram(tx, defi.JupPerpsProgram)`.
- **Rule**, evaluated before swap:
  - When `position` is set, drop SOL legs with `|Amount| < rentDust` and test the rest for sign.
  - If the remaining legs are all ≤ 0 or all ≥ 0 (or none remain), the kind is `TxPosition`.
  - Otherwise the old rules apply: swap, then stake, receive, send, fee.
  - The SOL legs themselves stay on the tx unchanged.

- [ ] **Step 1: Write failing tests** (use the `fakeLedger` builders `newLedger`, `l.add(txSpec{...})`, `l.run`, as in `TestActivityStake` at `activity_test.go:746`)

```go
func TestClassifyJupLock(t *testing.T)          // ix program JupLockedVoterProgram, JUP −1000, SOL −0.002 → Kind position; legs JUP −1000 and SOL −0.002 kept
func TestClassifyPerpsCloseIsPosition(t *testing.T) // ix JupPerpsProgram, USDC +250, SOL +0.003 → position
func TestClassifyPerpsJLPBuyIsSwap(t *testing.T)    // ix JupPerpsProgram, SOL −1.0, JLP +40 → swap (mixed sign beyond rentDust)
func TestClassifySwapNotPosition(t *testing.T)  // ix OrcaWhirlpoolProgram only, SOL −1 / USDC +150 → swap
func TestClassifyLockerUnlock(t *testing.T)     // ix JupLockerProgram, JUP +500 → position
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/sources/solana/ -run TestClassify -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/sources/solana/ -v`
Expected: PASS (existing tests too).

---

### Task 3: Native staking rewards in Solana activity

**Files:**
- Modify: `internal/sources/solana/stake.go` (keep pubkeys), `internal/sources/solana/activity.go` (state fields, reward scan after the signature scan)
- Create: `internal/sources/solana/rewards.go`
- Test: `internal/sources/solana/rewards_test.go`, `internal/sources/solana/fakechain_test.go` (new RPC cases)

**Interfaces:**
- Produces:
  - `func (s *Source) stakeAccounts(ctx) (lamports *big.Int, pubkeys []string, err error)`. It returns the pubkeys, sorted. The count = `len(pubkeys)`. Update both callers.
  - The `activityState` gets new `omitempty` fields: `RewardNewest uint64` (the newest epoch fetched), `RewardOldest uint64` (the oldest epoch fetched), and `RewardDone bool` (backfill reached `Since`, or the RPC refused). No version bump, since old JSON still decodes.
  - `func (r *activityRun) rewards(ctx) []core.Tx`:
    - `getEpochInfo` gives `epoch`. The newest completed epoch is `epoch−1`.
    - New epochs: from `RewardNewest+1` up to `epoch−1`.
    - Then backfill downward from `RewardOldest−1` while `!RewardDone`.
    - Per epoch: one `getInflationReward([pubkeys...], {"epoch": e})` and, for the first non-null entry, one `getBlockTime(effectiveSlot)`.
    - Each call spends `r.take()`.
    - Each non-null entry with `amount > 0` emits one tx per the Global Constraints.
    - Backfill stops (`RewardDone = true`) when an epoch's block time is before `Since`, or on an `*rpcError`. An `*rpcError` also adds the warning `"Staking rewards unavailable on this RPC for older epochs"` to the result's warnings (or to the log if `ActivityResult` has no warnings field; check `core.ActivityResult`).
  - With no stake accounts, or `IncludeStake == false`, `rewards` does nothing.
- The reward scan runs after the signature scan in the same `Activity` call, within the remaining budget.

- [ ] **Step 1: Add the fake RPC methods** (`getEpochInfo`, `getInflationReward`, `getBlockTime`) to `fakeChain.serveRPC`, driven by new fields `epoch uint64`, `rewards map[uint64]map[string]uint64` (epoch → pubkey → lamports), `slotTimes map[uint64]int64`, plus `failMethods` support.

- [ ] **Step 2: Write failing tests**

```go
func TestRewardsNewEpochs(t *testing.T)   // epoch 700; stake acct S1 rewards {698: 5_000_000, 699: 6_000_000}; effectiveSlot times inside Since → two reward txs, ExternalID "reward:S1:698"/"reward:S1:699", SOL +0.005/+0.006, Note "Staking reward (epoch 699)"; state RewardNewest 699
func TestRewardsBackfillStopsAtSince(t *testing.T) // epochs 690..699 have rewards; block time of 694 is before Since → txs only for 695..699; RewardDone true
func TestRewardsRPCErrorStopsBackfill(t *testing.T) // getInflationReward fails for epoch < 697 with -32004 → txs 697..699, RewardDone true, warning present, Activity returns no error
func TestRewardsSkipWithoutStake(t *testing.T)     // no stake accounts → no getInflationReward calls
func TestRewardsIdempotent(t *testing.T)           // second run with the returned state fetches no epoch twice
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/sources/solana/ -run TestRewards -v`
Expected: FAIL.

- [ ] **Step 4: Implement `rewards.go` and the state and `stakeAccounts` changes.**

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/sources/solana/ -v`
Expected: PASS.

---

### Task 4: `prices.Service.HistoricalMint` (CoinGecko)

**Files:**
- Modify: `internal/prices/prices.go` (`Options`: `CoinGeckoURL string`, `CoinGeckoKey func() string`; default const `DefaultCoinGeckoURL = "https://api.coingecko.com/api/v3"`)
- Create: `internal/prices/coingecko.go`
- Test: `internal/prices/coingecko_test.go`, `internal/prices/fakes_test.go` (a `/cg` route)

**Interfaces:**
- Produces: `func (s *Service) HistoricalMint(ctx context.Context, mint string, day time.Time) (usd float64, ok bool, err error)`.
  - **Order:**
    1. `price_history` with asset `"spl:"+mint`.
    2. Days older than 365 days before `Now()`, or in the future, are `ok=false` with no request.
    3. The negative cache (`histBad` key `"cg:"+mint`, 6h).
    4. A singleflight fetch of the window `[day−150d, min(day+150d, yesterday)]`, clamped to the last 365 days.
    5. Store every returned day.
    6. Answer from the stored day.
  - Today's date uses the newest point in the response.
  - A 404, or an empty `prices` array, marks the mint unknown.
  - A 429 or 5xx is an error, which `httpx` retries. Nothing is cached.

- [ ] **Step 1: Write failing tests** (fake route `/cg/coins/solana/contract/{mint}/market_chart/range` serving a `prices` series keyed by day, and recording headers and the call count)

```go
func TestHistoricalMintFetchesAndStores(t *testing.T) // fake has mint M at 2026-05-01 = 0.42; HistoricalMint(M, 2026-05-01) = 0.42 ok; second call makes no request (DB hit)
func TestHistoricalMintKeyHeader(t *testing.T)        // CoinGeckoKey returns "k1" → request header x-cg-demo-api-key == "k1"
func TestHistoricalMintNoKeyHeader(t *testing.T)      // key "" → header absent; still 0.42 ok
func TestHistoricalMintUnknownCached(t *testing.T)    // 404 → ok false; a second call within 6h makes no request
func TestHistoricalMintTooOld(t *testing.T)           // day = Now − 400d → ok false, zero requests
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/prices/ -run TestHistoricalMint -v`
Expected: FAIL.

- [ ] **Step 3: Implement `coingecko.go`.** Follow `history.go`'s `histFetch`, `histStore` and `histKnownMissing` patterns; don't duplicate their logic blocks, factor them out where they repeat.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/prices/ -v`
Expected: PASS.

---

### Task 5: The valuer uses mints, and revalue fills old rows

**Files:**
- Modify: `internal/activity/syncer.go` (`Valuer`, `valueCache.fill` at :408-426, `SyncOnce`)
- Test: `internal/activity/syncer_test.go`

**Interfaces:**
- Consumes: Task 4 `HistoricalMint`.
- Produces:
  - `type MintValuer interface { HistoricalMint(ctx context.Context, mint string, day time.Time) (float64, bool, error) }`, an optional interface checked by a type assertion on `Syncer.Valuer`, so existing fakes keep compiling.
  - `valueCache.fill`:
    - A leg without USD keeps the old symbol path first; SPL keys skip it.
    - If that yields nothing and `Leg.Mint != ""` and the valuer is a `MintValuer`, it uses `HistoricalMint(mint, day)`.
    - The `Tx.Fee` leg follows the same path.
    - The cache key for mint lookups is `"mint:"+mint+"|"+day`.
  - `func (s *Syncer) revalue(ctx context.Context) error`:
    - Called at the end of `SyncOnce`.
    - Loads txs from `Store.All` with `Time >= since`, where any leg or fee has `USD == nil` and is priceable (not fiat, has a symbol or mint).
    - Fills them through the same `valueCache`, and upserts only the txs that gained a value.
    - Logs a count.

- [ ] **Step 1: Write failing tests**

```go
func TestValuerFallsBackToMint(t *testing.T)   // leg {AssetKey "spl:M", Mint "M", USD nil}; fake MintValuer M@day = 0.5 → USD = amount×0.5 (signed like Amount)
func TestValuerSymbolFirst(t *testing.T)       // leg JUP with Mint set; symbol valuer returns 0.8 → mint valuer not called
func TestRevalueFillsOnlyMissing(t *testing.T) // stored tx: leg A USD 10, leg spl:M USD nil → after SyncOnce, A still 10, M filled; a tx with nothing missing is not re-upserted (Upsert count)
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/activity/ -run 'TestValuer|TestRevalue' -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/activity/ -v`
Expected: PASS.

---

### Task 6: CoinGecko key in Settings, and wiring

**Files:**
- Modify:
  - `internal/config/config.go` (env `COINGECKO_API_KEY` → `CoinGeckoKey string yaml:"-"`)
  - `main.go` (`prices.Options.CoinGeckoKey` closure: env key if set, else the kv value opened with the box, cached for 1 minute)
  - `internal/server/server.go` (route `r.Post("/settings/prices", s.settingsPrices)`)
  - `internal/server/settings_handlers.go`
  - `web/templates/settings.templ`, `web/templates/models.go` (`SettingsPage.PriceKey string`, masked; `PriceKeyFromEnv bool`)
  - `README.md` (one line under configuration)
- Test: `internal/server/server_test.go`

**Interfaces:**
- `POST /settings/prices`, form fields:
  - `coingeckoKey`: a non-empty value saves `box.Seal(key)` under kv `prices.coingeckoKey`.
  - `clear=1`: deletes the key.
- It replies with an SSE patch of `templates.SettingsPrices(p)`, a card with id `settings-prices` placed after `SettingsDisplay`. The card shows:
  - "CoinGecko key: ••••abcd", "not set", or "set by COINGECKO_API_KEY" (in which case the form is disabled)
  - one password input
  - Save and Clear buttons
  - the help line "Free demo key from coingecko.com/en/developers. Used for historical prices of Solana tokens Coinbase doesn't list (last 365 days)."
- The handler follows `settingsLogos` (`settings_handlers.go:66-80`) and passes the guard.

- [ ] **Step 1: Write failing tests**

```go
func TestSettingsPricesSaveSealed(t *testing.T) // POST coingeckoKey=abc123 → kv value IsSealed, opens to "abc123"; response contains "••••c123" (secrets.Mask)
func TestSettingsPricesClear(t *testing.T)      // POST clear=1 → kv key absent; response contains "not set"
func TestSettingsPricesNotSet(t *testing.T)     // GET /settings → contains id="settings-prices" and "not set"
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/server/ -run TestSettingsPrices -v`
Expected: FAIL.

- [ ] **Step 3: Implement.** Run `go tool templ generate`.

- [ ] **Step 4: Run the gate**

Run: `task generate && task vet && task test`
Expected: PASS, with empty vet output.
