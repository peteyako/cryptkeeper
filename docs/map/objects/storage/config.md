---
type: object
cluster: storage
universe: live
status: verified
verified: 2026-10-19 @ 5d02073
entity: internal/config/config.go
---

# Config

Server settings from `config.yaml` (never written by the app) plus secrets read from environment variables, loaded once at startup into `config.Config` (package `internal/config`).

## Why this shape

Two sources with different jobs: operator tuning (refresh cadence, RPC URLs, ntfy) is a plain file you edit and restart; secrets are env-only (`yaml:"-"`) so they never land in a file the app could echo or back up (`internal/config/config.go:31-41`). Sources (wallets, exchanges) are deliberately not here; they live in `data/sources.yaml` ([yaml-store](yaml-store.md)) (`config.go:1-3`). A missing `config.yaml` is fine: defaults apply (`:147-162`).

## Shape

- Keys (`config.go:18-29`): `refreshMinutes` (5), `staleMaxHours` (24), `activityMinutes` (15), `showTokenLogos`, `bitcoin.{esploraUrl, esploraFallbackUrls, esploraFallbackUrl, gapLimit}` (mempool.space, gap 20), `solana.{rpcUrl, includeStakeAccounts, hideUnpriced, minUsd, hideMints, allowMints, tokenOverrides, defi}`, `ntfy.{url, redactAmounts}`, `activity.backfillDays` (365), `tax.timezone`, `seedSources`. Defaults: `Default()` (`:129-145`); zero or negative values fall back (`:164-187`).
- Strict decode: an unknown key in `config.yaml` is a startup error (`config.go:154-158`).
- Fallback servers for Esplora are resolved in `Load` (`esploraFallbacks`, `:77-95`); after Load `EsploraFallbackURL` is empty and the list holds all.
- `seedSources` are imported into `sources.yaml` only while it has no sources, i.e. on first run (`config.go:28`, `main.go:433-455`).
- Env vars (`config.go:188-203`): `CRYPTKEEPER_SECRET_KEY` (legacy fallbacks `CRYPTKEEP_SECRET_KEY`, `VAULT_SECRET_KEY`, newest wins), `PORTFOLIO_PASSWORD`, `PORTFOLIO_USER` (default `admin`), `NTFY_TOKEN`, `COINGECKO_API_KEY`, `BLOCKSTREAM_CLIENT_ID`, `BLOCKSTREAM_CLIENT_SECRET`.
- `LoadDotEnv` reads `.env` (`KEY=VALUE`, optional `export`, quotes) without overriding real env (`config.go:218-249`); called first in `setup` (`main.go:146`). Template: `.env.example`; keys template: `config.example.yaml`.
- `EncryptExports()` is true only when the user set `CRYPTKEEPER_SECRET_KEY`; the generated `data/secret.key` does not count, since a backup encrypted with a key beside the data would be lost with the disk (`config.go:213-216`).
- `TaxLocation()` turns `tax.timezone` into a `*time.Location` (`:52`).

### config.yaml vs the Settings page

| Setting | Where it lives |
|---|---|
| everything in the key list above | `config.yaml` only; edit and restart |
| logo toggle | default is `showTokenLogos`; the Settings toggle is stored in DB `kv` key `ui.settings` and wins (`internal/server/settings_handlers.go:20-25,38`) |
| CoinGecko key, Blockstream key | saved sealed in DB `kv` from Settings; the env var wins when set (`settings_handlers.go:111,179`; `main.go:801-846`) |
| secret key, password, ntfy token | env / `.env` only |

## Connected to

- **owns:** the `Config` struct and env names.
- **owned-by:** `main.go` `setup` (`main.go:146-151`), which hands `cfg` to every consumer.
- **joins:** [secret-key](secret-key.md) (`SecretKey`), [backup-file](backup-file.md) (`EncryptExports`), [database](database.md) (`kv` keys), [bitcoin-source](../sources/bitcoin-source.md) and [solana-source](../sources/solana-source.md) (URLs, gap limit, token filters), [evaluate-alerts](../../processes/evaluate-alerts.md) (ntfy).
- **looks-like-but-is-not:** [source-def](../sources/source-def.md) (`sources.yaml`, per-source); the Settings page is not a config editor.

## If you change this

- **Hits:** `config.example.yaml` and `.env.example`; `Load` defaults and tests (`internal/config/config_test.go`); each package reading the changed field, including server handlers that read `s.Cfg` directly (`guard.go:46-50`, `settings_handlers.go:42,60-63`, `alerts_handlers.go:89-90`, `activity_handlers.go:107,131-134`, `tax.go:133`).
- **Does not hit:** `data/sources.yaml` ([yaml-store](yaml-store.md)), except `seedSources` on first run; DB tables ([database](database.md)).

## Surfaces

| Surface | Role |
|---|---|
| main.go `setup` | `LoadDotEnv` then `Load` |
| server (`Deps.Cfg`) | reads; Settings page writes DB `kv`, never config.yaml |
| user | edits config.yaml, `.env` |
| tests | `internal/config/config_test.go` |

## See

- Source: `internal/config/config.go`
- Docs: `config.example.yaml`, `.env.example`
