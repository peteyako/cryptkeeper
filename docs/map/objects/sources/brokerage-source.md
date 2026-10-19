---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/sources/brokerage/brokerage.go
---

# Brokerage source (`brokerage.Source`)

Source type `brokerage`: a stock/ETF/fund account typed in by hand as tickers and share counts, plus cash; CryptKeeper never connects to the broker.

## Why this shape

Brokerage access would need credentials the project refuses to hold, so the account is a hand-typed list like the manual source, but tickers must not collide with coin symbols (BTC and ETH are also ETF tickers). Hence stock holdings are keyed `stock:<TICKER>` and routed to Yahoo Finance by the price service, while cash is stored as plain `USD` so it is priced at one and joins the cash and stablecoins line in allocation (`internal/sources/brokerage/brokerage.go:17-19`, `internal/core/core.go:140-142`).

## Shape

- Stored as `Def.Holdings []manual.Holding` where `Asset` is a ticker (or `USD` for cash, or explicit `stock:USD`/`stock:CASH` for those tickers) and `Amount` is shares or dollars (`internal/sources/model.go:65-66`, `brokerage.go:17-44`).
- `Validate`: at least one holding; every asset passes `brokerage.ValidAsset` (cash, or a ticker as `core.NormalizeTicker` writes it: 1-15 of A-Z 0-9 . - ^ =) (`internal/sources/model.go:138-146`, `brokerage.go:37-44`, `internal/core/core.go:154-166`).
- `ParseLines`: `TICKER SHARES [note]`; first word `USD` or `CASH` means cash and a leading `$` is allowed; `stock:` prefix forces a ticker (`brokerage.go:127-163`).
- `Fetch`: cash line yields `AssetKey/Symbol "USD"`, name "Cash"; ticker line yields `AssetKey core.StockKey(t)`, `Name` from the lookup (else the ticker). Warnings for a ticker Yahoo does not know, one listed in a non-USD currency (only US-dollar listings are priced), or an unreachable lookup (prices may be out of date). Meta `Prices: Yahoo Finance`, `Holdings` count (`brokerage.go:72-121`).
- Naming/pricing wiring: `Lookup func(ctx, tickers) (map[string]Security, error)` is injected; `main.go` binds it to `prices.Securities` (Yahoo) in `app.tickerLookup` (`brokerage.go:46-66`, `main.go:288-304`). Prices themselves come from the price service via `stock:` keys with `Change24h`, `MarketTime`, `MarketOpen` ([price](../valuation/price.md); `internal/prices/prices.go:11,293`).
- Kind forced to `brokerage` by the form (`internal/server/source_handlers.go:193-199`); `HandEntered` true, so no activity sync (`internal/sources/model.go:37`). Brokerage sources are skipped in tax-download wallet lists and Koinly wallet matching (`internal/server/taxdownloads.go:405`, `internal/server/koinly_handlers.go:386`) and the test message counts tickers priced (`source_handlers.go:288-290`).
- No secrets, no config keys, no Settings handler; depends only on the price service's Yahoo settings.

## Connected to

- **owns:** nothing persisted outside `sources.yaml`.
- **owned-by:** [source-def](source-def.md); implements [source-contract](source-contract.md).
- **joins:** [price](../valuation/price.md) (Yahoo, `stock:` keys); [snapshot](../valuation/snapshot.md) (cash folds into the cash and stablecoin allocation line); reuses `manual.Holding` from [manual-source](manual-source.md).
- **looks-like-but-is-not:** [manual-source](manual-source.md) (coins, plain symbols, no lookup); a broker import in [koinly-import](../ledger/koinly-import.md) (brokerage ledger rows are not synced).

## If you change this

- **Hits:** the `stock:` key shape: `core.StockKey`, price routing in `internal/prices`, history lookups (`internal/prices/history.go:44`). Cash rule: `brokerage.Cash` and allocation grouping (`core.IsStable`, `internal/portfolio/types.go:43,62`). Ticker rule (`ValidAsset`): `Def.Validate` calls it (`internal/sources/model.go:143`), so a stricter rule makes saves and imports of existing rows fail ([source-def](source-def.md)). Currency rule: warning text and the lookup `Security`.
- **Does not hit:** [manual-source](manual-source.md) `ParseLines`; coin pricing (CoinGecko); activity sync.

## Surfaces

| Surface | Role |
|---|---|
| Server add/edit/test form | `ParseLines`, `FormatLines` (manual's), `tickersFound` test summary |
| Refresher | `Fetch` -> price service for `stock:` keys |
| User via YAML | `holdings:` in `data/sources.yaml`; tickers must be capitalised |
| Tests | `internal/sources/brokerage/brokerage_test.go`, `internal/server/brokerage_test.go` |

## See

- Source: `internal/sources/brokerage/brokerage.go`
