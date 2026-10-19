---
type: process
universe: live
status: verified
verified: 2026-10-19 @ d4899f7
consumes: [source-def, source-contract, price, database]
produces: [snapshot, value-history, price, database]
---

# refresh-balances

Every few minutes (or on demand) fetch every connected source in parallel, price what came back, keep the last good result for sources that failed, and push the new snapshot to open pages.

## Input → Movement → Output

Input: the registry's live sources (`a.wallets`), a timer tick, a `Kick()` from a registry change, or a forced `/refresh`. Movement: `Aggregator.Build` fetches all wallets concurrently, prices the distinct assets once, and `assemble` turns results plus prices into a `Snapshot`; the `Refresher` then stores it as latest, records history, publishes it and fires the alerts hook. Output: a new in-memory `Snapshot`, a possibly-new `history` row, and a Datastar patch of the dashboard on every open `/stream`.

## Why this shape

- Pages read `Refresher.Latest()` and never wait on slow APIs; only the refresher (and the add-source Test button) calls `Fetch`.
- Last-good fallback (`lastgood_wallet`): one flaky RPC or exchange must not zero a wallet's value and spike the chart. A failed fetch within `StaleMax` serves the cached result flagged `Stale`; beyond it, the wallet is `StatusError` and contributes nothing.
- `singleflight` plus detached 2-minute context: a double-clicked Refresh or a closed tab cannot start two builds or cancel one halfway.
- `Broker` holds one pending snapshot per subscriber: a slow tab gets the latest, never a backlog.
- Warm start: after a restart the first paint uses stored last-good results and cached prices, so it is not blank for the length of a full fetch. It is deliberately not recorded or alerted on.

## Steps

1. `serve` builds `History` (30 min spacing; rows older than 30 days compacted to one per UTC day at startup), `Broker`, `Aggregator` (fetch timeout 45s, `StaleMax` from config) and `Refresher` with `Interval` from config: `main.go:473-488`.
2. `OnSnapshot` is wired to `engine.OnSnapshot` in a goroutine so notifications never block refreshes: `main.go:491-493`. `registry.OnChange` kicks the refresher (and syncer): `main.go:509`.
3. `go ref.Run(ctx)` starts: `main.go:510`. `Run` first publishes the warm snapshot built from last-good rows and cached prices, if any: `internal/portfolio/refresher.go:47-56`, `internal/portfolio/aggregate.go:80-104`.
4. The loop calls `refresh`, then waits for ticker, kick or ctx: `internal/portfolio/refresher.go:57-67`. Forced refresh comes from `POST /refresh` via `Force`, which returns `ErrTooSoon` inside `MinForceInterval` (30s): `internal/portfolio/refresher.go:102-116`, `internal/server/handlers.go:252-263`.
5. `refresh` joins any in-flight build via singleflight and runs `Agg.Build` under a 2 minute context detached from the caller: `internal/portfolio/refresher.go:119-125`.
6. `Build` takes the current wallet list and starts one goroutine per wallet: `internal/portfolio/aggregate.go:59-66`.
7. `fetchOne`: a nil `Source` becomes an error row carrying `BuildErr`; otherwise `Fetch` runs under a per-wallet timeout through `safeFetch`, which turns a panic into an error: `internal/portfolio/aggregate.go:106-120`, `:137-145`.
8. On success the result is saved to `lastgood_wallet` and marked `StatusSynced`: `internal/portfolio/aggregate.go:122-127`, `:147-157`. On error a last-good row younger than `StaleMax` is returned `Stale` with the error text (`StatusStale`); else `StatusError`: `internal/portfolio/aggregate.go:129-134`.
9. After all fetches finish, `PriceRequests` dedupes assets (exchange symbols first, then named SPL tokens, then unnamed mints) and `a.Prices` is called once: `internal/portfolio/aggregate.go:68-72`, `:176-222`. That is `prices.Service.Get`: in-memory cache, live fetch (fresh prices upserted into `lastgood_price`), stablecoin peg, stored last-good price: `main.go:313-317`, `internal/prices/prices.go:182-226`, `:444-458`.
10. `assemble` prices each holding, moves spammy Solana tokens to `HiddenRows` when `HideUnpriced` is set, and `FromWallets` totals wallets, merges assets across wallets, sets `HasStale`: `internal/portfolio/aggregate.go:226-311`.
11. `refresh` stores the snapshot as `latest`, advances `lastSuccess` when any wallet synced (or there are no wallets), and logs: `internal/portfolio/refresher.go:126-140`.
12. `History.Record` writes a `history` row (prices + per-wallet amounts, skipping errored wallets) only if 30 minutes passed since the last row: `internal/portfolio/refresher.go:141-145`, `internal/portfolio/history.go:29-71`.
13. `Broker.Publish` hands the snapshot to each subscriber channel, replacing any undelivered one: `internal/portfolio/refresher.go:146-148`, `internal/portfolio/broker.go:32-45`.
14. `OnSnapshot` fires the alert engine (see [evaluate-alerts](evaluate-alerts.md)): `internal/portfolio/refresher.go:149-151`.
15. Only the overview page holds the hidden `LiveStream` element that opens `GET /stream` (cancelled when the element leaves the page); the handler subscribes, patches `Dashboard` immediately if a snapshot exists, re-renders it per published snapshot, and sends a keep-alive every 25s: `web/templates/overview.templ:15-18`, `web/templates/layout.templ:209-212`, `web/templates/js.go:93`, `internal/server/handlers.go:220-250`.

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
