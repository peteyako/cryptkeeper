---
type: object
cluster: valuation
universe: live
status: verified
verified: 2026-10-20 @ 0a7dfb9
entity: internal/prices/prices.go
---

# Price

A USD spot price with optional 24h change (`core.Price`), resolved per asset key by `prices.Service`, which tries several upstream sources, caches in memory, and keeps a last-good copy in SQLite.

## Why this shape

Asset identity differs by kind, so the chain is chosen by key: fiat, exchange symbol, `spl:<mint>`, `stock:<TICKER>`. Scam tokens reuse real tickers and exchange tickers can be stale (a delisted JUP product quotes about 1000x low), so anything with a mint is priced by mint on Jupiter first and `spl:` keys never fall back to symbol sources (`internal/prices/prices.go:283-286`). A lookup failure must not zero a balance, so a last-good price is served marked `Stale` up to `StaleMax` (`prices.go:479-506`).

## Shape

- Key forms (`Want.Key`): `USD` (constant 1), fiat codes (`EUR`...), symbols, `spl:<mint>`, `stock:<TICKER>`; `Want.Mint` lets a symbol fall back to Jupiter (`internal/prices/prices.go:53-60`, `:187-191`).
- Real order in `fetchLive`: stocks go to Yahoo; anything with a mint goes to Jupiter (`lite-api.jup.ag/price/v3`) first; mapped symbols Jupiter cannot price, and plain symbols, run `lookupSymbol`: fiat via Coinbase `/v2/exchange-rates` (price = 1/rate), else Coinbase spot (only if the product is tradable), else Kraken ticker (`prices.go:287-389`, `internal/prices/sources.go:40-210`). The package doc comment lists Coinbase before Jupiter for symbols; the code wins (`prices.go:7-8` vs `:283-286`).
- Stocks: Yahoo spark then chart, two hosts, requests paced 500ms; last-good usable up to `stockStaleMax` (4 days) because markets close (`internal/prices/stocks.go:23-40`, `prices.go:494-496`).
- Stablecoin peg fallback: when every source fails, `USD/USDC/USDT` (`core.Pegged`) are assumed 1.0, not stored or cached (`prices.go:216-220`, `internal/core/core.go:169`).
- TTL cache: 60s in memory (`DefaultTTL`); `cacheGet`/`cachePut` (`prices.go:44`, `:417-440`). `Cached()` answers from cache, last-good and pegs with no network, for the warm start (`prices.go:228-252`).
- `lastgood_price (asset, usd, change24h, as_of)` upserted on every live price; `lastGood` rejects rows older than `StaleMax` (default 24h) (`prices.go:444-506`, `internal/store/store.go:27`).
- `price_unknown (key, until)` only remembers CoinGecko "coin not found" for a mint (key `cg:<mint>`, 7 days); it belongs to the historical-price path, not spot (`internal/prices/coingecko.go:14-29`).
- `Change24h` comes from Coinbase stats, Kraken, Jupiter or Yahoo; may be nil (`prices.go:114-124`).
- Settings: `POST /settings/prices` saves or clears the CoinGecko demo key, sealed in the `kv` table under `prices.coingeckoKey`; `COINGECKO_API_KEY` wins (`internal/server/server.go:191`, `internal/server/settings_handlers.go:165-197`, `main.go:817-844`). CoinGecko is used for historical mint closes only, not spot.
- Construction: `prices.New(client, db, Options{TTL: 1m, StaleMax: cfg.StaleMax(), CoinGeckoKey})` (`main.go:174`).

Citations: `internal/prices/prices.go:182`, `internal/prices/sources.go:93`, `internal/prices/stocks.go:142`

## Connected to

- **owns:** `lastgood_price` and `price_unknown` rows ([database](../storage/database.md)); the in-memory TTL cache.
- **owned-by:** `main.go` builds one `Service`, shared by the aggregator and the server.
- **joins:** [snapshot](snapshot.md) (`PriceFunc`/`CachedPrices` adapters, `main.go:313-338`); [value-history](value-history.md) (same `Service` serves daily closes); [brokerage-source](../sources/brokerage-source.md) (`stock:` keys); [solana-source](../sources/solana-source.md) (mints, `solana.MintForSymbol`).
- **looks-like-but-is-not:** `price_history` daily closes ([value-history](value-history.md)); the cost-basis prices in tax lots ([tax-lot](../ledger/tax-lot.md)); a transaction leg's `USD` value.

## If you change this

- **Hits:** [snapshot](snapshot.md) (`Prices` map, `HasStale` via `Price.Stale`); `core.Price` readers (page models, `Change24h` display); alert price rules read `snap.Prices` and skip stale prices (`internal/alerts/engine.go:397-400`); `server.Historical`/watch-list pricing use the same `Service`.
- **Does not hit:** `portfolios.yaml` watch lists (they only name keys); `history` table (stores the prices as they were).

## Surfaces

| Surface | Role |
|---|---|
| Aggregator via `PriceFunc` | `Get` on every refresh; `Cached` on warm start |
| Server | watch-list rows (`internal/server/portfolio_handlers.go:81`), historical closes (`Deps.Historical`), Settings key form |
| Brokerage source | `Securities` names its tickers via `tickerLookup` (`main.go:295-297`) |
| Activity syncer | `Historical`/`HistoricalMint` as its `Valuer` for `Leg.USD` (`main.go:503`) |
| User | CoinGecko key in Settings or env `COINGECKO_API_KEY` |
| Tests | `internal/prices/prices_test.go`, `stocks_test.go`, `coingecko_test.go` (fake upstreams) |

## See

- Source: `internal/prices/prices.go`
