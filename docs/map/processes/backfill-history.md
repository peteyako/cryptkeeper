---
type: process
universe: live
status: verified
verified: 2026-10-20 @ 936c434
consumes: [snapshot, activity-entry, price, value-history, tax-lot]
produces: [value-history, price]
---

# backfill-history

When the user asks for it, rebuild the portfolio's past daily value rows by rolling today's holdings back through stored activity and pricing each day at its closing price; separately, load one asset's daily price series for the coin chart.

## Input → Movement → Output

Input: a click on "Load last N days" (`POST /chart/backfill`, N from the selected range) or a coin page load/range click (`POST /a/{asset}/chart`), plus the latest live snapshot, all activity and the `history` and `price_history` tables. Movement: the portfolio backfill walks each missing UTC day, subtracts later transactions from current amounts per wallet, looks up each held asset's daily close (cached in `price_history`, else fetched in 300-day windows), and writes all rows in one transaction; the coin chart only reads the same closes through `Series`. Output: new or extended `history` rows stamped 23:59:59 UTC, new `price_history` rows, and a re-patched value card or coin chart.

## Why this shape

- Normal recording (every refresh, 30 min apart) only starts when the app starts, so a new install or a newly added wallet has no past. Backfill is explicit and bounded, not automatic, so it never silently spends rate-limited API calls.
- Refuse while any wallet is errored or pending (`NotSyncedError`) or the snapshot is only the warm one: the past rows would be written without that wallet, for good, and the chart would show a cliff when it later appears.
- Prices are looked up before the write transaction opens and a failed lookup aborts everything (`PriceError`): the store has a single connection (a lookup inside the transaction would deadlock it), and a fallback price would be stored in the past permanently.
- Per-wallet fill (`fillUntil`): only days before a wallet's first recorded row are filled, and wallets missing from an existing day's rows are merged into them. Re-running after adding a source fills just that source; recorded history is never rewritten (`UPDATE ... WHERE amounts = old`, `INSERT OR IGNORE`).
- Closing prices are cached by asset and day with remembered misses, and fetched as windows around the requested day: a 365-day backfill of ten assets is a handful of requests, not thousands.

## Steps

1. The overview shows "Load last N days" only when the range's change is not valid; N is 7, 30, 365 or days since Jan 1 (YTD): `internal/server/handlers.go:162-185`, `web/templates/overview.templ:98-108`, click `web/templates/js.go:107`.
2. `chartBackfill` runs under a 60s timeout; errors become a note on the card (and a log line), as does a run that wrote nothing: `internal/server/history_handlers.go:24-67`.
3. `runBackfill` needs a ready, non-warm snapshot; reads activity through `taxActivity` (CSV duplicates removed); txs tagged `position` in `tax.yaml` keep their units like the tax engine: `internal/server/history_handlers.go:72-90`, `internal/server/tax.go:231-251`.
4. `Backfill` first rejects errored or pending wallets (stale ones are allowed): `internal/portfolio/backfill.go:74-82`. It spans `days+1` UTC days ending yesterday and loads what is recorded: `internal/portfolio/backfill.go:83-135`, `:310-349`.
5. For each day to write, each wallet's amount is current minus the signed legs and fees of every tx at or after the next UTC midnight; stakes, positions and failed txs keep units: `internal/portfolio/backfill.go:157-196`.
6. Price per held asset and day: `priceAt` returns pegged = 1, Solana tokens by mint close, other symbols by ticker then mint. A failed lookup returns an error; "no price" falls back to the snapshot price, then 1 for pegged: `internal/server/history_handlers.go:97-114`, `internal/portfolio/backfill.go:197-225`.
7. `Historical` answers from `price_history`, else a remembered miss, else one singleflight fetch of a 300-day window (150 forward) from Coinbase Exchange daily candles, stored in `price_history`; today's close is live and never stored; stocks go through Yahoo (requests paced 500 ms): `internal/prices/history.go:43-131`, `:190-243`, `:260-288`, `internal/prices/stocks.go:28-29`.
8. Solana mints use CoinGecko market-chart ranges (max 365 days back); a 429 pauses every CoinGecko lookup for 5 minutes or the Retry-After: `internal/prices/coingecko.go:44-94`, `:119-158`, `:168-205`.
9. All writes happen in one transaction; a cancelled context or any error writes nothing: `internal/portfolio/backfill.go:256-290`.
10. The handler rebuilds the overview page; `History.Series` reads rows since the range start, begins only when every live wallet has history, drops partial rows, and downsamples to 500 points: `internal/server/history_handlers.go:44-49`, `internal/portfolio/history.go:91-178`.
11. Coin chart: `GET /a/{asset}` renders at once with a hidden element whose `data-init` posts `/a/{asset}/chart?range=`; a range click posts the same: `internal/server/coin_handlers.go:86-93`, `web/templates/coin.templ:143-144`, `web/templates/js.go:157-159`.
12. `coinChart` builds the page with a 30s limit and `Series`, which calls `Historical` (or the mint close) for every day of the range, skipping failures, and patches `#coin-chart`: `internal/server/coin_handlers.go:29-105`, `internal/prices/series.go:24-72`. This writes `price_history` as a side effect but not `history`.

## If you change this

- **Hits:** [value-history](../objects/valuation/value-history.md) row format (`history.prices`/`amounts` JSON, written by `Record` in [refresh-balances](refresh-balances.md) too), the `price_history` cache and its callers (`sync-activity` valuation and tax reports use the same `Historical`), the rollback rules, which mirror `tax.RollBack` by copy ("portfolio must not import tax"), the range-to-days map, and the card's note texts.
- **Does not hit:** live price fetching (`Service.Get`); tax figures; there is no separate per-wallet backfill route, the per-wallet logic lives inside `Backfill`.

## Surfaces

| Surface | Role |
|---|---|
| Overview value card "Load last N days" | `POST /chart/backfill` |
| Coin page and range buttons | `POST /a/{asset}/chart` |
| `history` table | portfolio value rows (ts, prices, amounts) |
| `price_history` table | daily closes by asset |
| External APIs | Coinbase Exchange candles, CoinGecko market chart, Yahoo |

## See

- Objects: [value-history](../objects/valuation/value-history.md), [price](../objects/valuation/price.md), [snapshot](../objects/valuation/snapshot.md), [activity-entry](../objects/ledger/activity-entry.md), [tax-lot](../objects/ledger/tax-lot.md)
- Source: `internal/portfolio/backfill.go`, `internal/server/history_handlers.go`, `internal/prices/history.go`, `internal/prices/coingecko.go`
