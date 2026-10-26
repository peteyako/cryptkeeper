---
type: object
cluster: web
universe: live
status: verified
verified: 2026-10-26 @ be0017a
entity: internal/demo/demo.go
---

# Demo fixtures

`internal/demo` builds invented but internally consistent view models for every screen; `cmd/uipreview` serves them on :8790 (`task preview`) and `cmd/uipreview/genicons` renders the app icon.

## Why this shape

The UI must be viewable, screenshot-able and testable before any real data exists, and without touching `data/` or the network. The fixtures therefore produce the same `templates.*Page` structs the real handlers do, from a made-up portfolio (cold wallet, Coinbase, Kraken, Phantom, a family safe, brokerage accounts) with consistency rules the tests enforce: a row's value is amount x price rounded to cents, a wallet is the sum of its rows, the snapshot total the sum of wallets, asset shares sum to 1 (`internal/demo/demo.go:1-7`). Tax pages run invented activity through the real `tax.Compute`, so they cannot drift from the engine (`internal/demo/tax.go:16`).

## Shape

- Package `internal/demo` imports `web/templates`, `core`, `portfolio` and `tax`/`tax/report`, never `internal/server` (`demo.go:10-23`, `tax.go:3-13`). Builders by file: `demo.go` (prices, wallets, `Snapshot`, `SnapshotFor`, `Chart`, `Change`, `Shell`, `Overview*` (with `Trends`: a made-up week of prices per asset, from `change7d`, `demo.go:499-524`), `Wallet` (its holdings tab gets the same `Trends`, `demo.go:635`), source forms, `Settings`, `Portfolios`, `PortfolioForm`); `activity.go` (feed, paging, sync rows); `reports.go` (P&L costs); `tax.go` (`TaxPage`, `YearReport`: 2025 a closed Koinly year, 2026 open); `koinly.go` (wizard mid-import, empty, blocked; `KoinlyPage(step)` in `tax.go:246` picks steps 1-4); `alerts.go` (rule ids like `AlertColdDrop`); `coin.go` (`CoinPage`).
- Fixed ids: sources `IDCold`, `IDCoinbase`, `IDKraken`, `IDPhantom`, `IDMom`, `IDFidelity`, `IDRoth`; portfolios `long-term`, `trading`, `majors` (`demo.go:25-41`).
- `cmd/uipreview/main.go`: `router()` (`:80-`) wires the real URL shapes: pages, plus stub Datastar endpoints (`/view`, `/chart`, `/stream`, `/refresh`, `/w/{id}/tab/{tab}`, `/sources/test`, activity, reports, alerts). Stubs keep their changes in memory until restart or `?reset=1`. Query switches (`?empty=1`, `?invalid=1`, `?error=1`, `?tab=`, `?step=`, `?year=`) are documented at the file head (`:1-23`). It renders full documents only (`render`, `:72`), so it does not exercise the `IsNav` patch path. Route files: `pages.go` (`activityRoutes:32`, `reportsRoutes:77`, `alertsRoutes:243`). Listens on `:8790` (`:64`).
- `cmd/uipreview/genicons/main.go`: stdlib-only rasteriser for `icon-192/512`, `icon-maskable-512`, `apple-touch-icon` (`:36-60`); see [static-assets](static-assets.md).
- Tests that render demo pages: `web/templates/templates_test.go` (external package `templates_test`; `allPages:50`, fixed clock 2026-10-16, `:23`), which also feeds `static_test.go` (`:201`); `js_test.go` does not use fixtures; `internal/demo/demo_test.go` (consistency, paging, portfolio filters, page builders) and `tax_test.go`; `cmd/uipreview/main_test.go` (`TestEveryPageRenders:27` hits ~45 URLs and wants 200; stub tests for activity, reports, alerts). `internal/server` tests do not use `demo`.
- `task preview` runs `generate` then `go run ./cmd/uipreview` (`Taskfile.yml:32-36`).

## Connected to

- **owns:** the invented portfolio, its ids and the preview router.
- **owned-by:** none in the product binary; neither package is imported by `main.go`.
- **joins:** [page-model](page-model.md) (it constructs these structs), [templ-components](templ-components.md) (renders them), [static-assets](static-assets.md) (class-coverage test), [tax-report](../ledger/tax-report.md) (real engine over fake activity).
- **looks-like-but-is-not:** [server](server.md) (the real handlers; `uipreview` copies only their URL shapes); `data/` and `config.yaml` (never read here).

## If you change this

- **Hits:** adding or renaming a `templates.*Page` field → the demo builder that fills it (compile error) and, for meaning, the fixture numbers the tests assert; adding a page or route → a builder in `internal/demo`, a route in `cmd/uipreview`, a path in `TestEveryPageRenders`, and an entry in `allPages`; changing fixture figures → `demo_test.go`/`templates_test.go` expectations; dropping a fixture state also drops its classes from `static_test.go`'s coverage check (it only sees classes the demo pages render).
- **Does not hit:** production behavior; the real server, stores and `data/`.

## Surfaces

| Surface | Role |
|---|---|
| `cmd/uipreview` (`task preview`) | reads fixtures, serves :8790 |
| `web/templates` tests | read fixtures to render every page |
| `internal/demo` tests | check fixture consistency |
| Humans and screenshot tooling | view the previews |

## See

- Source: `internal/demo/demo.go`
