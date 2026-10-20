---
type: object
cluster: valuation
universe: live
status: verified
verified: 2026-10-20 @ 73e5bf4
entity: internal/portfolio/history.go
---

# Value history

Portfolio value over time: `portfolio.History` and its `history` table (amounts and prices per sample), plus the separate `price_history` table of daily USD closes served by `prices.Service`, plus the per-wallet on-demand chart backfill.

## Why this shape

A `history` row stores per-wallet amounts and per-asset prices, not a total, so any portfolio, including one created today, can be charted retroactively by summing only the pairs it includes (`internal/portfolio/history.go:13-14`, `:81-82`). Daily closes live in a different table because they answer a different question (what was BTC worth on a past day, for backfill, tax and activity), and come from other APIs.

## Shape

- `history (ts INTEGER PK, prices TEXT json assetKey->usd, amounts TEXT json walletID->assetKey->amount)` (`internal/store/store.go:29`; row type `internal/portfolio/history.go:21-25`).
- `Record`: at most one row per `Interval` (30 min); wallets in `StatusError` are skipped, stale ones recorded; no row if no wallet has amounts (`history.go:29-71`, `main.go:473`).
- `Compact`: rows older than 30 days keep only the last row per UTC day; run once at startup (`history.go:73-79`, `main.go:473-476`).
- `Series(from, Include, current)`: values each row with that row's prices; the series starts only once every live wallet has history, and rows missing a live wallet are dropped, so adding a source never looks like growth; appends the live snapshot; downsampled to 500 points (`history.go:84-178`, `:190-201`).
- Ranges `1D 1W 1M YTD 1Y All`; `ChangeFor` is `Valid` only when history reaches back to the window start (tolerance d/10, at least 24h for 7d+) (`history.go:203-267`).
- `PriceChanges` (nearest row to now-window, within 10%) feeds `AssetRow.Change7d`; `ValueAt` values one past row, used by portfolio_move alerts (`history.go:269-296`, `:326-356`, `internal/server/portfolio_handlers.go:115-135`, `main.go:414-425`).
- `PriceSeries(from)`: each asset's recorded positive prices since `from`, oldest first, one query on `history.prices` (`history.go:298-324`). The overview's price lines read it: `priceTrends` appends the current price, keeps assets with two points and downsamples to 96 (`internal/server/portfolio_handlers.go:139-163`); see [page-model](../web/page-model.md).
- `price_history (asset, day "YYYY-MM-DD" UTC, usd)` PK (asset, day): `Historical` reads it first, else Coinbase Exchange daily candles (windows of up to 300 days, stored in one transaction); USD/USDC/USDT are 1; stocks via Yahoo; `spl:` keys and unlisted symbols are unknown; today's close is never stored (`internal/prices/history.go:34-84`, `:246-290`, `internal/store/store.go:31`).
- Mint closes: `HistoricalMint[Close]` use CoinGecko (365-day cap, demo key, 429 pauses lookups) with misses remembered in `price_unknown` (`internal/prices/coingecko.go:20-44`).
- `Service.Series(assetKey, mint, days)` builds a daily series from those closes (`internal/prices/series.go:24`).
- Backfill: `History.Backfill(days, now, current, txs, isPosition, PriceAt)` rolls current holdings back through activity, per wallet, only for days before that wallet's first recorded row (on days that already have rows it merges the missing wallets into them); refuses with `NotSyncedError` if any wallet is error or pending; looks prices up first, then writes all rows in one transaction; a failed lookup (`PriceError`) writes nothing (`internal/portfolio/backfill.go:41-70`, `:260-291`). Rows are stamped 23:59:59 UTC.
- Handler: `POST /chart/backfill` -> `runBackfill` (needs a non-warm snapshot) -> `priceAt` adapter (pegs, mint closes, ticker then mint) (`internal/server/server.go:117`, `internal/server/history_handlers.go:28-130`).

Citations: `internal/portfolio/history.go:15`, `internal/prices/history.go:43`, `internal/portfolio/backfill.go:69`

## Connected to

- **owns:** the `history` and `price_history` tables' contents ([database](../storage/database.md) owns the schema and migrations).
- **owned-by:** `Refresher.History` calls `Record` after each build ([snapshot](snapshot.md)); the server holds `History` for charts.
- **joins:** [price](price.md) (`Service` serves both spot and closes); [activity-entry](../ledger/activity-entry.md) (backfill rolls amounts back through transactions); [portfolio-view](../views/portfolio-view.md) (`Include` filter); [alert-rule](../views/alert-rule.md) (`ValueAt`).
- **looks-like-but-is-not:** transaction history, which is activity ([activity-entry](../ledger/activity-entry.md)); `lastgood_price`, the freshest spot price ([price](price.md)); the tax engine's lots ([tax-lot](../ledger/tax-lot.md)).

## If you change this

- **Hits:** the `history` JSON shape is read by `Series`, `PriceChanges`, `ValueAt` and `Backfill` (`Compact` only reads `ts`), and through `Series` by CSV `/export/history.csv` (`internal/server/reports_handlers.go:347`); chart/value-card page models; [csv-export](../ledger/csv-export.md); alert portfolio_move; `Backfill` mirrors `tax.RollBack` rules (`backfill.go:56`), so change them together.
- **Does not hit:** `lastgood_price` and the spot chain; `portfolios.yaml`.

## Surfaces

| Surface | Role |
|---|---|
| Refresher | writes `history` rows (`Record`) |
| Startup | `Compact` |
| Server | chart `Series`, `PriceChanges`, `/chart/backfill`, history CSV |
| Alerts engine | `ValueAt` via the `HistoryValuer` closure |
| Tax / activity / backfill | read `price_history` through `Historical` |
| Tests | `internal/portfolio/backfill_test.go`, `portfolio_test.go`, `internal/prices/history_test.go`, `series_test.go` |

## See

- Source: `internal/portfolio/history.go`
- Movement: [backfill-history](../../processes/backfill-history.md)
