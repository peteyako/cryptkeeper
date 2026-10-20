---
type: process
universe: live
status: verified
verified: 2026-10-20 @ 936c434
consumes: [source-def, source-contract, secret-key, price]
produces: [source-def, yaml-store, portfolio-view, database, snapshot, activity-entry]
---

# connect-source

Take a pasted address, key or holdings list from the add/edit form, prove it works, seal its secrets, write it to `data/sources.yaml`, and make the app fetch it; or remove it and everything that referenced it.

## Input → Movement → Output

Input: the `/sources/new/{type}` form (or the edit form), with a Test button and a Save button, plus the registry file and the secret box. Movement: the form is parsed into a `sources.Def` with plaintext secrets and refused if it looks like a seed phrase; Test builds the source and fetches it once; Save builds it again for validation, seals secrets and writes the YAML atomically, and the registry's change hook kicks both loops. Output: a new or updated entry in `data/sources.yaml` (secrets sealed), a timestamped copy of the previous file in `data/backups/`, a live source in the next refresh, and a redirect to `/w/{id}`. Delete reverses it, including portfolios, saved scans and activity.

## Why this shape

- The app only ever needs public data. `LooksLikeSeed` runs in the handler (friendly message) and again in `Def.Validate` (no path, including import, can store one).
- Test and Save share `parseSourceForm`, and an edit with blank secret fields keeps the stored ones, so editing a label never forces re-pasting an API secret and Test of an edit works without re-entering it.
- Save validates by building the source (parses keys/addresses, no network), so a malformed xpub is refused at the form and not discovered at the next refresh.
- Secrets are sealed with the install's key before they touch disk; the plaintext lives only in memory and in the request.
- `WriteAtomic` (temp file, fsync, rename, 0600) with a backup first: a crash mid-write cannot leave a half `sources.yaml`.
- Registry rebuilds a live instance only when the definition hash changes, and re-reads the file when hand-edited, so editing the YAML is as valid as using the form.

## Steps

1. `GET /sources/new` picks a type; `GET /sources/new/{type}` renders the form (unknown type redirects); edit renders values without secrets and shows masked secrets: `internal/server/source_handlers.go:26-66`.
2. Test button posts the form to `/sources/test`: `web/templates/js.go:119`. `sourceTest` parses the form with `parseSourceForm`: `internal/server/source_handlers.go:248-259`.
3. `parseSourceForm` reads per-type fields (Coinbase key name and private key, Kraken key and secret, Bitcoin xpub/descriptor/address/wallet-file/Ledger export, Solana address, manual/brokerage holdings lines) and returns field errors: `internal/server/source_handlers.go:91-201`.
3a. Solana's vault field has a Find button: `POST /sources/find-vault` posts the form's address (and the edited id) and builds a throwaway source. `FindJupiterVault` looks through the wallet's history for the Jupiter limit-order vault (the edited source's newest transfers first, at most 400 transactions) while the handler patches progress into `#vault-find`, then swaps in the vault input holding the address. It saves nothing; Save below stores `jupiterVault`: `internal/server/source_handlers.go:269-341`, `internal/sources/solana/findvault.go:15-73`, `web/templates/sources.templ:437-464`, `web/templates/js.go:114-116`. See [solana-source](../objects/sources/solana-source.md).
4. Seed-phrase refusal on `key`, `address`, `jupiterVault`: `internal/server/source_handlers.go:202-206`, `internal/sources/model.go:97-101`, `:192-195`.
5. `mergeForTest` fills blank secrets from the stored source (edit case); `testSource` builds the live source, fetches once with a 45s timeout, prices the result and returns a message with rows and warnings: `internal/server/source_handlers.go:227-370`.
6. Save posts to `/sources` (new) or `/sources/{id}`. A new source gets `UniqueID(label)`; then `Registry.Build` (the factory, with blank secrets filled from the stored source) validates without network: `internal/server/source_handlers.go:404-415`, `internal/sources/registry.go:337-346`, `:182`.
7. `Registry.Upsert` re-reads the file if changed, keeps stored secrets for blank fields, seals plaintext ones with the box, defaults the kind, preserves `CreatedAt`, runs `Def.Validate`: `internal/sources/registry.go:186-218`, `internal/sources/model.go:97-130`.
8. `writeLocked` encodes YAML and calls `yamlfile.WriteAtomic`, which first copies the existing file into `data/backups/` (keep 30), then writes via temp file and rename; then runs `onChange` in a goroutine: `internal/sources/registry.go:364-380`, `internal/yamlfile/yamlfile.go:47-77`.
9. `onChange` is wired at startup to kick the refresher and syncer; the save handler also kicks both explicitly, then redirects to `/w/{id}`: `main.go:509`, `internal/server/source_handlers.go:426-430`.
10. The next `Entries()` call builds the new live source through `a.factory` (hash changed) and the refresher fetches it ([refresh-balances](refresh-balances.md)); activity-capable ones backfill ([sync-activity](sync-activity.md)): `internal/sources/registry.go:107-135`, `main.go:261`.
11. Delete: `POST /sources/{id}/delete` (after a client `confirm()`) calls `Registry.Delete`, which rewrites the file the same way (its `onChange` kicks both loops; the handler kicks only the refresher), then redirects to `/`: `web/templates/js.go:183-184`, `internal/server/source_handlers.go:433-443`, `internal/sources/registry.go:322-334`.
12. `cleanupSource` removes the wallet from every portfolio, deletes the saved Bitcoin scan (`bitcoin.scan.<id>` kv), deletes the source's activity and `activity_state`, and invalidates the tax cache: `internal/server/settings_handlers.go:401-418`, `internal/server/portfolio_handlers.go:453-462`, `internal/activity/store.go:280-293`.
13. Hand edits to `sources.yaml` take the same route after step 8: mtime change on `Entries()` reloads and fires `onChange`: `internal/sources/registry.go:63-102`.

## If you change this

- **Hits:** [source-def](../objects/sources/source-def.md) fields (form, `formValues`, `Validate`, export/import, YAML), the per-type factory in `main.go:261` and [source-contract](../objects/sources/source-contract.md), [secret-key](../objects/storage/secret-key.md) (changing the key makes stored secrets unreadable: the source card shows the error), [backup-restore](backup-restore.md) which writes the same file through `Registry.Import`, the add-source templates and their field names.
- **Does not hit:** price lookup and the snapshot shape; stored activity of other sources. Deleting a source does not touch `data/backups/` history, its `lastgood_wallet` row, alert rules that name it, or `tax.yaml` entries for its wallet.

## Surfaces

| Surface | Role |
|---|---|
| `/sources/new`, `/sources/new/{type}`, `/sources/{id}/edit` | forms (chrome-free shell, full reload on navigation) |
| `POST /sources/test` | one-shot fetch, result patched into the form |
| `POST /sources/find-vault` | Solana only: the vault field's Find button; fills the field, saves nothing |
| `POST /sources`, `POST /sources/{id}` | save, redirect to `/w/{id}` |
| `POST /sources/{id}/delete` | delete, redirect to `/` |
| `data/sources.yaml` | the store; hand-editable |
| `cryptkeeper sources export/import` | CLI over the same registry |

## See

- Objects: [source-def](../objects/sources/source-def.md), [source-contract](../objects/sources/source-contract.md), [secret-key](../objects/storage/secret-key.md), [yaml-store](../objects/storage/yaml-store.md), [backup-file](../objects/storage/backup-file.md), [portfolio-view](../objects/views/portfolio-view.md)
- Source: `internal/server/source_handlers.go`, `internal/sources/registry.go`, `internal/sources/model.go`
