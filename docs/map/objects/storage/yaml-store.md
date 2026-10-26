---
type: object
cluster: storage
universe: live
status: verified
verified: 2026-10-26 @ be0017a
entity: internal/yamlfile/yamlfile.go
---

# YAML store

The human-editable files under `data/` (package `internal/yamlfile` is the shared read/write layer; each file has its own owning package and store type).

## Why this shape

The user hand-edits these files, so a typo must fail loudly with a line number and a bad save must never leave a half-written file. Reads use strict decoding (unknown fields are errors, `KnownFields(true)`) (`internal/yamlfile/yamlfile.go:20-28`). Writes go through a temp file, fsync, chmod 0600 and rename (`:47-77`). Before overwriting, the old content is copied into a backup directory so a mistaken save is recoverable (`:79-97`).

## Shape

- `Decode` strict decode, EOF (empty file) is fine (`internal/yamlfile/yamlfile.go:21`); `Encode` 2-space indent (`:31`).
- `WriteAtomic(path, data, backupDir, keep)` makes the backup first, then the atomic write; empty `backupDir` skips backups (`:47-55`).
- Backup copy name `<base>-YYYYMMDD-HHMMSS.mmm.yaml` in `data/backups/` (mode 0700 dir, 0600 files); `prune` keeps the newest `keep` per base name, `keep <= 0` keeps all (`:92,151-164`).
- `ListBackups` / `ReadBackup` (name must come from the listing, which blocks path traversal) (`:107,138`).
- All stores share one `data/backups/` directory, wired in `main.go:176-201`.

### Files that use it

| File | Owning package (save line) | Retention |
|---|---|---|
| `data/sources.yaml` | sources `Registry` (`registry.go:369`) | 30 (literal) |
| `data/portfolios.yaml` | portfolios `Store` (`store.go:165`) | `backupsKept = 30` (`store.go:18`) |
| `data/alerts.yaml` | alerts `RuleStore` (`rulestore.go:136`) | `backupsKept = 30` (`rulestore.go:16`) |
| `data/tax.yaml` | tax `SettingsStore` (`settings.go:237`) | `settingsBackups = 20` (`settings.go:18`) |
| `data/tax/<year>.yaml` | tax `YearStore` (`years.go:326,388`; bare `Backup` before a delete, `:344,378`) | `yearBackups = 20` (`years.go:20`) |
| `data/costbasis.yaml` | costbasis `ManualStore` (`manual.go:285`) | `manualBackups = 20` (`manual.go:23`) |

`costbasis.yaml` is partly leftover: its manual lots move into `tax.yaml` once, while `tax.yaml` does not yet exist (`main.go:209-235`), but the store still loads and backs up per-asset entries. Neither `config.yaml` (read with its own strict decoder, `internal/config/config.go:156`) nor the encrypted export envelope (`internal/secrets/file.go:42`) goes through this package.

## Connected to

- **owns:** the read/write/backup mechanics only.
- **owned-by:** [source-def](../sources/source-def.md) (`sources.yaml`), [portfolio-view](../views/portfolio-view.md) (`portfolios.yaml`), [alert-rule](../views/alert-rule.md) (`alerts.yaml`), [tax-lot](../ledger/tax-lot.md) (`tax.yaml`, `tax/<year>.yaml`), [manual-cost-basis](../ledger/manual-cost-basis.md) (`costbasis.yaml`).
- **joins:** [backup-file](backup-file.md) reuses `Decode`/`Encode` for the one-file export.
- **looks-like-but-is-not:** [backup-file](backup-file.md) (user-downloaded export) versus these automatic copies in `data/backups/`; [database](database.md) (machine-written state); `activity.Store`, which is SQLite, not YAML.

## If you change this

- **Hits:** every store listed above (shared signature `WriteAtomic`); strictness change breaks hand-edited files and [backup-file](backup-file.md) `Parse`; backup naming change breaks `ListBackups`, and the portfolios restore list (`internal/server/portfolio_handlers.go:234`).
- **Does not hit:** [database](database.md) tables; [config](config.md) `config.yaml`.

## Surfaces

| Surface | Role |
|---|---|
| owning stores (main.go `setup`) | load at start, `WriteAtomic` on every save |
| web handlers (portfolios restore, `server.go:145`) | list and restore a backup copy |
| user | hand-edits files; a parse error names the line |
| tests | `internal/yamlfile/yamlfile_test.go` |

## See

- Source: `internal/yamlfile/yamlfile.go`
