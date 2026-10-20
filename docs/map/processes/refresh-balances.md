---
type: process
universe: live
status: verified
verified: 2026-10-20 @ a83d5cd
consumes: [source-def, source-contract, price, database]
produces: [snapshot, value-history, price, database]
---

# refresh-balances

Every few minutes (or on demand) fetch every connected source in parallel, price what came back, keep the last good result for sources that failed, and push the new snapshot to open pages.

## Input → Movement → Output

Input: the registry's live sources (`a.wallets`), a timer tick, a `Kick()` from a registry change, or a forced `/refresh`. Movement: `Aggregator.BuildProgress` fetches all wallets concurrently and prices their assets alongside the fetches, publishing a `Warm` progress snapshot as wallets finish; when the last one answers it prices only the assets it had not asked for yet, and `assemble` turns results plus prices into the finished `Snapshot`. The `Refresher` stores it as latest, records history, publishes it and fires the alerts hook. A `Kick()` cancels the build in progress, which then publishes nothing. Output: a stream of in-memory `Snapshot`s (progress, then the finished one), a possibly-new `history` row, and a Datastar patch of the dashboard on every open `/stream` for each.

## Why this shape

- Pages read `Refresher.Latest()` and never wait on slow APIs; only the refresher (and the add-source Test button) calls `Fetch`.
- Last-good fallback (`lastgood_wallet`): one flaky RPC or exchange must not zero a wallet's value and spike the chart. A failed fetch within `StaleMax` serves the cached result flagged `Stale`; beyond it, the wallet is `StatusError` and contributes nothing.
- `singleflight` plus detached 2-minute context: a double-clicked Refresh or a closed tab cannot start two builds or cancel one halfway. Only a `Kick()` stops a build, because its result is out of date: a generation counter (`gen`) makes the replaced build publish nothing, and the new one starts at once instead of waiting for its slow wallets.
- Progress snapshots: one slow wallet must not hold back the rest. A wallet still fetching shows what it showed before (`known`: the last finished build's result, else its last-good row, else nothing, `pending`), so totals do not dip while it answers.
- Early prices: the assets held before are priced at build start, in parallel with the fetches, so live prices do not wait for the slowest wallet. At the end only assets that were not in that request are priced, so nothing is asked for twice.
- `Broker` holds one pending snapshot per subscriber: a slow tab gets the latest, never a backlog, which matters now that a refresh publishes several.
- Warm start: after a restart the first paint uses stored last-good results and cached prices, so it is not blank for the length of a full fetch. It is deliberately not recorded or alerted on; neither are progress snapshots (both are `Warm`).

## Steps

1. `serve` builds `History` (30 min spacing; rows older than 30 days compacted to one per UTC day at startup), `Broker`, `Aggregator` (fetch timeout 45s, `StaleMax` from config) and `Refresher` with `Interval` from config: `main.go:473-488`.
2. `OnSnapshot` is wired to `engine.OnSnapshot` in a goroutine so notifications never block refreshes: `main.go:491-493`. `registry.OnChange` kicks the refresher (and syncer): `main.go:509`.
3. `go ref.Run(ctx)` starts: `main.go:510`. `Run` first publishes the warm snapshot built from last-good rows and cached prices, if any: `internal/portfolio/refresher.go:50-59`, `internal/portfolio/aggregate.go:216-240`.
4. The loop calls `refresh`, then waits for ticker, kick or ctx: `internal/portfolio/refresher.go:62-70`. Forced refresh comes from `POST /refresh` via `Force`, which returns `ErrTooSoon` inside `MinForceInterval` (30s); `Rebuild` runs a refresh whatever that says: `internal/portfolio/refresher.go:111-129`, `internal/server/handlers.go:254-265`.
5. `refresh` joins any in-flight build via singleflight and runs `Agg.BuildProgress` under a 2 minute context detached from the caller, remembering the current generation and its cancel func: `internal/portfolio/refresher.go:133-143`. `Kick()` bumps the generation and cancels the build in progress, so a replaced build publishes nothing and returns the latest snapshot instead: `internal/portfolio/refresher.go:76-91`, `:144-151`, `:159-168`.
6. `BuildProgress` takes the current wallet list, seeds each wallet with `known` (last finished build, else last-good row, else empty `pending`), and starts one goroutine per wallet: `internal/portfolio/aggregate.go:81-97`, `:161-172`.
7. `fetchOne`: a nil `Source` becomes an error row carrying `BuildErr`; otherwise `Fetch` runs under a per-wallet timeout through `safeFetch`, which turns a panic into an error: `internal/portfolio/aggregate.go:242-256`, `:273-281`.
8. On success the result is saved to `lastgood_wallet` and marked `StatusSynced`: `internal/portfolio/aggregate.go:258-263`, `:283-293`. On error a last-good row younger than `StaleMax` is returned `Stale` with the error text (`StatusStale`); else `StatusError`: `internal/portfolio/aggregate.go:265-270`.
9. Alongside the fetches, `PriceRequests` dedupes the assets already known (exchange symbols first, then named SPL tokens, then unnamed mints) and `a.Prices` is called once for them in a goroutine: `internal/portfolio/aggregate.go:98-103`, `:312-359`. That is `prices.Service.Get`: in-memory cache, live fetch (fresh prices upserted into `lastgood_price`), stablecoin peg, stored last-good price: `main.go:313-317`, `internal/prices/prices.go:182-226`, `:444-458`.
10. Progress: `progress` gets a `Warm` snapshot once at the start, then after each batch of finished wallets and when the early prices arrive. `inProgress` prices it without network calls: this build's early prices, else the last finished build's, else `CachedPrices`. The refresher publishes it while its generation is still current: `internal/portfolio/aggregate.go:104-131`, `:177-199`, `internal/portfolio/refresher.go:143-152`.
11. After the last wallet answers, a cancelled build returns unpriced and is dropped. Otherwise it waits for the early prices if still pending, then asks `a.Prices` only for assets not in the early request (`rest`); `remember` keeps the wallets and prices for the next build's `known` and `inProgress`: `internal/portfolio/aggregate.go:133-155`, `:202-210`.
12. `assemble` prices each holding, moves spammy Solana tokens to `HiddenRows` when `HideUnpriced` is set, and `FromWallets` totals wallets, merges assets across wallets, sets `HasStale`: `internal/portfolio/aggregate.go:156`, `:362-447`.
13. `refresh` stores the finished snapshot as `latest`, advances `lastSuccess` when any wallet synced (or there are no wallets), and logs: `internal/portfolio/refresher.go:153-176`.
14. `History.Record` writes a `history` row (prices + per-wallet amounts, skipping errored wallets) only if 30 minutes passed since the last row: `internal/portfolio/refresher.go:177-181`, `internal/portfolio/history.go:29-71`.
15. `Broker.Publish` hands the snapshot to each subscriber channel, replacing any undelivered one: `internal/portfolio/refresher.go:182-184`, `internal/portfolio/broker.go:32-45`.
16. `OnSnapshot` fires the alert engine (see [evaluate-alerts](evaluate-alerts.md)): `internal/portfolio/refresher.go:185-187`.
17. Only the overview page holds the hidden `LiveStream` element that opens `GET /stream` (cancelled when the element leaves the page); the handler subscribes, patches `Dashboard` immediately if a snapshot exists, re-renders it per published snapshot, and sends a keep-alive every 25s: `web/templates/overview.templ:15-18`, `web/templates/layout.templ:209-212`, `web/templates/js.go:93`, `internal/server/handlers.go:220-252`.

## If you change this

- **Hits:** [snapshot](../objects/valuation/snapshot.md) shape (every page model reading `Refresher.Latest()`), `lastgood_wallet` table in [database](../objects/storage/database.md), [value-history](../objects/valuation/value-history.md) recording, alert evaluation on `OnSnapshot`, the `/healthz` freshness check that reads `LastSuccess` (`internal/server/guard.go:72-84`), `Dashboard` patch contents. Tax reads the snapshot too ([compute-taxes](compute-taxes.md)): its quiet set is built from `HiddenRows`, a `Warm` or missing snapshot blocks caching and closing a year, and reports price today from `Assets` (`internal/server/tax.go:131-142`, `:370-384`, `internal/server/taxview.go:80-85`). [backfill-history](backfill-history.md) rolls back from it and refuses on `Warm`, errored or pending wallets.
- **Does not hit:** activity sync ([sync-activity](sync-activity.md) has its own syncer and schedule); the daily price closes in `price_history` ([backfill-history](backfill-history.md)); tax lots themselves, which are built from activity, not from balances.

## Surfaces

| Surface | Role |
|---|---|
| `GET /stream` | persistent SSE from the overview page; patches `Dashboard` on each snapshot |
| `POST /refresh` | forced refresh button; 30s floor, answers with a chip or full dashboard patch |
| `GET /portfolio.json` | raw latest snapshot |
| Timer / `registry.OnChange` | interval tick and kick after source add, edit or delete |
| `/healthz` | 200 if a refresh succeeded within 3 intervals |

## See

- Objects: [snapshot](../objects/valuation/snapshot.md), [price](../objects/valuation/price.md), [value-history](../objects/valuation/value-history.md), [source-contract](../objects/sources/source-contract.md), [source-def](../objects/sources/source-def.md), [database](../objects/storage/database.md)
- Source: `internal/portfolio/refresher.go`, `internal/portfolio/aggregate.go`, `internal/portfolio/broker.go`
