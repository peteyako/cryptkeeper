---
type: object
cluster: valuation
universe: live
status: verified
verified: 2026-10-20 @ a83d5cd
entity: internal/portfolio/types.go
---

# Snapshot

`portfolio.Snapshot` is the fully priced state of every connected source (or of one portfolio after filtering), built by the `Aggregator`, held and refreshed by the `Refresher`, and fanned out by the SSE `Broker`.

## Why this shape

Page loads must never wait on slow exchange or chain APIs, so the snapshot is a value built in the background and served from memory (`internal/portfolio/refresher.go:16-18`). One failing source must not blank the page, so each wallet falls back to its last good result and is labelled stale instead of dropped (`internal/portfolio/aggregate.go:265-268`). `assemble` is pure (wallets + results + prices in, snapshot out) so portfolio filtering can rebuild totals from a subset of rows with the same code (`internal/portfolio/aggregate.go:362`, `:386`, `:392`).

## Shape

- `Snapshot{UpdatedAt, TotalUSD, HasStale, Warm, Assets, Wallets, Prices}`; `Assets` merges rows across wallets, sorted priced-first then by value (`internal/portfolio/types.go:22-34`, `aggregate.go:439-444`).
- `WalletView` is one source's priced rows plus `Status` (`synced|stale|error|pending`), `Err`, `AsOf`, `HiddenRows`, `Warnings`, `Meta` (`internal/portfolio/types.go:12-19`, `:93-109`). Not the same thing as a source definition.
- `HoldingRow` = `core.Holding` + optional `Price`, `ValueUSD`, `Priced`; unpriced rows count 0 (`internal/portfolio/aggregate.go:450-459`).
- `HasStale` is true if any wallet is stale or any row's price is stale; the UI badge keys off it (`internal/portfolio/aggregate.go:397-404`).
- Last-good fallback: `lastgood_wallet` (wallet_id, json, as_of) is written on every successful fetch and read on failure if younger than `StaleMax` (`aggregate.go:262`, `:283-309`, `internal/store/store.go:28`). Older than `StaleMax` becomes `StatusError`.
- `Warm` snapshot: not a finished refresh. Built at startup from last-good wallets plus `CachedPrices` (no network), wallets `pending`; and published during a refresh as wallets finish and early prices arrive, each unfinished wallet showing what it showed before, priced from this build's early prices, the last build's, then `CachedPrices`. Never recorded, alerted on or backfilled from (`aggregate.go:216-240`, `:177-199`, `refresher.go:39-42`, `internal/portfolio/types.go:25-29`, `internal/server/history_handlers.go:73-75`).
- `HiddenRows` = the source's own `WalletResult.Hidden` (Solana `hideMints`), priced (`aggregate.go:371-373`), plus spam candidates: with `HideUnpriced`, Solana rows that are unverified, unpriced or under `MinUSD`; SOL and DeFi legs never hide (`aggregate.go:374-381`, `:464-469`).
- Cash/stablecoin grouping is a read-side rule: `core.IsStable` (USD/USDC/USDT pegs plus a ticker list) drives `StableShare()` on snapshot and wallet (`internal/core/core.go:176-179`, `internal/portfolio/types.go:36-72`); the "one line" display is in `web/templates/format.go:391`.
- `AssetRow.Change7d` is not set here: the server fills it from history per request (`internal/server/portfolio_handlers.go:115-135`).
- `Refresher`: one build at a time via singleflight; builds detach from the request context (2 min cap); a forced refresh within `MinForceInterval` (30s) returns `ErrTooSoon` (`refresher.go:110-125`). A `Kick()` cancels the build in progress; a replaced build publishes nothing (`gen`, `refresher.go:76-91`, `:144-151`). During a build, progress snapshots are stored as `latest` and published (`refresher.go:143-152`). After a finished build: store `latest`, `History.Record`, `Broker.Publish`, `OnSnapshot` (`refresher.go:159-187`).
- `Broker`: each subscriber channel holds one pending snapshot, a slow client only gets the latest (`internal/portfolio/broker.go:5-45`).
- Wiring: `main.go:477-488` (Aggregator, Refresher), `main.go:509` (source change kicks a refresh), `main.go:510` (`go ref.Run`).

Citations: `internal/portfolio/types.go:22`, `internal/portfolio/aggregate.go:37`, `internal/portfolio/refresher.go:19`

## Connected to

- **owns:** per-wallet last-good results (`lastgood_wallet` in [database](../storage/database.md)); the broker and the refresher's `latest` pointer.
- **owned-by:** the `Refresher` in `main.go`; read by [server](../web/server.md) via `Refresher.Latest()` (`internal/server/handlers.go:71`).
- **joins:** [price](price.md) via `PriceFunc`/`CachedPrices` (`internal/portfolio/aggregate.go:24-25`, `main.go:313-324`); [value-history](value-history.md) via `Refresher.History`; [source-contract](../sources/source-contract.md) results (`core.WalletResult`) are the input; [alert-rule](../views/alert-rule.md) via `OnSnapshot`; [portfolio-view](../views/portfolio-view.md) filters it.
- **looks-like-but-is-not:** `portfolios` (the user's named views, [portfolio-view](../views/portfolio-view.md)); `WalletView` vs the source definition [source-def](../sources/source-def.md); a stored history row (`history` table) is not a snapshot.

## If you change this

- **Hits:** every page model that reads `Snapshot`/`WalletView` fields ([page-model](../web/page-model.md), [templ-components](../web/templ-components.md), `internal/demo` fixtures via [demo-fixtures](../web/demo-fixtures.md)); `portfolios.Apply` (rebuilds via `FromWallets`); `History.Record` and `History.Backfill` (read `Wallets[].Rows`, `Status`, `Prices`); `export.Holdings` ([csv-export](../ledger/csv-export.md)); the tax `quietSet` (reads `HiddenRows`, `internal/server/tax.go:370`, [tax-lot](../ledger/tax-lot.md)) and the Koinly cutover balances (rows, [koinly-import](../ledger/koinly-import.md)); the alert engine (reads `Status`, `Prices[].Stale`, rows); the `lastgood_wallet` JSON (it is `core.WalletResult`, so changing that type changes stored rows).
- **Does not hit:** the portfolios YAML or `Selector`; `price_history` daily closes.

## Surfaces

| Surface | Role |
|---|---|
| Refresher (interval, `Kick`, `Force`) | builds and publishes |
| Server handlers / SSE stream | read `Latest()`, subscribe to Broker (`internal/server/handlers.go:224`); `POST` refresh calls `Force` (`handlers.go:258`) |
| Source add/remove/edit handlers | `Refresher.Kick()` (`internal/server/source_handlers.go:351`) |
| History, alerts engine | consume each new snapshot |
| Tests | `internal/portfolio/portfolio_test.go`, `warm_test.go` |
| User hand-editing YAML | indirect: `HideUnpriced`/`MinUsd`/`staleMaxHours` in config (`internal/config/config.go:111-112`, `:20`) |

## See

- Source: `internal/portfolio/aggregate.go`
- Movement: [refresh-balances](../../processes/refresh-balances.md)
