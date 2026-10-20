---
type: process
universe: live
status: verified
verified: 2026-10-20 @ 0a7dfb9
consumes: [source-def, portfolio-view, alert-rule, manual-cost-basis, tax-lot, yaml-store, database, secret-key, config]
produces: [backup-file, source-def, portfolio-view, alert-rule, manual-cost-basis, tax-lot, yaml-store, database]
---

# backup-restore

Export every user-configured store (and optionally the database) as one YAML file, encrypted as a whole when a secret key is set; or read such a file back, validate all of it first, and write each section it contains, merging or replacing.

## Input → Movement → Output

Input: for export, the stores and flags (`secrets`, `db`); for import, an uploaded or CLI-named file and a mode (merge or replace). Movement: export gathers each section, optionally attaches a gzip+base64 SQLite snapshot, and seals the file if `CRYPTKEEPER_SECRET_KEY` is set; import decrypts, parses and validates every section, and only then applies sections in a fixed order, finishing with the database. Output: a `cryptkeeper-backup*.yaml` download (or file/stdout from the CLI), or changed stores plus a one-line summary, with refresh and sync kicks.

## Why this shape

- One file, optional sections: a sources-only or portfolios-only export from an older build restores through the same path, and a section absent from the file is never touched, even with replace.
- Parse everything before writing anything: a typo in the alerts section must not leave sources replaced and the rest stale. (A failure after writing begins stops there and the summary says what was done.)
- Secrets stay sealed even when exported (`-secrets`): the file is only restorable with the same key, so a leaked backup is not a leaked API key. Without the flag they are blanked, and restored exchange sources need keys re-entered.
- Whole-file encryption when the key exists, always with secrets: the tax history and holdings are sensitive too, not only API keys. `OpenFile` passes plain files through unchanged, so older plain exports keep importing.
- Database last: a replace that removed sources (and their activity) then gets the backup's activity back instead of losing it to the cleanup hook.
- The database comes from a consistent snapshot taken while the app runs, not a file copy of a live WAL database.

## Steps

1. Settings has a GET form to `/backup/export` and a multipart form posted to `/backup/import`: `web/templates/settings.templ:382`, `:409`, `web/templates/js.go:412`. CLI: `backup export [-o] [-secrets] [-db]` and `backup import FILE [-replace]`: `main.go:676-732`.
2. Export decides: encrypted when `EncryptExports()` (secret key set), which forces secrets on; `?secrets=1` keeps sealed secrets; `?db=1` adds the database: `internal/server/settings_handlers.go:311-314`, `internal/config/config.go:227`.
3. `build` collects sources (via `Registry.Export`), portfolios, alert rules, non-empty cost basis, non-empty tax settings and every closed tax year: `internal/backup/backup.go:124-169`.
4. With the database, `DB.Snapshot` is gzipped and base64-encoded into the `database` field: `internal/backup/backup.go:75-97`, `internal/store/snapshot.go:24`.
5. `sealExport` wraps the YAML in the `cryptkeeper: encrypted` envelope (name gets `-encrypted`); the download is `no-store`: `internal/server/settings_handlers.go:326-351`, `internal/secrets/file.go:28-35`. The CLI writes the same bytes to stdout or a 0600 file: `main.go:702-709`.
6. Import reads at most 512 MB (larger is refused, not truncated) from the multipart `file`: `internal/server/settings_handlers.go:353-378`.
7. `OpenFile` decrypts an envelope (clear errors for a wrong or missing key) or returns plain bytes unchanged: `internal/server/settings_handlers.go:380`, `internal/secrets/file.go:40-59`.
8. `backup.Parse` decodes strictly, rejects other versions and empty files, and validates every section (source defs, portfolios, alert rules, cost basis, tax settings, tax years with no duplicate year): `internal/backup/backup.go:179-250`.
9. `Import` checks the file's sections have stores, unpacks the database up front (2 GB cap), then applies in order: sources (merge by id; replace deletes missing ones and calls `OnSourceRemoved` for each), portfolios, alerts, cost basis, tax settings, tax years: `internal/backup/backup.go:274-356`.
10. `OnSourceRemoved` (server) is `cleanupSource`: drops portfolio references, saved Bitcoin scan, activity: `internal/server/settings_handlers.go:300-306`, `:441-458`. The CLI hook drops portfolio references and activity only: `main.go:758-761`.
11. Last, `DB.Restore` merges or replaces the database: `internal/backup/backup.go:357-363`.
12. The handler words the outcome from `Result.Summary`, invalidates the tax cache if tax data or the database changed, and kicks the refresher and syncer: `internal/server/settings_handlers.go:380-409`.
13. Separately, every store save writes a timestamped copy of the old file into `data/backups/` (see [backup-file](../objects/storage/backup-file.md)); that is not an export and restore does not read it: `internal/yamlfile/yamlfile.go:47-55`.

## If you change this

- **Hits:** the `backup.File` schema and `Version` (every old export must still parse), each section's own import (`Registry.Import`, `Store.Import`, `RuleStore.Import`, tax stores), [secret-key](../objects/storage/secret-key.md) and the envelope format, `cleanupSource`, the Settings backup UI, and the CLI twin in `main.go`.
- **Does not hit:** the automatic `data/backups/` copies; CSV exports ([csv-export](../objects/ledger/csv-export.md)); tax reports. The sources-only and portfolios-only CLI exports (`sources export`, `portfolios export`) are separate commands over the same stores, though the same envelope applies.

## Surfaces

| Surface | Role |
|---|---|
| Settings -> Backup | download (secrets and database options) and restore (merge or replace) |
| `GET /backup/export`, `POST /backup/import` | the two routes |
| `cryptkeeper backup export` / `import` | CLI |
| `CRYPTKEEPER_SECRET_KEY` | turns on whole-file encryption |

## See

- Objects: [backup-file](../objects/storage/backup-file.md), [secret-key](../objects/storage/secret-key.md), [yaml-store](../objects/storage/yaml-store.md), [database](../objects/storage/database.md), [source-def](../objects/sources/source-def.md), [portfolio-view](../objects/views/portfolio-view.md), [alert-rule](../objects/views/alert-rule.md), [manual-cost-basis](../objects/ledger/manual-cost-basis.md), [config](../objects/storage/config.md)
- Source: `internal/backup/backup.go`, `internal/server/settings_handlers.go`, `main.go`
