---
type: object
cluster: web
universe: live
status: verified
verified: 2026-10-19 @ 5d02073
entity: web/templates/models.go
---

# Page model

The plain Go view models (`OverviewPage`, `SettingsPage`, `PortfoliosPage`, `WalletPage`, ...) and the display-decision helpers in package `web/templates` that sit between a handler and a `.templ` file.

## Why this shape

Templates are dumb: a handler computes everything (snapshot, history, P&L, sync status) and hands one struct to one templ component, and the display decisions that would otherwise be `if` ladders in templ live in Go helpers so they can be unit tested (`web/templates/view.go:15-16`). The model package imports `core`, `portfolio`, `tax` but never `internal/server`, so `internal/demo` can build the same structs from fixtures with no server (see [demo-fixtures](demo-fixtures.md)). Every full page embeds `Shell` so the chrome (sidebar, active tab, portfolio, range) is the same for all.

## Shape

- Boundary: handler (`internal/server/*_handlers.go`) builds `templates.XxxPage` → `templates.XxxFull(page)` renders the document or, on a Datastar navigation, only the shell regions (see [server](server.md), [templ-components](templ-components.md)). Example: `overviewPage` → `templates.OverviewFull` (`internal/server/handlers.go:109-140,201`).
- `Shell` (`web/templates/models.go:15-24`): `Title`, `Active` (one of overview/wallets/activity/reports/alerts/settings/portfolios), `Portfolios []PortfolioNav`, `PF`, `Range`, `ShowLogos`, `AuthUser`. Built by `Server.shell` (`internal/server/handlers.go:43-60`). Embedded by every full-page model; `TaxPage` and `ReviewPage` are sections inside `ReportsPage` (`models.go:295-296`) and carry none, and the print views (`TaxReportView`, `print_view.go:30`) have no app shell.
- `SyncInfo` (`models.go:30`) drives the sync chip; `PnLCell` (`:466`) the P/L column.
- Pages in `models.go`: `OverviewPage:42`, `WalletsPage:64`, `WalletPage:72`, `SourcePickPage:100`, `SourceFormPage:103`, `SettingsPage:120`, `PortfoliosPage:175`, `PortfolioFormPage:201`, `ActivityPage:243`, `ReportsPage:283`, `TaxPage:304`, `ReviewPage:341`, `KoinlyPage:409`/`KoinlyWizard:416`, `AlertsPage:486`/`AlertFormPage:498`, `CloseCard:512`, `CoinPage:534`.
- Builders and helpers by file (all `web/templates/`): `view.go` (source/status labels `StatusView:146`, `SyncView:163`, holding names, `Location`/`Local` time zone `:451-456`, `asset()` content-hashed URLs `:422`, `SafeLogoURL:408`, nav context `WithNav/IsNav:586-593`); `activity_view.go` (`TxTitle`, `TxStatusView`, date/leg text); `alerts_view.go`; `koinly_view.go`; `reports_view.go`; `tax_view.go`; `print_view.go`; `chart.go` (SVG path, axes, hover JSON from `portfolio.Point`).
- `format.go` number rules, documented at its head (`web/templates/format.go:1-10`): `Fiat` 2 decimals with true minus (`:75`), `PriceFiat` (`:90`), `CoinAmount`/`Units` min 4 max 8 decimals, stables 2 (`:134,153`), `ChangePct` arrow+sign never color alone (`:182`), `ShortAddr` first 6 + last 4 (`:260`), `StalePrice` older than 15 min gets `~` (`:272`), `Ago/Since` (`:297,315`), `Allocation`/`HoldingItems` (`:401,477`).
- `errors_view.go`: `ExplainError(raw, sourceType, public, every)` turns a stored raw sync error into `ErrorView{Text, Fix, Raw}` in plain language; errors are matched by wording because they are stored as text (`web/templates/errors_view.go:8-40,117`).
- `SyncRow` (`models.go:227`) carries per-source sync state to the sync card (Activity and Settings pages); `ActivityRow` (`:219`) is one feed entry plus the labels the template cannot look up.

## Connected to

- **owns:** `Shell`, every `*Page` struct, number/date formatting, error wording.
- **owned-by:** package `web/templates` (same package as the `.templ` files, so templ can call the unexported helpers).
- **joins:** [server](server.md) (builds models), [templ-components](templ-components.md) (reads them), [demo-fixtures](demo-fixtures.md) (builds them from invented data), [snapshot](../valuation/snapshot.md), [price](../valuation/price.md), [tax-report](../ledger/tax-report.md), [activity-entry](../ledger/activity-entry.md).
- **looks-like-but-is-not:** `portfolio.WalletView`/`AssetRow` (domain rows embedded in pages, not owned here); `internal/server` view-ish types like `viewSignals` (request input, not output).

## If you change this

- **Hits:** adding or renaming a page field → the building handler in `internal/server`, the `.templ` that reads it, `internal/demo` builders, and `templates_test.go`/`format_test.go` expectations; changing `format.go` rules → every page plus `format_test.go`, and server-built text that reuses `Fiat`/`Coin` (`internal/server/coin_handlers.go`, `alerts_handlers.go`, `koinly_handlers.go`, `review.go`); `Allocation`/`Monogram` write token names (`--accent`, `--src-*`, `--gain`, `--disc-*`) into inline styles (`web/templates/format.go:359,427-454`), so they move with `tokens.css` ([static-assets](static-assets.md)); changing `ExplainError` → `errors_view_test.go` and the error text on the wallet page and the sync card (`wallets.templ:95`, `activity.templ:267`).
- **Does not hit:** the `portfolio.Snapshot` shape (domain; change it in [snapshot](../valuation/snapshot.md)); CSV exports in `internal/export` and the print/8949 pages (`report.Money8949`, `FormatQty`): separate formatting.

## Surfaces

| Surface | Role |
|---|---|
| `internal/server` handlers | write (construct models) |
| `.templ` components | read |
| `internal/demo`, `cmd/uipreview` | write (fixtures) |
| `web/templates/*_test.go` | read and assert on rendered output |

## See

- Source: `web/templates/models.go`
