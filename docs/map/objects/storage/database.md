---
type: object
cluster: storage
universe: live
status: verified
verified: 2026-10-20 @ 0a7dfb9
entity: internal/store/store.go
---

# Database

The SQLite file `data/cryptkeeper.db` (package `internal/store`, type `store.DB`) holds all machine-written state; human-editable state lives in YAML instead ([yaml-store](yaml-store.md)).

## Why this shape

Caches, history and sync state are written by the app every few minutes and must survive restarts, but nobody edits them by hand, so they go in SQLite and not in YAML (`internal/store/store.go:1-3`). One writer connection (`SetMaxOpenConns(1)`) with WAL and a 5 s busy timeout keeps SQLite simple and avoids SQLITE_BUSY (`internal/store/store.go:36-40`). The first six tables are created by `store`'s own `001-base` migration (`internal/store/store.go:25-32`); every table added since is registered by its owning package with `store.AddMigration` from `init()`, so a new table touches the owner, not a central schema file (`internal/store/store.go:101-110`). Migrations are tracked by name in `schema_migrations`, so Go init order never matters (`internal/store/store.go:20-23,48-97`).

## Shape

- Open/migrate: `store.Open` (`internal/store/store.go:35`), `migrate` (`:48`). Never edit a released migration; add a new one with a new name (`:101-102`).
- First release used a `schema_version` counter; `migrate` maps it onto `001-base` (`internal/store/store.go:69-77`).
- Typed JSON helpers on the `kv` table: `GetJSON`, `PutJSON`, `DeleteKV` (`internal/store/store.go:113-140`).
- Whole-file copy for backups: `DB.Snapshot` (VACUUM INTO a temp file, `internal/store/snapshot.go:24`) and `DB.Restore` (`:43`): the snapshot is first migrated to this build's schema (`:59`), then every table both files have is copied by shared columns (`:175`), merging by primary key (`INSERT OR IGNORE`) or, with replace, after emptying the table; `schema_migrations` and `sqlite_sequence` are skipped (`notRestored`, `:20`).
- Legacy file name move: `dbName = "cryptkeeper.db"`, `legacyDBNames = {cryptkeep.db, vault.db}`; `renameLegacyDB` moves the file plus `-wal`/`-shm` (main file last) once, only if `dbName` does not exist yet (`main.go:102-136`), called in `setup` before `store.Open` (`main.go:165-168`).

### Table to owner

| Table | Created by (migration) | Read/written by | Holds | Card |
|---|---|---|---|---|
| `schema_migrations` | `migrate` (`store.go:49`) | store | applied migration names | this card |
| `kv` | `001-base` (`store.go:26`) | server, main.go (keys below) | JSON settings blobs | [config](config.md) |
| `lastgood_price` | `001-base` (`store.go:27`) | prices (`prices.go:442-485`) | last live price per asset, for stale fallback | [price](../valuation/price.md) |
| `lastgood_wallet` | `001-base` (`store.go:28`) | portfolio aggregator (`aggregate.go:349,358`) | last good result per source (JSON) | [snapshot](../valuation/snapshot.md) |
| `history` | `001-base` (`store.go:29`) | portfolio (`history.go:65,76,92`, `backfill.go:273`) | value over time: ts, prices, amounts | [value-history](../valuation/value-history.md) |
| `token_meta` | `001-base` (`store.go:30`) | solana (`sources/solana/metacache.go:52,72`); server reads (`taxview.go:129`) | SPL token metadata cache | [solana-source](../sources/solana-source.md) |
| `price_history` | `001-base` (`store.go:31`) | prices (`history.go:245-276`; `stocks.go:346`) | daily USD closes (asset, day) | [price](../valuation/price.md) |
| `price_unknown` | `prices-unknown-v1` (`prices/coingecko.go:17`) | prices (`coingecko.go:215-263`) | assets an upstream does not know, until-time | [price](../valuation/price.md) |
| `activity` | `activity-v1` (`activity/store.go:22-33`) | activity `Store` | normalized ledger entries, JSON plus filter columns | [activity-entry](../ledger/activity-entry.md) |
| `activity_state` | `activity-v1` (`activity/store.go:34`) | activity `Store` | per-source sync cursor, backfill flag, last error | [activity-entry](../ledger/activity-entry.md) |
| `alert_state` | `alerts-v1` (`alerts/state.go:19-25`) | alerts engine | per-rule engine state (baselines, cooldowns) | [alert-rule](../views/alert-rule.md) |
| `alert_log` | `alerts-v1` (`alerts/state.go:26`) | alerts engine | fired alerts and delivery errors | [alert-rule](../views/alert-rule.md) |

`kv` keys: `ui.settings` (`internal/server/settings_handlers.go:32`), `prices.coingeckoKey` and `bitcoin.blockstreamKey` (sealed, `:166`, `:206`; read by `main.go:801-846` too), `bitcoin.scan.<walletID>` (saved scans: constant `:443`, written by `main.go:795`), `tax.koinly.draft` (`internal/server/koinly_handlers.go:34`). There is no `extra` table in the app: it appears only in a test (`internal/store/store_test.go:39`).

## Connected to

- **owns:** the file, the migration runner, the `001-base` schema and the `kv` helpers; each later table's schema belongs to its package.
- **owned-by:** `main.go` opens it once in `setup` and passes it to every consumer.
- **joins:** [backup-file](backup-file.md) (optional `database:` section is a `Snapshot`), [secret-key](secret-key.md) (sealed values inside `kv`).
- **looks-like-but-is-not:** [yaml-store](yaml-store.md) (user-edited config); `store.DB.Snapshot` (SQLite file copy) is not the priced [snapshot](../valuation/snapshot.md); `activity.Store` is a SQLite store despite the `*Store` name.

## If you change this

- **Hits:** a new named migration (`AddMigration` in the owning package's `init()`, or a new entry after `001-base` in `store.go` for a base table); the package that reads/writes the table; `DB.Restore` copies shared columns only, after migrating the snapshot, so a column a newer build added is dropped when its backup is restored into an older build (`internal/store/snapshot.go:59,175`); a table that must never be restored goes in `notRestored` (`snapshot.go:20`).
- **Does not hit:** `data/*.yaml` stores ([yaml-store](yaml-store.md)); `config.yaml` keys ([config](config.md)); `internal/backup` (it carries the snapshot bytes without knowing the tables).

## Surfaces

| Surface | Role |
|---|---|
| main.go `setup` | renames legacy file, opens, migrates |
| prices, portfolio, activity, alerts, solana packages | read and write their own tables |
| server handlers, main.go key/scan helpers | read and write `kv`; server reads `token_meta` |
| backup export/import | `Snapshot` / `Restore` |
| tests | `store_test.go` registers throwaway migrations |

## See

- Source: `internal/store/store.go`, `internal/store/snapshot.go`
