---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/sources/manual/manual.go
---

# Manual source (`manual.Source`)

Source type `manual`: holdings typed in by hand for coins held anywhere the app cannot read; priced like any other holding.

## Why this shape

It is the escape hatch for custody the app cannot query (paper wallets, other chains, OTC). Holdings live inline in `data/sources.yaml` rather than in a database, so the user can hand-edit them and they come out in backups and exports. `Fetch` never touches the network, so it cannot fail and never needs a scan state or activity sync. The package also owns `manual.Holding`, which the brokerage source reuses.

## Shape

- `Holding{Asset, Amount, Note}` with YAML keys `asset`, `amount`, `note` (`internal/sources/manual/manual.go:16-20`); stored in `Def.Holdings` (`internal/sources/model.go:66`).
- `New(id, holdings)`; `Fetch` returns one `core.Holding` per line with `AssetKey = NormalizeAsset(asset)`, `Mint` set for `spl:` keys, `AmountStr` formatted from the float amount, `Note`, and Meta `Type`/`Entries` (`manual.go:28-49`).
- `NormalizeAsset`: upper-cases symbols; `spl:<mint>` keys keep their case (mints are case-sensitive base58) and get a shortened symbol like `abcd…wxyz` (`manual.go:38-40,53-59`).
- `ParseLines` / `FormatLines`: the form textarea, one `ASSET AMOUNT [note]` per line; `#` comments, thousands commas allowed, negative amounts rejected, at least one line required (`manual.go:63-100`).
- Validation in `Def.Validate`: at least one holding (`internal/sources/model.go:134-137`). Kind is forced to `manual` by the form (`internal/server/source_handlers.go:186-192`) and is the default (`internal/sources/model.go:165`).
- Registered in the factory: `case sources.TypeManual: manual.New(d.ID, d.Holdings)` (`main.go:286-287`). `HandEntered("manual")` is true, so no activity sync and no sync-activity row (`internal/sources/model.go:37`).
- No `ActivitySource`, no secrets, no config keys, no Settings handler.

## Connected to

- **owns:** nothing persisted outside `sources.yaml`.
- **owned-by:** [source-def](source-def.md); implements [source-contract](source-contract.md).
- **joins:** [price](../valuation/price.md) prices by `AssetKey` (a symbol, or `spl:` mint); [brokerage-source](brokerage-source.md) reuses `manual.Holding` and its edit form helpers (`FormatLines`).
- **looks-like-but-is-not:** [manual-cost-basis](../ledger/manual-cost-basis.md) (`costbasis.yaml`, tax lots), which is about what was paid, not what is held; `core.KindManual` (the custody kind, shared with nothing else).

## If you change this

- **Hits:** `NormalizeAsset` is not manual-only: it also normalises portfolio asset selectors and watch lists (`internal/portfolios/portfolios.go:163-164`, `internal/server/portfolio_handlers.go:306`, [portfolio-view](../views/portfolio-view.md)) and price-alert assets (`internal/server/alerts_handlers.go:209`, [alert-rule](../views/alert-rule.md)), and its output is the [price](../valuation/price.md) key and must match Solana's `spl:<mint>` keys. Line format: `internal/server/source_handlers.go` form re-fill (`formValues` uses `manual.FormatLines` for brokerage too). A `Holding` field change also hits [brokerage-source](brokerage-source.md) and `sources.Def.Holdings`.
- **Does not hit:** [brokerage-source](brokerage-source.md) parsing (it has its own `ParseLines`), activity sync, `config.yaml`.

## Surfaces

| Surface | Role |
|---|---|
| Server add/edit form | `ParseLines`, `FormatLines` |
| Refresher | `Fetch` (instant) |
| User via YAML | edit `holdings:` list in `data/sources.yaml` (picked up on mtime change) |
| Tests | `internal/sources/manual/manual_test.go` |

## See

- Source: `internal/sources/manual/manual.go`
