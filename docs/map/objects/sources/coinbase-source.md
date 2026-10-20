---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-20 @ 936c434
entity: internal/sources/coinbase/coinbase.go
---

# Coinbase source (`sources.TypeCoinbase`, `coinbase.Source`)

A read-only Coinbase account, authenticated with a view-only CDP API key (key name plus private key), whose balances come from two APIs reconciled by account id and whose activity comes from the v2 transaction list.

## Why this shape

- Neither API alone is complete. Advanced Trade lists trading balances; the v2 API also lists vaults and staking wallets. Fetch calls both, merges by account id, and lets either fail alone (a warning, not an error); only both failing is an error `internal/sources/coinbase/coinbase.go:498-549`.
- Where both list an account, v2 wins (balance, code, type) and a difference is surfaced in Meta, not hidden `internal/sources/coinbase/coinbase.go:560-584`, `internal/sources/coinbase/coinbase.go:540-542`.
- Staking and vaults are flags on a holding, not separate assets: the asset stays `BTC`/`ETH` (so it prices and merges with other sources) and `Staked`/`Vault` mark where it sits `internal/sources/coinbase/coinbase.go:631-695`, `internal/sources/coinbase/coinbase.go:615-628`.
- Every request carries a fresh single-use JWT, including httpx retries, because CDP requires one per request `internal/sources/coinbase/coinbase.go:258-280`.

## Shape

- **Registration:** `TypeCoinbase` `internal/sources/model.go:20`; Def fields `KeyName`, `PrivateKey` (secret, sealed) `internal/sources/model.go:50-51`, `internal/sources/model.go:78`; Validate requires only `keyName`, a missing private key is allowed so an import-without-secrets loads and the card asks for it `internal/sources/model.go:118-123`; default kind exchange `internal/sources/model.go:156`. Factory `main.go:264-268` refuses an empty private key with the "edit this source" message.
- **Auth:** `parseKey` accepts an ECDSA P-256 PEM (SEC1, PKCS8 fallback, `\n`-escaped input forgiven) or base64 of a 64-byte Ed25519 key `internal/sources/coinbase/coinbase.go:142-157`. `BuildJWT`: header `kid`+`nonce`, claims `sub`, `iss=cdp`, `nbf`, `exp` (+120 s), `uri="METHOD host path"` with the query dropped; Ed25519 adds `aud` `internal/sources/coinbase/coinbase.go:230-255`. The v2 API is documented for ECDSA only; an Ed25519 key that gets 401/403 on v2 yields a warning saying so `internal/sources/coinbase/coinbase.go:520-524`.
- **Fetch:** Advanced Trade `GET /api/v3/brokerage/accounts` (cursor pages, balance = available + hold, `ACCOUNT_TYPE_VAULT` = vault) `internal/sources/coinbase/coinbase.go:384-433`; v2 `GET /v2/accounts` (`next_uri` pages, `CB-VERSION` pinned, type `vault`) `internal/sources/coinbase/coinbase.go:436-484`. Next URIs are reduced to path+query so a hostile URL never receives the bearer token `internal/sources/coinbase/coinbase.go:487`. Amounts are exact (`big.Rat`) until the final float.
- **Staked detection** (`normalizeCurrency`): code `ETH2`, suffix `-STAKED`, or a currency name containing "staked" unless "unstaked"/"wrapped" (cbETH-style wrapped tokens stay liquid, own price) `internal/sources/coinbase/coinbase.go:615-628`. One holding per (symbol, staked, vault); sort puts plain before staked before vault `internal/sources/coinbase/coinbase.go:631-695`. Holding fields `Staked`, `Vault` are on `core.Holding` `internal/core/core.go:35-36`.
- **Meta rows:** masked API key, account/vault counts, reconciliation summary, balance differences `internal/sources/coinbase/coinbase.go:535-542`.
- **Activity** (`core.ActivitySource`) is v2 only: list accounts (cached in state 30 min) then each account's `/transactions`, newest first, with cursors per account, pending re-checks (max 25, 14 days) and a default budget of 20 when none is given (the syncer always passes 30, `internal/activity/syncer.go:20`) `internal/sources/coinbase/activity.go:34-51`, `internal/sources/coinbase/activity.go:464`, `internal/sources/coinbase/activity.go:726`. Each v2 transaction becomes one `core.Tx` with a single leg in the account's currency; USD value from `native_amount` `internal/sources/coinbase/activity.go:256-336`. Kind map: buy/sell/trade -> trade, send/receive by sign, rewards -> reward, deposits/withdrawals/staking transfers -> transfer, unknown -> other with the Coinbase type in the note `internal/sources/coinbase/activity.go:221-241`; status map `internal/sources/coinbase/activity.go:243`. Send fees are split into `Tx.Fee` `internal/sources/coinbase/activity.go:352`.
- **Config keys:** none in config.yaml; everything lives on the Def (`keyName`, sealed `privateKey`). `BaseURL` is test-only `internal/sources/coinbase/coinbase.go:64-65`.
- **Settings handlers:** edit form refill (key name only) `internal/server/source_handlers.go:72-73`; form parse with required-field checks, private key optional on edit `internal/server/source_handlers.go:124-131`.

## Connected to

- **owns:** the JWT/key parsing, account reconciliation, `normalizeCurrency`, v2 transaction mapping.
- **owned-by:** [source-def](source-def.md); contract in [source-contract](source-contract.md); secrets sealed by [secret-key](../storage/secret-key.md).
- **joins:** [snapshot](../valuation/snapshot.md) (prices `AssetKey`), [activity-entry](../ledger/activity-entry.md), exchange CSV imports (`coinbase-csv`) are deduped against it via `importedSourceType["coinbase-csv"]` `internal/server/reports_handlers.go:24`.
- **looks-like-but-is-not:** [kraken-source](kraken-source.md) (HMAC, ledger-based, no vaults); a Solana vault/escrow (`Holding.Vault` set by [solana-source](solana-source.md) for the Jupiter limit-order vault, meaning something different).

## If you change this

- **Hits:** `internal/sources/model.go` and `main.go` factory for new Def fields; `internal/server/source_handlers.go`; [snapshot](../valuation/snapshot.md) if staked/vault semantics move; [activity-entry](../ledger/activity-entry.md) and tax pairing if the one-leg-per-account mapping or kinds change; CSV-import dedupe matches on `ExternalID` (the v2 transaction id), so changing it breaks `dedupeImported` and `csvTargets` (`internal/server/reports_handlers.go:24-40`, `internal/server/tax.go:286`).
- **Does not hit:** [kraken-source](kraken-source.md) (separate `normalizeCurrency` vs `NormalizeAsset`; they do not share code); `internal/prices` symbol mapping unless AssetKeys change.

## Surfaces

| Surface | Role |
|---|---|
| refresher | reads `Fetch` holdings, warnings, Meta |
| activity syncer | `Activity` with state and budget |
| Settings handlers | write `keyName`/`privateKey`; read nothing else |
| CSV import dedupe | treats `coinbase-csv` as a duplicate of this live type |
| tests | `internal/sources/coinbase/testdata`, `coinbase_test.go`, `activity_test.go` |

## See

- Source: `internal/sources/coinbase/coinbase.go`
