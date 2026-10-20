---
type: object
cluster: ledger
universe: live
status: verified
verified: 2026-10-20 @ 936c434
entity: internal/export/export.go
---

# CSV export

`internal/export`: four plain CSV downloads (holdings, portfolio history, profit and loss, activity) written for spreadsheets; the package is only formatting, the handlers choose the data.

## Why this shape

The writers take already-resolved values (a snapshot, points, rows, entries, a label map) and know nothing of the server, so the page and the file cannot drift apart: the handler does the filtering the page does, and `export` renders. Output is deliberately machine-friendly and never encrypted: unlike the YAML exports (backup, sources, portfolios), CSV downloads do not go through `secrets.Box`, and `Cache-Control: no-store` is the only protection (`internal/server/reports_handlers.go:332-336`). Because token symbols and counterparties come from outside, any text cell starting with `= + - @` (or tab/CR) gets a leading apostrophe (`internal/export/export.go:41-51`).

## Shape

- Conventions: header row first, RFC 4180 via `encoding/csv`, numbers as shortest decimal (no exponent), NaN/Inf become empty, timestamps RFC 3339 UTC (`internal/export/export.go:1-8,24-32,53`).
- `Holdings(w, snap)`: wallet label, source type, asset key, symbol, amount, price, value, stale, protocol; hidden holdings are left out (`internal/export/export.go:68-72`).
- `History(w, pts)`: timestamp, value_usd (`internal/export/export.go:94-95`).
- `PnL(w, rows)`: per asset basis, unrealized and one `realized_<year>` column per year present; `PnLRow` mirrors the page row (`internal/export/export.go:104-122`).
- `Activity(w, txs, labels)`: one row per leg; the fee appears on the first row only so sums are not doubled; a leg-less tx still gets one row (`internal/export/export.go:162-169`).
- Handlers (all GET): `/export/holdings.csv` (`?pf=` portfolio), `/export/history.csv` (`?pf=`, `?range=`), `/export/pnl.csv`, `/export/activity.csv` (feed filters: source, asset, kind, range, from, to) (`internal/server/server.go:175-178`, `internal/server/reports_handlers.go:338-410`). Activity uses `Activity.All` plus the same period/kind/asset filters, not `withoutSpam`, so spam-tagged entries are included.
- `exportPnL` builds rows from `taxResult` plus `realizedByAsset`; with no tax result the rows have no basis (`internal/server/reports_handlers.go:366-379`).
- Download links: `web/templates/reports_view.go:72-75` (Reports page), `web/templates/activity_view.go:327-329` and `web/templates/js.go:305` (activity feed button), `cmd/uipreview/pages.go:150` (preview stub).

## Connected to

- **owns:** the four writers and their column order.
- **owned-by:** [server](../web/server.md) routes and handlers.
- **joins:** [snapshot](../valuation/snapshot.md) (holdings), [value-history](../valuation/value-history.md) (history), [tax-lot](tax-lot.md) (P&L cells), [activity-entry](activity-entry.md) (activity), [portfolio-view](../views/portfolio-view.md) (`?pf=`).
- **looks-like-but-is-not:** [tax-report](tax-report.md) (Koinly-style CSV, TurboTax, TXF, IRS PDFs, served under `/reports/tax/`); [backup-file](../storage/backup-file.md) (YAML export, encryptable).

## If you change this

- **Hits:** the four handlers in `internal/server/reports_handlers.go`; the Reports and activity links; `internal/export/export_test.go`; users' spreadsheets (columns are a contract); the demo preview route.
- **Does not hit:** `internal/tax/report` writers; backup encryption (`CRYPTKEEPER_SECRET_KEY` never touches CSV).

## Surfaces

| Surface | Role |
|---|---|
| Reports page buttons, activity feed button | read: trigger downloads |
| `export` handlers | read snapshot, history, tax result, activity; write the response |
| user | opens in a spreadsheet; no write-back |
| tests | `internal/export/export_test.go` |

## See

- Source: `internal/export/export.go`
