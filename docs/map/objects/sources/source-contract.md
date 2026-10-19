---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/core/core.go
---

# Source contract (`core.Source`, `core.WalletResult`, `core.Holding`)

The small set of types every source type implements and returns; the aggregator and UI only ever see these, never a Coinbase or Esplora type.

## Why this shape

`internal/core` imports no other internal package, so any source, the aggregator and templates can share it without cycles (`internal/core/core.go:1-3`). The mandatory interface is tiny (`ID`, `Fetch`); everything optional (addresses, UTXOs, activity) is a separate interface that a source may or may not implement, discovered with a type assertion. That is how manual and brokerage sources stay trivial and how the activity syncer picks only capable sources (`main.go:367-374`).

## Shape

- `Source`: `ID() string`, `Fetch(ctx) (WalletResult, error)` (`internal/core/core.go:70-73`). On an error the aggregator discards the returned result and serves the last-good one marked `Stale` (within `StaleMax`) or an empty one with `Err` set from the error; it also overwrites `WalletID`, fills a zero `AsOf`, applies a 45 s timeout and recovers a panic (`internal/portfolio/aggregate.go:106-145`). A source need not set `Err` itself.
- `WalletResult`: `Holdings`, `Hidden` (spam/dust/unpriced, shown collapsed), `NFTCount`, `Warnings`, `Meta []KV` for the wallet-detail Connection panel, `Err`, `Stale` (last-good holdings), `AsOf` (`internal/core/core.go:49-61`).
- `Holding`: `AssetKey` is the cross-source identity (`BTC`, `USDC`, `spl:<mint>`, `stock:<TICKER>`); `Symbol`, `Name`, `Amount` (negative is debt), `AmountStr` exact decimal, `Staked`, `Vault`, `Verified`, DeFi tags `Protocol/Position/PositionKind`, `Note` (`internal/core/core.go:24-46`). Key helpers `SPLKey/IsSPLKey`, `StockKey/IsStockKey`, `NormalizeTicker` (`:130-166`).
- `Kind`: `exchange | hot | hardware | manual | brokerage`, the custody type that drives colour and dot shape (`internal/core/core.go:11-21`). `Validate` in sources accepts exactly these.
- Optional interfaces: `AddressLister`, `AddressDeriver`, `UTXOLister`, `ReceiveAddresser` (Bitcoin; Solana lists addresses) (`:108-127`). `ActivitySource` lives in `internal/core/activity.go:82-89` and is described with [activity-entry](../ledger/activity-entry.md).
- `core.Price` (USD, AsOf, Stale, Change24h, MarketTime, MarketOpen) is defined here too (`internal/core/core.go:75-87`); behaviour is in [price](../valuation/price.md).
- Stablecoin helpers `Pegged` (USD, USDC, USDT only) and `IsStable` (wider list) (`internal/core/core.go:168-180`): two different sets, easy to confuse.

## Connected to

- **owns:** nothing persisted; these are in-memory shapes (JSON tags feed the SSE/page models).
- **owned-by:** implemented by every `internal/sources/*` package; built by [source-def](source-def.md).
- **joins:** [snapshot](../valuation/snapshot.md) prices `Holding`s into `WalletView`; [activity-entry](../ledger/activity-entry.md) for `ActivitySource`.
- **looks-like-but-is-not:** `portfolio.HoldingRow` (priced); `manual.Holding` (YAML input line with `Asset/Amount/Note`, not a `core.Holding`).

## If you change this

- **Hits:** adding a `Holding` field: only the sources that set it and the templates that show it; `portfolio.HoldingRow` embeds `Holding` (`internal/portfolio/types.go:110-111`, `internal/portfolio/aggregate.go:314-315`) so it reaches page models ([page-model](../web/page-model.md)) unchanged, and last-good JSON (`lastgood_wallet`) decodes older rows with the zero value. Adding a `Kind`: `Validate` kind switch (`internal/sources/model.go:112-116`), `DefaultKind`, the kind switches in `web/templates/view.go:63,81,123,132,472` and the source form selects ([templ-components](../web/templ-components.md)), and the self-custody check in the alerts engine (`internal/alerts/engine.go:611`, [alert-rule](../views/alert-rule.md)). Changing `AssetKey` rules: [price](../valuation/price.md) key routing and [manual-source](manual-source.md)/[brokerage-source](brokerage-source.md) normalisation.
- **Does not hit:** `sources.Def` (the configuration; see [source-def](source-def.md)); `core.Tx` ledger types.

## Surfaces

| Surface | Role |
|---|---|
| Source packages | implement `Source` (+ optional interfaces) |
| Refresher / aggregator | calls `Fetch`, prices holdings |
| Server wallet pages | type-assert `AddressLister`, `UTXOLister`, `ReceiveAddresser` (`internal/server/handlers.go:330-344`) |
| Koinly import | type-asserts `AddressDeriver` (`internal/server/koinly_handlers.go:395`) |
| Tests | per-source `_test.go` files |

## See

- Source: `internal/core/core.go`
