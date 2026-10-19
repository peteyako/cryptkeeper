---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/sources/registry.go
---

# Source definition (`sources.Def`, `sources.Registry`)

A "source" (UI word: wallet) as the user configures it is a `sources.Def` row in `data/sources.yaml`; the `Registry` owns that file and turns each row into a live `core.Source` through a factory.

## Why this shape

The file is human-editable YAML, so the registry treats the disk as the truth: it re-reads on mtime change instead of caching forever, and keeps serving the last good file when a hand edit is broken (`internal/sources/registry.go:110`). Secrets are sealed in the same file (`enc:v1:…`) so the file is safe to back up and export; live sources are built only from an *opened* copy and rebuilt only when the def's hash changes, so editing a description never restarts a wallet's sync (`internal/sources/registry.go:385-392`). Type-specific parsing (keys, addresses) is deliberately NOT in `Validate`: it happens when the factory builds the source, so a bad row shows as `BuildErr` on its card instead of blocking the whole file (`internal/sources/model.go:95-96`).

## Shape

- `Def` is a flat union: shared `id/type/label/kind/description/createdAt`, then per-type fields (coinbase `keyName`+`privateKey`; kraken `apiKey`+`apiSecret`; bitcoin `key`/`descriptor`/`scriptType`/`importedVia`; solana `address`+`jupiterVault`; manual and brokerage `holdings`) (`internal/sources/model.go:40-69`).
- Type constants and the display-order list `Types` (`internal/sources/model.go:19-33`); `HandEntered` is true for manual and brokerage, which have no activity (`internal/sources/model.go:37`); `DefaultKind` maps type to custody kind (`internal/sources/model.go:154`).
- Secret fields: `privateKey`, `apiKey`, `apiSecret` only (`internal/sources/model.go:78`). Sealed via `secrets.Box` in `Upsert`/`Import`; blank on an existing id keeps the stored value (`internal/sources/registry.go:194-207`, `260-272`).
- `Validate`: seed-phrase/`xprv` refusal on key, descriptor, address, jupiterVault (`LooksLikeSeed`: 12/18/24 lowercase words or `xprv/yprv/zprv`/`prv(`) (`internal/sources/model.go:98-102`, `195-211`); id regex `^[a-z0-9][a-z0-9-]{0,39}$` (`:93,103`); label required; description <= 280 chars (`:30,109`); kind in the five `core.Kind`s; per-type required fields; brokerage assets via `brokerage.ValidAsset` (`:143`). Coinbase/Kraken secrets may be missing (import without secrets).
- `Registry`: `Entries()` returns `Entry{Def, Source, BuildErr}` per row in file order, building lazily and caching by `defHash` (`internal/sources/registry.go:107-135`); `Get/Defs` return sealed defs; `Open` decrypts; `Build` makes a source from a plaintext def for "Test connection" (`:182`); `Upsert`, `Import(merge|replace)`, `Delete`, `UniqueID`, `Export(includeSecrets)` (`:186,242,322,337,349`). Writes go through `yamlfile.WriteAtomic` with 30 backups (`:369`).
- Hand-edit reload: `reloadLocked` compares file mtime on every `Entries/Get/Defs/Upsert` call and fires `onChange` (wired to refresher+syncer kick) (`internal/sources/registry.go:63-103`, `main.go:509`). It rejects duplicate ids and defaults a blank `kind`, but does NOT call `Validate`: only saves and imports validate.
- Factory: `sources.Factory func(Def) (core.Source, error)` is supplied by `app.factory` in `main.go:262-293`, a switch on `Def.Type`; unknown type errors. Coinbase/Kraken without secrets fail with "needs its API ... key" (`main.go:265-272`).

## Connected to

- **owns:** the built `core.Source` instances ([source-contract](source-contract.md)); per-type builders [bitcoin-source](bitcoin-source.md), [solana-source](solana-source.md), [coinbase-source](coinbase-source.md), [kraken-source](kraken-source.md), [manual-source](manual-source.md), [brokerage-source](brokerage-source.md).
- **owned-by:** [yaml-store](../storage/yaml-store.md) (file + backups); sealing by [secret-key](../storage/secret-key.md).
- **joins:** [portfolio-view](../views/portfolio-view.md) references source ids; [snapshot](../valuation/snapshot.md) and [activity-entry](../ledger/activity-entry.md) key rows by `Def.ID`; [server](../web/server.md) add/edit/test handlers.
- **looks-like-but-is-not:** `portfolio.WalletView` (priced row in a snapshot); the "Wallet" UI word covers exchanges too.

## If you change this

- **Hits:** a new field or type: `main.go` factory switch, `internal/server/source_handlers.go` (`formValues`, `parseSourceForm`, `testSource`), `web/templates` source form, `Validate`, `Types`/`DefaultKind`, demo fixtures ([demo-fixtures](../web/demo-fixtures.md)). A new secret field: `SecretFields` + `Secret()` switch. `defHash` input: any non-description field restarts that source.
- **Does not hit:** `config.yaml` (global node/RPC settings; see [config](../storage/config.md)); `portfolios.yaml`.

## Surfaces

| Surface | Role |
|---|---|
| Server source handlers (`internal/server/source_handlers.go`) | Upsert, Delete, Build for Test, Get/Open for edit forms |
| Settings import/export (`internal/server`) | `Import`, `Export` |
| Refresher / syncer (`main.go`) | `Entries()`, `onChange` kick |
| User hand-editing `data/sources.yaml` | reload on mtime; no `Validate` |
| Tests | `internal/sources/registry_test.go` |

## See

- Source: `internal/sources/registry.go`, `internal/sources/model.go`
