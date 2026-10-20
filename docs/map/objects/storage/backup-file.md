---
type: object
cluster: storage
universe: live
status: verified
verified: 2026-10-20 @ 0a7dfb9
entity: internal/backup/backup.go
---

# Backup file

The one YAML file a user downloads to save everything they configured (package `internal/backup`, type `backup.File`), optionally with the whole database inside.

## Why this shape

Restoring a machine should be one file and one action. Every section is optional, so a sources-only or portfolios-only export restores through the same path (`internal/backup/backup.go:1-6`). Import validates all sections before writing any (`:177-250,279`), and the database goes last so a replace that removed sources still gets their activity back (`:357-364`). Not to be confused with the automatic copies in `data/backups/` ([yaml-store](yaml-store.md)).

## Shape

- `File` sections (`backup.go:35-46`): `version` (1), `sources`, `portfolios`, `alerts`, `costBasis`, `tax`, `taxYears`, `database` (gzip then base64 of a `store.DB.Snapshot`).
- `Stores` bundles the live stores; a nil store skips that section on export and rejects it on import (`:50-61,367-380`). `OnSourceRemoved` lets a replace drop the source's activity and portfolio refs (`:58-60`; wired `main.go:758-762`).
- `Export(st, withSecrets)` blanks exchange secrets unless asked; `ExportDatabase` adds the snapshot (`:65-97`). Saved API keys inside the database stay sealed either way (`:73-74`).
- `Parse` is strict (unknown fields are line-numbered errors), version-checked, and rejects an empty file (`:179-192`).
- `Import(st, raw, replace)`: merge by id (sources, portfolios, alerts), by asset plus identical-lot dedupe (cost basis), per-key overlay (tax settings), skip existing or earlier closed years (tax years); replace makes each present section exactly the file's; absent sections are untouched even with replace (`:270-365`; merge helpers `:382-470`). Database restore: `DB.Restore` (`internal/store/snapshot.go:43`).
- Restore size limit 512 MB on the web (`internal/server/settings_handlers.go:356`); unpacked database limit 2 GB (`backup.go:101`).
- Encryption: when `CRYPTKEEPER_SECRET_KEY` is set (`Config.EncryptExports`), the whole file is wrapped by `SealFile` and secrets are always kept sealed; import runs `OpenFile` first (`settings_handlers.go:311-350,380`; `main.go:734-752`). See [secret-key](secret-key.md).

## Connected to

- **owns:** the document format and import merge rules.
- **owned-by:** none; built from stores held by main.go `backupStores()` (`main.go:754`) and `server.backupStores` (`settings_handlers.go:300-306`).
- **joins:** [source-def](../sources/source-def.md), [portfolio-view](../views/portfolio-view.md), [alert-rule](../views/alert-rule.md), [manual-cost-basis](../ledger/manual-cost-basis.md), [tax-lot](../ledger/tax-lot.md), [database](database.md), [secret-key](secret-key.md), process [backup-restore](../../processes/backup-restore.md).
- **looks-like-but-is-not:** timestamped copies in `data/backups/` ([yaml-store](yaml-store.md)); [csv-export](../ledger/csv-export.md) and [tax-report](../ledger/tax-report.md) downloads; the older per-type `sources export` / `portfolios export` files (same envelope, narrower content; still importable).

## If you change this

- **Hits:** a new section means `File`, `Stores`, `build`, `Parse` (incl. its nothing-to-import check, `backup.go:190-192`), `Import`, `check`, `Result`/`Summary` (`backup.go:253,473`), the web outcome check (`settings_handlers.go:386`), and both `backupStores` constructors (`main.go:754`, `settings_handlers.go:300`); `invalidateTax` after tax or database restore (`settings_handlers.go:397-398`). A format change needs a `Version` bump (`backup.go:32`).
- **Does not hit:** `data/backups/` retention ([yaml-store](yaml-store.md)); per-section store logic stays in the owning package's `Import`; database tables (restore is generic, see [database](database.md)).

## Surfaces

| Surface | Role |
|---|---|
| web `GET /backup/export?secrets=1&db=1`, `POST /backup/import` | download / restore (`internal/server/server.go:193-194`) |
| CLI `backup export [-o] [-secrets] [-db]`, `backup import FILE [-replace]` | same via main.go (`main.go:676-730`) |
| CLI `sources` / `portfolios export/import` | narrower exports (`main.go:543,606`); web `/portfolios/export`, `/portfolios/import` (`server.go:140,144`) |
| tests | `internal/backup/backup_test.go`, `database_test.go`, `writeoffs_test.go` |

## See

- Source: `internal/backup/backup.go`
