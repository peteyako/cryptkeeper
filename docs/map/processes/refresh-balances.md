---
type: process
universe: live
status: verified
verified: 2026-10-20 @ 73e5bf4
consumes: [source-def, source-contract, price, database]
produces: [snapshot, value-history, price, database]
---

# refresh-balances

Every few minutes (or on demand) fetch every connected source in parallel, price what came back, keep the last good result for sources that failed, and push the new snapshot to open pages. In between, prices alone are refreshed every minute by default (`priceMinutes`, or the Settings choice) without fetching any wallet.

## Input → Movement → Output

Input: the registry's live sources (`a.wallets`), a timer tick, a `Kick()` from a registry change, a forced `/refresh`, or (prices only) the price ticker and the prices chip's `/refresh/prices`. Movement: `Aggregator.BuildProgress` fetches all wallets concurrently and prices their assets alongside the fetches, publishing a `Warm` progress snapshot as wallets finish; when the last one answers it prices only the assets it had not asked for yet, and `assemble` turns results plus prices into the finished `Snapshot`. The `Refresher` stores it as latest, records history, publishes it and fires the alerts hook. A `Kick()` cancels the build in progress, which then publishes nothing. Between refreshes the price ticker (`priceMinutes`, default 1, or the Settings choice) calls `reprice`: `Aggregator.Reprice` prices the last finished build's balances again with no wallet fetch, and the result is published like a refresh. Output: a stream of in-memory `Snapshot`s (progress, then the finished one), a possibly-new `history` row, and a Datastar patch of the dashboard on every open `/stream` for each.

## Why this shape

- Pages read `Refresher.Latest()` and never wait on slow APIs; only the refresher (and the add-source Test button) calls `Fetch`.
- Last-good fallback (`lastgood_wallet`): one flaky RPC or exchange must not zero a wallet's value and spike the chart. A failed fetch within `StaleMax` serves the cached result flagged `Stale`; beyond it, the wallet is `StatusError` and contributes nothing.
- `singleflight` plus detached 2-minute context: a double-clicked Refresh or a closed tab cannot start two builds or cancel one halfway. Only a `Kick()` stops a build, because its result is out of date: a generation counter (`gen`) makes the replaced build publish nothing, and the new one starts at once instead of waiting for its slow wallets.
- Progress snapshots: one slow wallet must not hold back the rest. A wallet still fetching shows what it showed before (`known`: the last finished build's result, else its last-good row, else nothing, `pending`), so totals do not dip while it answers.
- Early prices: the assets held before are priced at build start, in parallel with the fetches, so live prices do not wait for the slowest wallet. At the end only assets that were not in that request are priced, so nothing is asked for twice.
- `Broker` holds one pending snapshot per subscriber: a slow tab gets the latest, never a backlog, which matters now that a refresh publishes several.
- Reprice: prices move faster than balances, so a refresh every 5 minutes (the default `refreshMinutes`) would leave the header's prices old. A reprice reuses the last build's wallets (`known`), so a failing wallet is not retried, and it does not advance `lastSuccess` (it is not a sync). It gives way to a refresh: it does not start while one runs, and its result is dropped if a build finished or a kick came while it priced (the `builds` and `gen` counters). Snapshots carry `SyncedAt` (when balances were fetched; a reprice keeps it) and `PricedAt` (when prices came in), which feed the header's two chips.
- Warm start: after a restart the first paint uses stored last-good results and cached prices, so it is not blank for the length of a full fetch. It is deliberately not recorded or alerted on; neither are progress snapshots (both are `Warm`).

## Steps

1. `serve` builds `History` (30 min spacing; rows older than 30 days compacted to one per UTC day at startup), `Broker`, `Aggregator` (fetch timeout 45s, `StaleMax` from config) and `Refresher` with `Interval` and `PriceInterval` from config (the server then applies the Settings overrides, step 18): `main.go:473-488`.
2. `OnSnapshot` is wired to `engine.OnSnapshot` in a goroutine so notifications never block refreshes: `main.go:491-493`. `registry.OnChange` kicks the refresher (and syncer): `main.go:509`.
3. `go ref.Run(ctx)` starts: `main.go:510`. `Run` first publishes the warm snapshot built from last-good rows and cached prices, if any: `internal/portfolio/refresher.go:58-67`, `internal/portfolio/aggregate.go:274-298`.
4. The loop calls `refresh`, then waits for ticker, kick or ctx: `internal/portfolio/refresher.go:95-108`. Forced refresh comes from `POST /refresh` via `Force`, which returns `ErrTooSoon` inside `MinForceInterval` (30s); `Rebuild` runs a refresh whatever that says: `internal/portfolio/refresher.go:189-207`, `internal/server/handlers.go:289-300`.
5. `refresh` joins any in-flight build via singleflight and runs `Agg.BuildProgress` under a 2 minute context detached from the caller, remembering the current generation and its cancel func: `internal/portfolio/refresher.go:259-269`. `Kick()` bumps the generation and cancels the build in progress, so a replaced build publishes nothing and returns the latest snapshot instead: `internal/portfolio/refresher.go:154-169`, `:270-277`, `:285-294`.
6. `BuildProgress` takes the current wallet list, seeds each wallet with `known` (last finished build, else last-good row, else empty `pending`), and starts one goroutine per wallet: `internal/portfolio/aggregate.go:90-106`, `:212-223`.
7. `fetchOne`: a nil `Source` becomes an error row carrying `BuildErr`; otherwise `Fetch` runs under a per-wallet timeout through `safeFetch`, which turns a panic into an error: `internal/portfolio/aggregate.go:300-314`, `:331-339`.
8. On success the result is saved to `lastgood_wallet` and marked `StatusSynced`: `internal/portfolio/aggregate.go:316-321`, `:341-351`. On error a last-good row younger than `StaleMax` is returned `Stale` with the error text (`StatusStale`); else `StatusError`: `internal/portfolio/aggregate.go:323-328`.
9. Alongside the fetches, `PriceRequests` dedupes the assets already known (exchange symbols first, then named SPL tokens, then unnamed mints) and `a.Prices` is called once for them in a goroutine: `internal/portfolio/aggregate.go:107-115`, `:370-417`. That is `prices.Service.Get`: in-memory cache, live fetch (fresh prices upserted into `lastgood_price`), stablecoin peg, stored last-good price: `main.go:313-317`, `internal/prices/prices.go:182-226`, `:444-458`.
10. Progress: `progress` gets a `Warm` snapshot once at the start, then after each batch of finished wallets and when the early prices arrive. `inProgress` prices it without network calls: this build's early prices, else the last finished build's, else `CachedPrices`. The refresher publishes it while its generation is still current: `internal/portfolio/aggregate.go:117-143`, `:228-254`, `internal/portfolio/refresher.go:269-278`.
11. After the last wallet answers, a cancelled build returns unpriced and is dropped. Otherwise it waits for the early prices if still pending, then asks `a.Prices` only for assets not in the early request (`rest`); `remember` keeps the wallets, prices and `SyncedAt`/`PricedAt` for the next build's `known` and `inProgress` and for `Reprice`; the finished snapshot gets the same two times: `internal/portfolio/aggregate.go:145-171`, `:258-268`.
12. `assemble` prices each holding, moves spammy Solana tokens to `HiddenRows` when `HideUnpriced` is set, and `FromWallets` totals wallets, merges assets across wallets, sets `HasStale`: `internal/portfolio/aggregate.go:169`, `:420-505`.
13. `refresh` stores the finished snapshot as `latest`, advances `lastSuccess` when any wallet synced (or there are no wallets), and logs: `internal/portfolio/refresher.go:279-302`.
14. `History.Record` writes a `history` row (prices + per-wallet amounts, skipping errored wallets) only if 30 minutes passed since the last row: `internal/portfolio/refresher.go:303-307`, `internal/portfolio/history.go:29-71`.
15. `Broker.Publish` hands the snapshot to each subscriber channel, replacing any undelivered one: `internal/portfolio/refresher.go:308-310`, `internal/portfolio/broker.go:32-45`.
16. `OnSnapshot` fires the alert engine (see [evaluate-alerts](evaluate-alerts.md)): `internal/portfolio/refresher.go:311-313`.
17. Only the overview page holds the hidden `LiveStream` element that opens `GET /stream` (cancelled when the element leaves the page); the handler subscribes, patches `Dashboard` immediately if a snapshot exists, re-renders it per published snapshot, and sends a keep-alive every 25s: `web/templates/overview.templ:15-18`, `web/templates/layout.templ:209-212`, `web/templates/js.go:93`, `internal/server/handlers.go:255-287`.
18. Tickers: the balance interval is `refreshMinutes` and the price interval `priceMinutes` from config, unless the user picked others in Settings (`refreshMinutes` and `priceMinutes` in the database `kv` key `ui.settings`; balances at least 1 minute, prices 0 meaning "with the balances"; `Server.intervals`, which also holds prices to at most the balance interval; the same save carries `activityMinutes` too, which `applyIntervals` hands to the activity syncer, see [sync-activity](sync-activity.md)). `RefreshEvery()` is `Interval`; when `PriceEvery()` (`PriceInterval`, never above `Interval`) is shorter, `Run` also ticks on it and starts `reprice` in a goroutine, so it never delays a kick. `SetIntervals(balances, prices)` changes both while running (a balance value of 0 keeps the current one) and signals `Run` (`intervalSet`) to restart both tickers (`startTickers`). The server applies the saved values when it starts, after `POST /settings/intervals`, after a database restore and after a reset (back to config.yaml's): `internal/config/config.go:214-218`, `main.go:488`, `internal/portfolio/refresher.go:68-87`, `:103-106`, `:111-149`, `internal/server/settings_handlers.go:52-84`, `:132-163`, `internal/server/server.go:97-99`, `:190`.
19. `reprice` joins any in-flight reprice via singleflight and skips when a build runs, nothing has finished yet, the latest is `Warm`, or (unless forced) the prices are younger than half a `PriceEvery`. It calls `Agg.Reprice` under a 1 minute context and drops the result if `latest`, `gen` or the running build changed meanwhile. Otherwise it stores it as `latest`, publishes it, records history and fires `OnSnapshot`: `internal/portfolio/refresher.go:218-255`.
20. `Aggregator.Reprice` rebuilds each wallet from `known`, prices their assets with `a.Prices`, and assembles the snapshot with the build's `SyncedAt` and a new `PricedAt`. It returns false before the first finished build, on cancellation, or when `builds` changed (a build finished meanwhile): `internal/portfolio/aggregate.go:178-208`.
21. The prices chip posts `POST /refresh/prices`: the handler shows the chip as updating, calls `Refresher.RepriceNow` (a forced `reprice`, without the half-interval floor) and patches the chips, plus the dashboard if it repriced: `web/templates/js.go:111-112`, `web/templates/components.templ:14-19`, `internal/portfolio/refresher.go:209-212`, `internal/server/handlers.go:302-316`.
22. The header has two chips, patched together as `#sync-status`: `priceSync` shows `PricedAt` (amber for stale prices or after three `PriceEvery` intervals), `walletSync` shows pending, failing or stale sources and otherwise `SyncedAt`. `Every` words each interval in the chip tooltip (the wallets chip from `RefreshEvery()`, the prices chip from `PriceEvery()`): `internal/server/handlers.go:78-139`, `web/templates/models.go:28-38`, `web/templates/errors_view.go:91-103`.

## If you change this

- **Hits:** [snapshot](../objects/valuation/snapshot.md) shape (every page model reading `Refresher.Latest()`), `lastgood_wallet` table in [database](../objects/storage/database.md), [value-history](../objects/valuation/value-history.md) recording, alert evaluation on `OnSnapshot`, the `/healthz` freshness check that reads `LastSuccess` against 3 `RefreshEvery()` intervals, so a Settings change moves it (`internal/server/guard.go:72-84`), `Dashboard` patch contents. Tax reads the snapshot too ([compute-taxes](compute-taxes.md)): its quiet set is built from `HiddenRows`, a `Warm` or missing snapshot blocks caching and closing a year, and reports price today from `Assets` (`internal/server/tax.go:131-142`, `:370-384`, `internal/server/taxview.go:80-85`). [backfill-history](backfill-history.md) rolls back from it and refuses on `Warm`, errored or pending wallets.
- **Does not hit:** activity sync ([sync-activity](sync-activity.md) has its own syncer and schedule); the daily price closes in `price_history` ([backfill-history](backfill-history.md)); tax lots themselves, which are built from activity, not from balances.

## Surfaces

| Surface | Role |
|---|---|
| `GET /stream` | persistent SSE from the overview page; patches `Dashboard` on each snapshot |
| `POST /refresh` | forced refresh button and the wallets chip; 30s floor, answers with a chip or full dashboard patch |
| `POST /settings/intervals` | the Settings "Refresh" card (balances, prices and activity selects); saves the choices, restarts both refresh tickers and resets the activity syncer's ([sync-activity](sync-activity.md)) |
| `POST /refresh/prices` | the prices chip; reprices the last balances, no wallet fetch, no floor; patches the chips and, when it repriced, the dashboard |
| `GET /portfolio.json` | raw latest snapshot |
| Timer / `registry.OnChange` | interval tick, price tick and kick after source add, edit or delete |
| `/healthz` | 200 if a refresh succeeded within 3 `RefreshEvery()` intervals |

## See

- Objects: [snapshot](../objects/valuation/snapshot.md), [price](../objects/valuation/price.md), [value-history](../objects/valuation/value-history.md), [source-contract](../objects/sources/source-contract.md), [source-def](../objects/sources/source-def.md), [database](../objects/storage/database.md)
- Source: `internal/portfolio/refresher.go`, `internal/portfolio/aggregate.go`, `internal/portfolio/broker.go`
