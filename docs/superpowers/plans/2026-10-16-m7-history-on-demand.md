# M7: On-demand coin history (value-chart backfill and per-coin price charts) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The user asked for coin history to load on demand from CoinGecko or similar, for example "load the latest 7 or 30 days". This plan covers two things, both triggered from the chart range control:
1. **Value-chart backfill.** When the selected range (1W, 1M, 1Y) has no history yet, the chart offers "Load last 7 / 30 / 365 days".
   - Doing so writes past daily `history` rows.
   - Prices come from Coinbase daily candles, or CoinGecko by mint.
   - Holdings are rolled back through activity.
2. **Per-coin price charts.** A coin page `/a/{asset}` has its own range control (7D, 30D, 90D, 1Y). It fetches that coin's daily prices on demand. This is display only.

**Architecture:**
- **Prices:** `prices.Service.Series` builds a daily USD series for an asset from the existing `Historical` / `HistoricalMint`. Those are DB-backed (`price_history`) and fetch 300-day windows, so a 30-day series costs at most one request per asset.
- **Backfill:** `portfolio.History.Backfill` takes these inputs:
  - the current snapshot
  - activity, used to roll back per-wallet amounts day by day (the same rules as `tax.RollBack`: failed txs count the fee only; stake and position txs are zero-net)
  - a price function

  It inserts one row per UTC day at 00:00 for days that have no row yet. It never overwrites a recorded row.
- **Server:** `POST /chart/backfill` runs the backfill and re-patches `#value-card`. `GET /a/{asset}` and `POST /a/{asset}/chart` render the coin page.

**Tech Stack:** Go 1.26, templ + Datastar, modernc sqlite.

**Spec:** the user's request in conversation (2026-10-16): "for coin history, make it so it can load some from coin gecko or similar on demand. for example: load latest 7 or 30 days, etc". Answers: "Both", and the trigger is "Chart range control".

## Global Constraints

- Backfill only fills days with no existing row: `INSERT OR IGNORE`, keyed by `ts` = 23:59:59 UTC of the day (end-of-day amounts × that day's close), and only for days strictly before the earliest existing row's UTC day, or when the table is empty. Recorded history is never modified.
- Day count per range: `1W → 7`, `1M → 30`, `1Y → 365`. The offer is shown only when `portfolio.ChangeFor(range, ...)` is not `Valid`. "1D" and "All" never show it.
- **Price fallback:** if an asset has no historical price for a day (unlisted, or older than CoinGecko's 365-day demo limit), the row uses the asset's current snapshot price. This is a documented estimate: the asset contributes a flat line, never a cliff. Pegged stablecoins are 1.
- **Per-wallet amounts for day D:** current amount minus the signed effect of every tx with `Time >= D+1d` (00:00 UTC). Amounts that come out negative are clamped to 0. Only wallets present in the current snapshot are written.
- **Coin page ranges:** `7D → 7`, `30D → 30`, `90D → 90`, `1Y → 365` days. Daily points, with today's point = the current price. An empty series shows "No price history for <SYM>".
- No new dependencies. Requests are bounded by the `price_history` cache. The backfill handler has a 60 s context timeout and replies with an error card on timeout.
- Uses `Deps.Historical` (introduced in M5 Task 2). If M5 isn't present when this runs, add it here with the same interface.
- Do NOT `git add`, `git commit`, `git stash` or switch branches.
- Gate: `task generate && task vet && task test`.

## Review Focus

1. **Backfill run twice.** The second run inserts nothing, and the chart is unchanged. Test: Task 2 `TestBackfillIdempotent`.
2. **Recorded rows inside the window are not overwritten.** Test: Task 2 `TestBackfillKeepsRecordedRows`.
3. **An asset with no historical price.** It uses the current price, so the series has no dip to zero. Test: Task 2 `TestBackfillPriceFallback`.
4. **A source added today.** Its wallet is included in backfilled rows, so `Series` starts at the backfilled range and not at "now" only. Test: Task 2 `TestBackfillMakesSeriesStartEarlier`.
5. **A coin with an `spl:` key.** It is priced by mint through `HistoricalMint`. Test: Task 1 `TestSeriesUsesMintForSPL`.

---

### Task 1: `prices.Service.Series`

**Files:**
- Create: `internal/prices/series.go`
- Test: `internal/prices/series_test.go`

**Interfaces:**
- Produces: `func (s *Service) Series(ctx context.Context, assetKey, mint string, days int) ([]Point, error)`, where `type Point struct { Day time.Time; USD float64 }` (Day = 00:00 UTC).
  - It covers the days from `today − days + 1` to today, ascending.
  - A pegged asset gives 1 for every day.
  - For an `spl:` key, or when `Historical` says not ok and `mint != ""`, it uses `HistoricalMint(mint, day)`.
  - Days with no price are omitted.
  - Errors from individual days are logged and skipped. Only a context error is returned.

- [ ] **Step 1: Write failing tests** (reuse `fakes_test.go`)

```go
func TestSeriesDailyFromCandles(t *testing.T) // BTC candles for the last 10 days; Series("BTC","",7) → 7 points ascending, last = today's (live candle)
func TestSeriesUsesMintForSPL(t *testing.T)   // "spl:M" with CoinGecko fake series → points from HistoricalMint
func TestSeriesPegged(t *testing.T)           // "USDC" → 7 points of 1
func TestSeriesSkipsMissingDays(t *testing.T) // candles missing 2 days → 5 points
```

- [ ] **Step 2–4:** `go test ./internal/prices/ -run TestSeries -v` → FAIL; implement; PASS.

---

### Task 2: `portfolio.History.Backfill`

**Files:**
- Create: `internal/portfolio/backfill.go`
- Test: `internal/portfolio/backfill_test.go`

**Interfaces:**
- Produces:

```go
type PriceAt func(ctx context.Context, assetKey, mint string, day time.Time) (float64, bool)
func (h *History) Backfill(ctx context.Context, days int, now time.Time, current Snapshot, txs []core.Tx, price PriceAt) (inserted int, err error)
```

  - Mints come from the snapshot rows (`HoldingRow.Mint`).
  - The rollback rules are in the Global Constraints. Txs are grouped by `SourceID`, and only wallets in `current` are used.
  - Prices: `price(...)`, else the current snapshot price.

- [ ] **Step 1: Write failing tests** (temp `store.DB` as other portfolio tests do)

```go
func TestBackfillWritesDailyRows(t *testing.T)    // empty history, days 7 → 7 rows at 00:00 UTC; amounts = current (no txs)
func TestBackfillRollsBackActivity(t *testing.T)  // wallet w SOL 10 now; receive +3 two days ago → rows older than that have SOL 7
func TestBackfillIdempotent(t *testing.T)         // run twice → second inserted 0
func TestBackfillKeepsRecordedRows(t *testing.T)  // existing row 3 days ago → only days before it are written; that row unchanged
func TestBackfillPriceFallback(t *testing.T)      // price func false for spl:X → row price = snapshot price
func TestBackfillMakesSeriesStartEarlier(t *testing.T) // after backfill, Series(from now−7d) returns ≥ 7 points
```

- [ ] **Step 2–4:** `go test ./internal/portfolio/ -run TestBackfill -v` → FAIL; implement; PASS.

---

### Task 3: "Load last N days" on the value chart

**Files:**
- Modify:
  - `web/templates/overview.templ` (`ValueCard`: when `!p.Change.Valid && p.Range in {1W,1M,1Y}`, show the offer `Load last <N> days` under the chart, posting to `/chart/backfill` with `range`, `pf` and the loading indicator)
  - `web/templates/models.go` (`OverviewPage.BackfillDays int`, 0 = no offer)
  - `internal/server/handlers.go` (`overviewPage` sets `BackfillDays`)
  - `internal/server/server.go` (route `POST /chart/backfill`)
  - new handler `s.chartBackfill` in `internal/server/history_handlers.go`
- Test: `internal/server/history_test.go`

**Interfaces:**
- Consumes: Task 2 `Backfill`, `Deps.Historical` (`Historical` and `HistoricalMint`), `Activity.All`, `s.latest()`.
- The handler:
  1. Runs `Backfill(ctx with 60s timeout, days, now, snap, txs, priceAt)`.
  2. Re-renders `templates.ValueCard(s.overviewPage(r, v))` via SSE.
  3. On error or timeout, patches the card with the note `Couldn't load history: <err>`.

- [ ] **Step 1: Write failing tests**

```go
func TestValueCardOffersBackfill(t *testing.T)  // empty history, range 1M → page contains "Load last 30 days"
func TestValueCardNoOfferWhenValid(t *testing.T) // history covering 1W → no offer
func TestChartBackfillPatches(t *testing.T)     // POST /chart/backfill range=1W with fake Historical → history has 7 rows; response contains id="value-card"
```

- [ ] **Step 2–4:** `go test ./internal/server/ -run 'TestValueCard|TestChartBackfill' -v` → FAIL; implement (`go tool templ generate`); PASS.

---

### Task 4: Coin page with an on-demand price chart

**Files:**
- Create: `web/templates/coin.templ`, `internal/server/coin_handlers.go`
- Modify:
  - `internal/server/server.go` (routes `GET /a/{asset}` and `POST /a/{asset}/chart`)
  - `web/templates/overview.templ` and the wallet holdings rows (link the asset name to `/a/{asset}`, URL-escaped)
  - `web/templates/models.go` (`CoinPage{Shell; AssetKey, Symbol, Name, LogoURL string; Price *core.Price; Amount, ValueUSD float64; Wallets []CoinWallet{ID, Label string; Amount, ValueUSD float64}; Range string; Points []portfolio.Point; Change portfolio.Change}`)
- Test: `internal/server/coin_test.go`

**Interfaces:**
- `GET /a/{asset}` renders a full page:
  - header (logo, symbol, name, current price, 24h change if known)
  - holdings across wallets
  - the price chart, with id `coin-chart`, default range `30D`, and a range control `7D/30D/90D/1Y`
- `POST /a/{asset}/chart` (field `range`) loads `Series` on demand and patches `#coin-chart`. The `chartSVG` component is reused with a label like "SOL price, past 30 days".
- An unknown asset returns 404.

- [ ] **Step 1: Write failing tests**

```go
func TestCoinPage(t *testing.T)        // snapshot with SOL in two wallets → page shows both wallets and id="coin-chart"
func TestCoinChartRange(t *testing.T)  // POST /a/SOL/chart range=7D with fake Historical → patch has a path element and label "past 7 days"
func TestCoinUnknown404(t *testing.T)
func TestAssetLinksToCoin(t *testing.T) // overview HTML has href="/a/SOL"
```

- [ ] **Step 2–4:** `go test ./internal/server/ -run 'TestCoin|TestAssetLinks' -v` → FAIL; implement; then `task generate && task vet && task test` → PASS.
