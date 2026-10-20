---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-20 @ 936c434
entity: internal/sources/kraken/kraken.go
---

# Kraken source (`sources.TypeKraken`, `kraken.Source`)

A read-only Kraken account over the private Spot REST API: balances from `Balance`, activity from `Ledgers`, signed with an API key and base64 secret (HMAC-SHA512).

## Why this shape

- Kraken's balance codes are a zoo (`XXBT`, `ZUSD`, `DOT28.S`, `ETH2.S`, `USD.HOLD`). `NormalizeAsset` collapses them to the common ticker plus a `staked` flag, so Kraken balances merge with the same asset elsewhere and priced by ticker `internal/sources/kraken/kraken.go:294-320`.
- Matching is case-sensitive and unknown suffixes are left alone on purpose: tokenized stocks use a lowercase `x` (`AAPLx`), and an unrecognised product should show as its own unpriced symbol rather than silently merge into the wrong asset `internal/sources/kraken/kraken.go:283-293`.
- Kraken requires a strictly increasing nonce per key. Nonces are shared process-wide per API key, so a source rebuilt while an old fetch runs cannot reuse one; each attempt, including httpx retries, takes a fresh nonce and signature `internal/sources/kraken/kraken.go:62-64`, `internal/sources/kraken/kraken.go:119-127`, `internal/sources/kraken/kraken.go:216`.
- The ledger is append-only and rate-limited (2 points per call), so activity keeps "runs" of processed ledger instead of a timestamp cursor `internal/sources/kraken/activity.go:3-23`.

## Shape

- **Registration:** `TypeKraken` `internal/sources/model.go:21`; Def fields `APIKey`, `APISecret` (both secret, sealed) `internal/sources/model.go:53-54`, `internal/sources/model.go:78`; Validate checks nothing type-specific (missing secrets allowed) `internal/sources/model.go:124-125`; default kind exchange `internal/sources/model.go:156`. Factory `main.go:269-273` refuses empty key or secret. `New` rejects a secret that is not base64 `internal/sources/kraken/kraken.go:73-75`.
- **Auth:** `Sign` = base64(HMAC-SHA512(base64-decoded secret, urlPath + SHA256(nonce + postData))) `internal/sources/kraken/kraken.go:105-114`; headers `API-Key`, `API-Sign`, POST form body starting with `nonce` `internal/sources/kraken/kraken.go:213-232`. Needed key permissions: Query Funds and Query Ledger Entries, shown in Meta `internal/sources/kraken/kraken.go:203-206`. Errors arrive in `error[]` even on HTTP 200 `internal/sources/kraken/kraken.go:247-249`.
- **Fetch:** one holding per (symbol, staked), amounts summed exactly with `decimalSum` then floated, zeros dropped, plain before staked `internal/sources/kraken/kraken.go:151-200`, `internal/sources/kraken/kraken.go:355-388`.
- **Asset normalization:** strip `.HOLD` (same asset, not staked); staked suffixes `.S .M .F .B .P` merge into the base symbol with `Staked=true`; `ETH2`/`ETH2.S` is staked ETH; bonded variants lose two trailing digits (`DOT28.S` -> `DOT`), only with a staking suffix; legacy `X`/`Z` codes map via a table that deliberately omits `XTZ`, `ZRX`, `XRP` `internal/sources/kraken/kraken.go:254-336`.
- **Activity** (`core.ActivitySource`): `POST /0/private/Ledgers`, 50 per call, offset paging, budget capped at 6 per call regardless of the syncer `internal/sources/kraken/activity.go:45-52`, `internal/sources/kraken/activity.go:570-575` (the doc comment at `activity.go:558` still says "default 10"; the code says 6). State is `Runs` (newest id, oldest id, length), `Done`, `Since`, `Count`, `Start`, `Sweep` `internal/sources/kraken/activity.go:64-85`; `start` is rounded to the UTC day so a moving horizon does not invalidate the ledger count `internal/sources/kraken/activity.go:584-588`. A 429 or throttle text becomes `ErrRateLimited`, with progress kept `internal/sources/kraken/activity.go:58`, `internal/sources/kraken/activity.go:184-187`, `internal/sources/kraken/activity.go:204-213`.
- **Ledger to Tx:** entries grouped by `refid` into one `core.Tx` with a leg per entry and one fee leg (extra fee assets go in the note) `internal/sources/kraken/activity.go:310-416`. Kind per entry: trade/spend/receive/conversion -> trade, deposit -> receive, withdrawal -> send, staking/earn/dividend/reward -> reward if positive, transfer if a Spot/Earn allocation move (`isAllocation`), else other; when entries of one refid disagree the higher `kindRank` wins `internal/sources/kraken/activity.go:269-300`. USD legs get `USD` set directly, and in a two-leg USD trade the other leg takes the USD amount `internal/sources/kraken/activity.go:302-308`, `internal/sources/kraken/activity.go:379-390`. Status is always confirmed.
- **Config keys:** none in config.yaml; everything is on the Def. `BaseURL` is test-only `internal/sources/kraken/kraken.go:40-41`.
- **Settings handlers:** form parse, key and secret required only when adding `internal/server/source_handlers.go:132-138`; the edit form shows no Kraken values (secrets never refilled) `internal/server/source_handlers.go:71-87`.
- **Cross-source use:** `solana.MintForSymbol` lets a Kraken `JUP` balance price by Solana mint `main.go:332`. `NormalizeAsset` is exported because the Kraken CSV importer uses it too `internal/costbasis/kraken_csv.go:101`.

## Connected to

- **owns:** HMAC signing, nonce counter, `NormalizeAsset`, ledger run state.
- **owned-by:** [source-def](source-def.md); contract in [source-contract](source-contract.md); secrets sealed by [secret-key](../storage/secret-key.md).
- **joins:** [snapshot](../valuation/snapshot.md), [activity-entry](../ledger/activity-entry.md), [price](../valuation/price.md) (ticker from `NormalizeAsset`); CSV import dedupe via `importedSourceType["kraken-csv"]` `internal/server/reports_handlers.go:24`.
- **looks-like-but-is-not:** [coinbase-source](coinbase-source.md) (JWT, two APIs, vault flag, `normalizeCurrency` is a different function).

## If you change this

- **Hits:** `internal/sources/model.go` and `main.go` factory for Def fields; `internal/server/source_handlers.go`; [price](../valuation/price.md) symbol mapping if `NormalizeAsset` output changes (merges with other sources by `AssetKey`), and the Kraken CSV importer, which calls it ([manual-cost-basis](../ledger/manual-cost-basis.md), `internal/costbasis/kraken_csv.go:101`); [activity-entry](../ledger/activity-entry.md) and tax if kinds or refid grouping change (Tx ID = `sourceId:refid`, upserts); Kraken CSV import dedupe, which matches on `ExternalID` = refid (`internal/server/reports_handlers.go:24-40`).
- **Does not hit:** [coinbase-source](coinbase-source.md); the Solana mint table beyond `MintForSymbol`.

## Surfaces

| Surface | Role |
|---|---|
| refresher | reads `Fetch` holdings and Meta |
| activity syncer | `Activity` with state; handles `ErrRateLimited` |
| Settings handlers | write `apiKey`/`apiSecret` |
| tests | `internal/sources/kraken/kraken_test.go`, `activity_test.go` (httptest servers) |

## See

- Source: `internal/sources/kraken/kraken.go`
