---
type: process
universe: live
status: verified
verified: 2026-10-19 @ e280a97
consumes: [activity-entry, manual-cost-basis, koinly-import, tax-lot, price, portfolio-view, snapshot, yaml-store]
produces: [tax-lot, tax-report, csv-export, yaml-store]
---

# compute-taxes

Replay stored activity through a per-wallet FIFO engine into lots, disposals, income and review issues; let the user resolve issues; freeze a finished year; render the year as screens, CSV, TXF, printable 8949 and filled IRS PDFs.

## Input → Movement → Output

Input: every stored activity entry (minus CSV rows the live API already synced), the last closed year's closing lots (a CryptKeeper close or a Koinly opening), `data/tax.yaml` (manual lots, tags, price overrides, write-offs, scope), the hidden-mint config and the latest snapshot's auto-hidden assets. Movement: `taxInput` assembles these, `tax.Compute` replays them in time order, the server caches the result and turns it into a `YearReport`; the user fixes issues by tagging or pricing (which invalidates the cache), then closes the year. Output: `tax.Result` (lots, disposals, income, issues), a frozen `data/tax/<year>.yaml` on close, and report downloads.

## Why this shape

- The engine is a pure function of `Input`: no clock, no I/O. Every consumer (overview P&L, review tab, close, downloads) gets one answer from one place.
- Closing freezes the rows and the closing lots; the next year starts from them. Without that, a late sync or a changed tag would silently rewrite a filed return.
- Per-wallet queues, not a global pool: transfers between the user's own wallets (matched in [sync-activity](sync-activity.md)) move lots with their dates and cost; a global pool would count them as sales.
- The result is cached for a minute because the overview's P&L column would otherwise replay all activity on every live push; every edit calls `invalidateTax`.
- The quiet set (auto-hidden tokens) may only silence review items, never drop a leg, because it changes as balances change.
- Close refuses while the snapshot is only the warm one, while txs in the year are pending, or if rows fall outside the year.

## Steps

1. A request needs tax figures: the Tax tab (`GET /reports/tax`), a download, the overview P&L, the review panel, or close: `internal/server/server.go:156-168`.
2. `taxResult` serves a cached `tax.Result` younger than a minute, else calls `taxInput` then `tax.Compute`; a result computed before the first live (non-warm) snapshot is not cached. Newly synced activity does not invalidate the cache; it shows once the minute is up: `internal/server/tax.go:87-105`.
3. `taxActivity` reads all activity once per request context, drops CSV imports duplicated by live sync (`dedupeImported`) and remaps remaining CSV pseudo-wallets to the one live exchange of their type: `internal/server/tax.go:231-251`, `:38-62`.
4. `taxInput` adds `tax.yaml` manual lots, write-offs, tags and price overrides, the spam set (configured hidden Solana mints) and the quiet set (snapshot hidden rows); `From`, `Opening`, `OpeningParked` and `CarriedPeers` come from the last closed year: `internal/server/tax.go:106-158`, `:355-384`. Old `costbasis.yaml` manual lots were moved into `tax.yaml` once at startup (`main.go:205`); the costbasis CSV parsers now only produce activity rows (`internal/server/reports_handlers.go:296-312`).
5. A tax scope (a portfolio's wallets) removes other wallets' txs, lots and write-offs and tags the transfers crossing the boundary own-wallet: `internal/server/tax.go:155-157`, `:205-224`.
6. `Compute` seeds queues with opening lots, merges manual lots and write-offs into the time-ordered tx stream, and applies each tx by kind (transfer halves, trades, rewards, positions, tags, fees): `internal/tax/engine.go:141-170`, `:277-316`. Lots are consumed oldest first and partially split: `internal/tax/queue.go:29-51`; term is by calendar date in the tax zone: `internal/tax/queue.go:65-77`.
7. The review tab groups `Result.Issues` (missing basis/price, unmatched send/receive, unpaired transfer, undated lot) with tag options and price keys: `internal/server/review.go:109-215`.
8. Each fix (tag, bulk tag, tag delete, price override, write-off, write-off delete) runs through `reviewSave` -> `TaxSettings.Update` (writes `data/tax.yaml`) -> `invalidateTax`: `internal/server/review_handlers.go:57-69`. The scope choice does the same in `taxViewScope`: `internal/server/taxview.go:392-421`. Importing exchange CSVs, deleting a source and a backup restore that touched tax data or the database also invalidate: `internal/server/reports_handlers.go:325`, `internal/server/settings_handlers.go:320-322`, `:343-347`.
9. A Koinly import becomes a closed year: `koinlyClose` builds opening lots from reconciled holdings and saves `tax.Year{Source: "koinly"}`; it then kicks the syncer: `internal/server/koinly_handlers.go:624-672`, `:601-613`.
10. Close (`POST /reports/tax/close`, two steps): checks order, year ended, snapshot not warm, nothing pending, recomputes with `To`, refuses rows outside the year, asks for confirmation with the open issue count, then saves the year with closing lots and invalidates: `internal/server/close_handlers.go:105-190`. Reopen deletes the file: `internal/server/close_handlers.go:224-262`.
11. `yearReportWith` builds the report: a closed year `FromClosed`; the current open year from the cached result; an earlier open year recomputed with `To` and cached a minute: `internal/server/taxview.go:254-297`. Prices for year-end holdings come from `resolver`: `internal/server/taxview.go:149-212`.
12. Downloads each call `taxBundleFor` (year must be in the selector) then a writer: Koinly-format CSVs, other-gains, TurboTax CSV, TXF, Form 8949 page, full report page, IRS PDFs and a zip: `internal/server/taxdownloads.go:43-67`, `:142-154`; writers in `internal/tax/report/koinlycsv.go:63-131`, `internal/tax/report/turbotax.go:42-90`, `internal/tax/irs/irs.go:63`.

## If you change this

- **Hits:** [tax-lot](../objects/ledger/tax-lot.md) types and engine rules (they feed every report), the issue kinds and the review UI, the frozen year file format (`internal/tax/years.go`; older closed years must still load), [tax-report](../objects/ledger/tax-report.md) writers, the Koinly opening import, anything that must call `invalidateTax` after changing tax inputs. Outside the Tax tab, `taxResult` also feeds the overview P&L column (on every `/stream` patch), the wallet page's cost tiles and `/export/pnl.csv` ([csv-export](../objects/ledger/csv-export.md)): `internal/server/reports_handlers.go:89-125`, `:366-382`.
- **Does not hit:** balances and the snapshot ([refresh-balances](refresh-balances.md)); the activity CSV export in `/export/activity.csv`, which does not use the engine; live prices on the overview.

## Surfaces

| Surface | Role |
|---|---|
| `/reports/tax` (+ `/year`, `/scope`, `/tag*`, `/price`, `/writeoff*`) | view, scope, resolve issues |
| `/reports/tax/close`, `/close-cancel`, `/reopen` | freeze or reopen a year |
| `/reports/koinly/*` | Koinly import wizard and accept |
| `/reports/tax/{year}/...` | CSV, TXF, 8949, report, IRS PDF, zip |
| Overview and coin pages | P&L from the cached result |

## See

- Objects: [tax-lot](../objects/ledger/tax-lot.md), [tax-report](../objects/ledger/tax-report.md), [koinly-import](../objects/ledger/koinly-import.md), [manual-cost-basis](../objects/ledger/manual-cost-basis.md), [activity-entry](../objects/ledger/activity-entry.md), [yaml-store](../objects/storage/yaml-store.md), [portfolio-view](../objects/views/portfolio-view.md), [price](../objects/valuation/price.md)
- Source: `internal/tax/engine.go`, `internal/server/tax.go`, `internal/server/taxview.go`, `internal/server/close_handlers.go`, `internal/server/taxdownloads.go`
