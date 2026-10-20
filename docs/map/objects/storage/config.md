---
type: object
cluster: storage
universe: live
status: verified
verified: 2026-10-20 @ ea0d7ef
entity: internal/config/config.go
---

# Config

Server settings from `config.yaml` (never written by the app) plus secrets read from environment variables, loaded once at startup into `config.Config` (package `internal/config`).

## Why this shape

Two sources with different jobs: operator tuning (refresh cadence, RPC URLs, ntfy) is a plain file you edit and restart; secrets are env-only (`yaml:"-"`) so they never land in a file the app could echo or back up (`internal/config/config.go:32-42`). Sources (wallets, exchanges) are deliberately not here; they live in `data/sources.yaml` ([yaml-store](yaml-store.md)) (`config.go:1-3`). A missing `config.yaml` is fine: defaults apply (`:149-164`).

## Shape

- Keys (`config.go:18-31`): `refreshMinutes` (5), `priceMinutes` (1), `staleMaxHours` (24), `activityMinutes` (15), `showTokenLogos`, `bitcoin.{esploraUrl, esploraFallbackUrls, esploraFallbackUrl, gapLimit}` (mempool.space, gap 20), `solana.{rpcUrl, includeStakeAccounts, hideUnpriced, minUsd, hideMints, allowMints, tokenOverrides, defi}`, `ntfy.{url, redactAmounts}`, `activity.backfillDays` (365), `tax.timezone`, `seedSources`. `refreshMinutes` is how often balances are fetched and `priceMinutes` how often prices alone are refreshed between balance refreshes; the Settings page can override both (numbers `refreshMinutes` and `priceMinutes` stored in the database `kv` key `ui.settings`; balances held to at least 1, prices to at most the balance interval, a saved prices 0 meaning "with the balances"; applied at start, after a restore and after a change; a reset returns to config.yaml's) (`internal/server/settings_handlers.go:22-31`, `:51-74`, `:121-142`); `PriceInterval()` is `min(priceMinutes, refreshMinutes)`, so prices never come less often than balances (`config.go:214-218`; `Refresher.PriceInterval`, [refresh-balances](../../processes/refresh-balances.md)). Defaults: `Default()` (`:130-147`); zero or negative values fall back (`:166-192`, `priceMinutes` at `:169-171`).
- Strict decode: an unknown key in `config.yaml` is a startup error (`config.go:156-160`).
- Fallback servers for Esplora are resolved in `Load` (`esploraFallbacks`, `:78-96`); after Load `EsploraFallbackURL` is empty and the list holds all.
- `seedSources` are imported into `sources.yaml` only while it has no sources, i.e. on first run (`config.go:30-31`, `main.go:433-455`).
- Env vars (`config.go:193-208`): `CRYPTKEEPER_SECRET_KEY` (legacy fallbacks `CRYPTKEEP_SECRET_KEY`, `VAULT_SECRET_KEY`, newest wins), `PORTFOLIO_PASSWORD`, `PORTFOLIO_USER` (default `admin`), `NTFY_TOKEN`, `COINGECKO_API_KEY`, `BLOCKSTREAM_CLIENT_ID`, `BLOCKSTREAM_CLIENT_SECRET`.
- `LoadDotEnv` reads `.env` (`KEY=VALUE`, optional `export`, quotes) without overriding real env (`config.go:229-260`); called first in `setup` (`main.go:146`). Template: `.env.example`; keys template: `config.example.yaml`.
- `EncryptExports()` is true only when the user set `CRYPTKEEPER_SECRET_KEY`; the generated `data/secret.key` does not count, since a backup encrypted with a key beside the data would be lost with the disk (`config.go:224-227`).
- `TaxLocation()` turns `tax.timezone` into a `*time.Location` (`:53`).

### config.yaml vs the Settings page

| Setting | Where it lives |
|---|---|
| everything in the key list above | `config.yaml` only; edit and restart |
| balances and prices refresh intervals | defaults are `refreshMinutes` and `priceMinutes`; the Settings "Refresh" card ("Refresh balances", "Refresh prices") is stored in DB `kv` key `ui.settings` as `refreshMinutes` and `priceMinutes` and wins (`settings_handlers.go:51-74`, `web/templates/settings.templ:192-204`). The wallets chip, a source's retry note and `/healthz` follow the interval in effect (`Refresher.RefreshEvery`), not config.yaml's |
| logo toggle | default is `showTokenLogos`; the Settings toggle is stored in DB `kv` key `ui.settings` and wins (`internal/server/settings_handlers.go:22-31,44`) |
| CoinGecko key, Blockstream key | saved sealed in DB `kv` from Settings; the env var wins when set (`settings_handlers.go:167,235`; `main.go:801-846`) |
| secret key, password, ntfy token | env / `.env` only |

## Connected to

- **owns:** the `Config` struct and env names.
- **owned-by:** `main.go` `setup` (`main.go:146-151`), which hands `cfg` to every consumer.
- **joins:** [secret-key](secret-key.md) (`SecretKey`), [backup-file](backup-file.md) (`EncryptExports`), [database](database.md) (`kv` keys), [bitcoin-source](../sources/bitcoin-source.md) and [solana-source](../sources/solana-source.md) (URLs, gap limit, token filters), [evaluate-alerts](../../processes/evaluate-alerts.md) (ntfy).
- **looks-like-but-is-not:** [source-def](../sources/source-def.md) (`sources.yaml`, per-source); the Settings page is not a config editor.

## If you change this

- **Hits:** `config.example.yaml` and `.env.example`; `Load` defaults and tests (`internal/config/config_test.go`); each package reading the changed field, including server handlers that read `s.Cfg` directly (`guard.go:46-50`, `settings_handlers.go:48,61,91-94`, `alerts_handlers.go:89-90`, `activity_handlers.go:107,131-134`, `tax.go:133`).
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
