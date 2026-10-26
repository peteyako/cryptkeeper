---
type: object
cluster: storage
universe: live
status: verified
verified: 2026-10-26 @ be0017a
entity: internal/secrets/secrets.go
---

# Secret key

The one AES-256-GCM key (type `secrets.Box`, package `internal/secrets`) that seals exchange API secrets and saved API keys at rest and, when the user supplies the key, whole exports.

## Why this shape

It protects a leaked `sources.yaml` or backup, not someone who can also read the key file (`internal/secrets/secrets.go:3-6`). The key comes from the env var when set, else a file generated once with mode 0600, so a default install needs no setup (`:42-73`). Plaintext values still `Open` unchanged so a hand-edited `sources.yaml` works and is re-sealed on save (`:88-89`).

## Shape

- Key source: `LoadKey(envKey, keyFile)`: base64 of 32 bytes from `CRYPTKEEPER_SECRET_KEY` (legacy names resolved in config, see [config](config.md)), else `data/secret.key`, created if missing (`internal/secrets/secrets.go:43-73`; called `main.go:157`).
- `NewBox` requires exactly 32 bytes (`secrets.go:27-30`).
- Sealed value format `enc:v1:<base64(nonce||ciphertext)>`, with the prefix as AEAD additional data (`:21,84-85`). `Seal("")` stays empty (`:76-79`); `IsSealed` (`:111`).
- `Open` errors read "wrong key or corrupted value" (`:105`).
- Whole-file envelope: `SealFile` writes YAML `cryptkeeper: encrypted`, `version: 1`, `data: "enc:v1:..."`; `OpenFile` passes non-envelope files through unchanged, accepts the old `cryptkeep:` tag, and with a nil Box refuses an encrypted file (`internal/secrets/file.go:10-59`).
- `Mask` shows the last 4 characters, e.g. `••••3f9a` (`secrets.go:114`).

### Who holds the Box

| Holder | Use |
|---|---|
| `app.box` in main.go (`main.go:89,161`) | CLI export `SealFile`, import `OpenFile` (`main.go:735-752`), key helpers (`main.go:801,820`) |
| `sources.Registry` (`internal/sources/registry.go:32,49`) | seals and opens exchange secrets in `sources.yaml` (`:171,201,266`) |
| `server.Deps.Box` (`internal/server/server.go:44`) | seals/opens saved CoinGecko and Blockstream keys in DB `kv` (`settings_handlers.go:177,194,226,293`), export seal (`:349`), import open (`:380`; `portfolio_handlers.go:454`); `secrets.Mask` for the UI (`settings_handlers.go:182,248`; `source_handlers.go:54`) |

## Connected to

- **owns:** the sealing format and key loading.
- **owned-by:** `main.go` `setup` builds it from [config](config.md) `SecretKey`.
- **joins:** [source-def](../sources/source-def.md) (sealed secret fields), [yaml-store](yaml-store.md) (`sources.yaml` content), [database](database.md) (sealed `kv` values), [backup-file](backup-file.md) (encrypted export), [connect-source](../../processes/connect-source.md).
- **looks-like-but-is-not:** `PORTFOLIO_PASSWORD` (HTTP basic auth, not encryption); `Config.EncryptExports()` (is the env var set) is not "a key exists".

## If you change this

- **Hits:** every sealed value on disk (`sources.yaml`, `kv` keys `prices.coingeckoKey`, `bitcoin.blockstreamKey`), every backup made with `-secrets` and every encrypted export: a new key cannot open old ones. Format changes need a new prefix version and `OpenFile` version handling.
- **Does not hit:** `PORTFOLIO_PASSWORD`; the `ui.settings` kv entry (not sealed).

## Surfaces

| Surface | Role |
|---|---|
| main.go `setup` | loads key, builds Box |
| registry, server, CLI backup/sources commands | Seal / Open / SealFile / OpenFile |
| user | sets `CRYPTKEEPER_SECRET_KEY` (`openssl rand -base64 32`) or backs up `data/secret.key` |
| tests | `internal/secrets/secrets_test.go`, `file_test.go` |

## See

- Source: `internal/secrets/secrets.go`, `internal/secrets/file.go`
- Docs: `.env.example`
