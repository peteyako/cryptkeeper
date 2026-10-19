---
type: object
cluster: ledger
universe: live
status: verified
verified: 2026-10-19 @ d4899f7
entity: internal/costbasis/manual.go
---

# Manual cost basis

`internal/costbasis`, two halves: the live Coinbase/Kraken CSV parsers that turn exchange exports into activity, and the retired manual cost entries of `data/costbasis.yaml` (leftover: kept for backups and a one-time migration). The card is `live` for the parsers; treat everything marked *leftover* below as leftover.

## Why this shape

The package doc names both halves (`internal/costbasis/manual.go:1-3`). The manual store predates the FIFO tax lots; the server wiring calls it "the retired manual costs: kept for backup only" (`internal/server/server.go:49`). The tax engine never reads it: manual lots now live in `tax.yaml` (`ManualLots`, `internal/tax/settings.go:32`). The CSV half is in force: it is how a user backfills exchange history an API cannot reach (`/reports/import`). There is no average-cost calculation left in the package; cost basis is FIFO in `internal/tax` only.

## Shape

- **CSV parsers (live).** `ParseCoinbaseCSV` (`internal/costbasis/coinbase_csv.go:97`) and `ParseKrakenLedgersCSV` (`internal/costbasis/kraken_csv.go:51`) return `[]core.Tx`, sorted by time. Default source ids `coinbase-csv`/`kraken-csv`; `OnSkip` reports ignored rows (`internal/costbasis/csv.go:14-22`). `ParseOptions.SourceID` could aim an import at a live source, but `csvImport` always passes the pseudo-source ids (`internal/server/reports_handlers.go:296-305`); de-duplication happens later instead: `dedupeImported` drops CSV rows whose `ExternalID` a live source of the same type already has, and `csvTargets` re-wallets the rest (`internal/server/reports_handlers.go:23-40`, `internal/server/tax.go:226-251`).
- `ExternalID`: the Coinbase ID column, else a stable content hash plus an occurrence counter, so re-importing is idempotent (`internal/costbasis/coinbase_csv.go:300-330`); the Kraken refid. USD is set only for USD-priced rows; others are left for the syncer's valuation (`internal/costbasis/coinbase_csv.go:90-96`). Kraken groups ledger rows by refid, normalizes assets with `kraken.NormalizeAsset` and values a USD-quoted trade's other leg (`internal/costbasis/kraken_csv.go:101,133,209`).
- **Manual store (leftover).** `Manual{Version, Assets []ManualAsset, Lots []ManualLot}`, strict YAML, asset keys upper-cased except an `spl:` mint (`internal/costbasis/manual.go:82-126`). `ManualAsset.Override` was a per-asset replace-the-computed-state escape hatch (`manual.go:78-81`); nothing consumes it, and `SetAsset`/`DeleteAsset` have no callers outside tests (`manual.go:295-343`).
- `ManualStore` reloads on file mtime/size change, writes atomically keeping 20 backups (`manual.go:21-24`, `:197-293`); built in `main.go:189`.
- `migrateCostBasis` copies only `Lots` into `tax.yaml` as wallet `untracked` (an undated lot gets 2000-01-01), once, only while `tax.yaml` does not exist; notes are dropped and `assets` entries (opening amounts and overrides alike) are not migrated, with a warning (`main.go:209-236`).
- Backups still carry and restore it (`CostBasis *costbasis.Manual`, `internal/backup/backup.go:40,151-153,323-331`; `mergeManual` at `internal/backup/backup.go:382`).

## Connected to

- **owns:** the two CSV parsers; `data/costbasis.yaml` (leftover).
- **owned-by:** CSV output lands in [activity-entry](activity-entry.md); the YAML file and its backup copies in [yaml-store](../storage/yaml-store.md).
- **joins:** [tax-lot](tax-lot.md) (receives migrated lots; CSV rows become engine input after `dedupeImported` and re-walleting, `internal/server/tax.go:231`), [kraken-source](../sources/kraken-source.md) (`kraken.NormalizeAsset`), [server](../web/server.md) (`csvImport`), [backup-file](../storage/backup-file.md).
- **looks-like-but-is-not:** [tax-lot](tax-lot.md) is the live cost basis (see CONTEXT "cost basis"); `tax.yaml` `manualLots` is the live manual-lot input.

## If you change this

- **Hits:** CSV half: `csvImport` (`internal/server/reports_handlers.go:277-328`); the pseudo-source ids, hard-coded again in the server (`importedSourceType`/`dedupeImported`, `reports_handlers.go:23-40`; `csvTargets`, `internal/server/tax.go:284`; feed labels, `internal/server/activity_handlers.go:63-64`); the `ExternalID` scheme, which is the de-dupe key against live API activity and part of every stored `Tx.ID`, so changing it duplicates re-imported rows and orphans `tax.yaml` tags and price overrides keyed by tx ID; testdata under `internal/costbasis/testdata`. Manual half: `internal/backup` (field `costBasis`, merge on restore); `main.go` migration.
- **Does not hit:** `tax.Compute`; once `tax.yaml` exists, editing `costbasis.yaml` changes no lot, report or P&L. To change a manual lot, edit `tax.yaml` (see [tax-lot](tax-lot.md)).

## Surfaces

| Surface | Role |
|---|---|
| `/reports/import` csvImport | calls the CSV parsers, upserts activity |
| backup export/import | reads and writes `Manual` |
| startup migration | reads once into `tax.yaml` |
| user hand-editing `costbasis.yaml` | accepted by the loader but has no tax effect once `tax.yaml` exists |
| tests | `internal/costbasis/*_test.go` with CSV fixtures |

## See

- Source: `internal/costbasis/`
- Doc: `docs/superpowers/specs/2026-10-16-koinly-tax-design.md`
