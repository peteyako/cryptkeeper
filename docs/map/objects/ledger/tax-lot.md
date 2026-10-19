---
type: object
cluster: ledger
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/tax/engine.go
---

# Tax lot

The FIFO cost basis of `internal/tax`: per-wallet, per-asset lots (`tax.Lot`), the disposals and income they produce, the review issues, and the user's tax inputs in `data/tax.yaml` and closed years in `data/tax/<year>.yaml`.

## Why this shape

Koinly is the reference, so the engine copies its rules: one FIFO queue per (wallet, asset), where wallet is the tx's `SourceID` (`internal/tax/engine.go:262-264`, `internal/tax/queue.go:6`). Moves between the user's own wallets carry lots across keeping date and cost; only the difference is a "Fee" disposal. Only the open year is computed. A closed year is frozen into a file that also holds the closing lots the next year opens with, so a year Koinly closed (imported) and a year CryptKeeper closed look the same to the engine (`internal/tax/years.go:37-66`). `Compute` is pure: all state arrives in `Input` and the server assembles it.

## Shape

- `Lot{Wallet, Asset, Amount, CostUSD, Acquired, Various, TxID, Flag}`; flags `missing-basis`, `transfer-date`; pseudo-wallet `untracked` (`internal/tax/types.go:7,40-56`). `Disposal`, `Income`, `Parked`, `WriteOff`, `Issue` follow (`internal/tax/types.go:59-140`).
- `Input` / `Result` / `Compute` (`internal/tax/engine.go:31,70,141`). Replay order: opening lots, then txs by Time then ID, with manual lots and write-offs merged in before the first tx at or after their time (`internal/tax/engine.go:141-170`). Per-kind rules are in the comment at `internal/tax/engine.go:98-140`; transfers by `PeerID` are in `internal/tax/transfer.go:19`.
- Short-fall: what lots cannot cover is a cost-0 row "Missing cost basis" plus `IssueMissingBasis` (`internal/tax/engine.go:138-140`).
- Positions: units moved into a DeFi/perp position are `Parked`, not disposed; a write-off disposes parked units that will not return (`internal/tax/position.go`, `internal/tax/types.go:96-113`).
- Tags (`purchase`, `income`, `airdrop`, `own-wallet`, `position`, `spam`, `sale`, `gift`, `lost`...) override the movement rule of untagged receives/sends (`internal/tax/types.go:24-37`, `internal/tax/tags.go:10-30`). Issues (`unmatched-receive`, `missing-price`, `transfer-unpaired`...) are the review queue (`internal/tax/types.go:115-129`).
- `Settings` = `data/tax.yaml`: `walletMap`, `assetMap`, `manualLots`, `tags`, `prices` (PriceKey/FeePriceKey overrides), `walletBox`, `writeOffs`, `scope` (`internal/tax/settings.go:28-40`); `SettingsStore` reloads on file change and writes with backups (`internal/tax/settings.go:138-250`).
- `YearStore`: one file per closed year, loaded only at startup, so hand edits need a restart (`internal/tax/years.go:227-275`); `OpenYearStart` = Jan 1 after the last closed year in the zone it was closed in (`internal/tax/years.go:93`). `Year.Source` is `koinly` or `cryptkeeper` (`cryptkeep` in years closed before the rename, `internal/tax/years.go:40`).
- Server builds `Input` in `taxInput`: opening from the last closed year, tags/prices/manual lots from settings, activity via `taxActivity`, spam from hidden mints, `Quiet` from the snapshot, then portfolio scope; the result is cached a minute (`internal/server/tax.go:87-163`). `RollBack` derives wallet quantities at a cutover from today's (`internal/tax/rollback.go:20`).
- Movement (steps) is owned by the compute-taxes process card.

## Connected to

- **owns:** `data/tax.yaml`, `data/tax/<year>.yaml`.
- **owned-by:** [yaml-store](../storage/yaml-store.md) (atomic writes, backups).
- **joins:** [activity-entry](activity-entry.md) (input), [koinly-import](koinly-import.md) (writes closed year, maps, opening lots), [tax-report](tax-report.md) (consumes Result/Year), [manual-cost-basis](manual-cost-basis.md) (old lots migrated in), [portfolio-view](../views/portfolio-view.md) (`scope`), [snapshot](../valuation/snapshot.md) (`Quiet`, `Unrealized`/`Coverage` in `internal/tax/views.go`).
- **looks-like-but-is-not:** `internal/costbasis` entries and `Position` tagging on `core.Holding`.

## If you change this

- **Hits:** `internal/tax/report` and `irs` (row shape and boxes); `Year` YAML (old files must still load); server `tax.go`, `close_handlers.go`, `review_handlers.go`, `taxview.go`, `reports_handlers.go` (P&L, wallet tiles); koinly `BuildOpening`; `internal/demo/tax.go` (runs the real `Compute`); `internal/backup` (carries `tax.yaml` and every closed year, `internal/backup/backup.go:41-42`); `main.go` `migrateCostBasis` (builds `tax.Lot`s, `main.go:212-236`); tax view models in `web/templates`; `tax.yaml` validation in `Settings.normalized`.
- **Does not hit:** `internal/costbasis` (it does not import `internal/tax`; its CSV rows reach the engine only as activity); price fetching; [csv-export](csv-export.md) holdings/history.

## Surfaces

| Surface | Role |
|---|---|
| tax pages, P&L, wallet cost tiles | read `Result` via `taxResult` |
| `/reports/tax/close`, `/reopen` | write/delete closed years (`internal/server/close_handlers.go:105,224`) |
| review actions tag/price/writeoff | update `tax.yaml` (`internal/server/review_handlers.go:120-372`) |
| Koinly accept | writes a `koinly` year and wallet/asset maps |
| user hand-editing `tax.yaml` | live reload; `data/tax/*.yaml` needs restart |
| tests | `internal/tax/*_test.go` |

## See

- Source: `internal/tax/engine.go`
- Doc: `docs/superpowers/specs/2026-10-16-koinly-tax-design.md`
