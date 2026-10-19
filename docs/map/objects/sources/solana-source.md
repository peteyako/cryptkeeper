---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/sources/solana/solana.go
---

# Solana source (`sources.TypeSolana`, `solana.Source`)

A read-only Solana wallet: native SOL, every SPL token (Token and Token-2022), native stake accounts, an NFT count and the wallet's Jupiter limit-order vault, built from one public address; DeFi positions come from [solana-defi](solana-defi.md).

## Why this shape

- The source never prices or judges spam. It returns holdings keyed by `AssetKey` (exchange ticker for verified well-known mints, `spl:<mint>` otherwise) so a USDC on Solana merges with USDC on an exchange; the aggregator prices and hides (`internal/sources/solana/solana.go:1-7`, `internal/sources/solana/solana.go:388-394`).
- Identity of a mint is the mint, never its ticker: look-alike tokens reuse tickers, so only the hand-verified table in `internal/sources/solana/knownmints.go:16` earns an exchange key.
- Hostile inputs are everywhere (anyone mints a token with any name, RPC URLs embed API keys), so token text is sanitised (`internal/sources/solana/text.go:27`) and RPC errors are scrubbed of the URL (`internal/sources/solana/rpc.go:99`).
- The public RPC is shared and rate-limited, so Fetch is bounded: capped Jupiter batches, capped on-chain lookups, a resolve deadline (`internal/sources/solana/tokens.go:19-31`), and the activity scan runs on a request budget with resumable cursors (`internal/sources/solana/activity.go:21-50`).

## Shape

- **Registration:** type constant and display order `internal/sources/model.go:23`, `internal/sources/model.go:33`; default kind hot `internal/sources/model.go:160`; Validate needs `address` `internal/sources/model.go:130`; Def fields `Address`, `JupiterVault` `internal/sources/model.go:61-64`. Built by the factory case `main.go:280-285` (also passes the shared meta cache, `main.go:175`).
- **Config** (`solana.Config`): `RPCURL`, `IncludeStake`, `HideMints`, `AllowMints`, `Overrides`, `DeFi`, `JupiterVault` `internal/sources/solana/solana.go:40-55`. `New` rejects bad pubkeys and a vault equal to the wallet `internal/sources/solana/solana.go:75-96`; on the public RPC host it sets request pacing `internal/sources/solana/solana.go:104-109`.
- **Fetch** `internal/sources/solana/solana.go:194`: getBalance, token accounts of both programs summed per mint (`internal/sources/solana/tokens.go:104-143`), optional stake sum via `getProgramAccounts` memcmp at the withdraw-authority offset (`internal/sources/solana/stake.go:11-35`; a refusing RPC only adds a warning, `internal/sources/solana/stake.go:15`). wSOL folds into SOL; zero balances skipped; decimals-0 amount-1 mints are NFTs, counted not listed `internal/sources/solana/solana.go:233-247`, `internal/sources/solana/solana.go:290`. Staked SOL is a second SOL holding with `Staked=true` `internal/sources/solana/solana.go:250-252`.
- **Metadata cache:** resolver order cache, Jupiter verified list (daily), Jupiter search, Token-2022 extension, Metaplex PDA, shortened mint `internal/sources/solana/tokens.go:151-228`. `MetaCache` is SQLite `token_meta` (TTL 24 h from `main.go:175`) or in memory `internal/sources/solana/metacache.go:38-75`; table at `internal/store/store.go:30`. A fallback (shortened-mint) name is never cached, and an on-chain name is not cached when Jupiter errored, so both are retried next fetch `internal/sources/solana/tokens.go:384-390`.
- **Spam control, two layers:** (1) user `HideMints` go to `WalletResult.Hidden` at fetch time `internal/sources/solana/solana.go:265-267`; `AllowMints` force `Verified` `internal/sources/solana/solana.go:395`. (2) the aggregator moves unverified, unpriced or below-`MinUSD` Solana rows into `HiddenRows` when `hideUnpriced` is on `internal/portfolio/aggregate.go:240`, `internal/portfolio/aggregate.go:328-333`; SOL and DeFi legs are exempt. Config `internal/config/config.go:108-117`, defaults `internal/config/config.go:135-140`, wiring `main.go:485-486`.
- **Jupiter vault:** extra owner whose SOL and tokens come back tagged `Vault=true`, `Protocol=jupiter`, kind limit `internal/sources/solana/solana.go:321-366`; activity watches it as the wallet's own `internal/sources/solana/activity.go:777-783`.
- **Activity** (`core.ActivitySource`, `internal/sources/solana/activity.go:52`): watches owner plus each token account (capped 300), cursors `Newest`/`Oldest`/gaps in opaque state `internal/sources/solana/activity.go:87-108`; default budget 40 when none is given `internal/sources/solana/activity.go:55`, but the syncer always passes its own 30 `internal/activity/syncer.go:20`, `internal/activity/syncer.go:383-395`. `classify` nets owner balance change per mint, fee split out, kind from legs `internal/sources/solana/activity.go:664`, `internal/sources/solana/activity.go:795-822`: opposite signs swap, stake-program use stake, position programs `position`, otherwise send/receive/fee. A failed tx is kept with `TxFailed` `internal/sources/solana/activity.go:767-769`. Staking rewards (no tx) come from `getInflationReward` per epoch `internal/sources/solana/rewards.go:81`, only with `IncludeStake`.
- **Tax inputs:** none are built here. Solana legs reach the tax engine as ordinary activity; `HideMints` become the tax spam set `internal/server/tax.go:355`, auto-hidden tokens only silence review items `internal/server/tax.go:370`; `HideMints` also stop historical valuation `main.go:504`.
- Comment vs code: `internal/sources/solana/defi/defi.go:9-10` and `internal/sources/solana/defi/rpc.go:14-15` say the solana package adapts its own RPC client for the adapters; code uses `defi.NewHTTPRPC` instead `internal/sources/solana/solana.go:122`. So DeFi calls share the host pace but skip the per-method pace of `rpcClient.call` `internal/sources/solana/rpc.go:55-61`.

## Connected to

- **owns:** the `token_meta` cache usage, known-mint table, DeFi adapter wiring [solana-defi](solana-defi.md).
- **owned-by:** [source-def](source-def.md) (`data/sources.yaml` entry); instantiated per [source-contract](source-contract.md) rules.
- **joins:** [snapshot](../valuation/snapshot.md) (prices and hides), [activity-entry](../ledger/activity-entry.md) (Tx stream), [database](../storage/database.md) (`token_meta`), [config](../storage/config.md) (`solana:` block).
- **looks-like-but-is-not:** [bitcoin-source](bitcoin-source.md) (also an `AddressLister`); `portfolio.WalletView.HiddenRows` (the display result of spam control, not source output).

## If you change this

- **Hits:** `internal/sources/model.go` and `main.go` factory for new Def fields; `internal/server/source_handlers.go:79`, `internal/server/source_handlers.go:180` (form values and parse); `internal/server/activity_handlers.go:130` (public-RPC advice); [snapshot](../valuation/snapshot.md) hide rules if spam semantics move; [activity-entry](../ledger/activity-entry.md) if Tx kinds or ID format change (ID = `sourceId:signature`, upserts expected); the `AssetKey` scheme (known-mint ticker, else `spl:<mint>` plus `Mint`) is what [price](../valuation/price.md) routes on (`internal/prices/prices.go:9`) and what the tax spam set matches (`internal/server/tax.go:355`).
- **Does not hit:** [bitcoin-source](bitcoin-source.md) or exchange sources; [price](../valuation/price.md) internals while that key scheme holds (mint pricing is in `internal/prices`, not here).

## Surfaces

| Surface | Role |
|---|---|
| refresher (`portfolio` Fetch) | reads holdings, hidden, NFT count, meta |
| activity syncer | calls `Activity` with state and budget; `SkipMints` |
| source form handlers | write `Address`, `JupiterVault`; activity page reads `solana.rpcUrl` |
| wallet detail page | `core.AddressLister.Addresses` `internal/sources/solana/solana.go:170` |
| Koinly import | matches wallets by `Def.Address`/`JupiterVault` `internal/server/koinly_handlers.go:390` |
| tests | `internal/sources/solana/testdata/*.json` fixtures |

## See

- Source: `internal/sources/solana/solana.go`
