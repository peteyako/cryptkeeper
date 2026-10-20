---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-20 @ 936c434
entity: internal/sources/solana/defi/defi.go
---

# Solana DeFi positions (`solana/defi` adapters)

The adapters that read a Solana wallet's Orca, Raydium and Jupiter positions and report the assets underneath as legs, which [solana-source](solana-source.md) turns into tagged holdings.

## Why this shape

- A position is not a token balance: an Orca/Raydium LP is an NFT whose value lives in a pool account, a Jupiter lock is an escrow PDA, a perp is API/account state. Each adapter decodes the protocol's own account layout and returns the underlying amounts, so the existing pricing path (amount x price by mint) values them with no new price source.
- Positions must not be counted twice: each reports `ClaimedMints` (position NFT, LP token) and the solana source removes them from the plain token list and the NFT count `internal/sources/solana/defi/defi.go:60-62`, `internal/sources/solana/solana.go:224-237`.
- The package never imports `solana` (that imports it), so it has its own small `RPC` interface, its own HTTP implementation of it, and a copy of the PDA/base58 helpers `internal/sources/solana/defi/defi.go:9-10`, `internal/sources/solana/defi/defi.go:87-93`, `internal/sources/solana/defi/rpc.go:17`, `internal/sources/solana/defi/defi.go:310-352`. The solana source uses that HTTP implementation (`internal/sources/solana/solana.go:122`), not its own paced client, despite the comments saying otherwise: adapter calls get the host pace, not the per-method pace.
- Layouts, discriminators, seeds and endpoints were checked against GitHub and live mainnet on 2026-10-16 and are written up in [docs/defi-sources.md](../../../defi-sources.md); constants sit next to the code that uses them. Read that page for offsets; do not copy them here.
- Fees and rewards are excluded from every LP value (documented limit).

## Shape

- **Contract:** `Adapter{Name(); Positions(ctx, rpc, Input)}` may return partial positions with an error, which becomes a warning `internal/sources/solana/defi/defi.go:76-85`. `Position{Protocol, ID, Kind, Label, InRange, Legs, ClaimedMints}`; `Leg{Mint, Amount, AmountStr, Decimals, USDValue}` `internal/sources/solana/defi/defi.go:52-74`. Kinds `lp`, `perp`, `locked`, `limit` `internal/sources/solana/defi/defi.go:28-33` (these strings are `core.Holding.PositionKind`).
- **Which run:** `solana.Config.DeFi` from `solana.defi` in config.yaml, default all three `internal/config/config.go:142`. `defi.Adapters` treats an empty list as all `internal/sources/solana/defi/defi.go:122-148`, but the source builds adapters only when the list is non-empty `internal/sources/solana/solana.go:120-123`, so an explicit empty list means none.
- **Orca Whirlpool:** NFT -> position PDA -> pool -> mint decimals -> amounts; shared CLMM flow `internal/sources/solana/defi/orca.go:131`, program `internal/sources/solana/defi/orca.go:15`. Position bundles unsupported `internal/sources/solana/defi/orca.go:87-88`.
- **Raydium:** CLMM positions through the same flow (decimals from the pool), then AMM v4/CPMM LP tokens priced from the keyless Raydium API v3 `internal/sources/solana/defi/raydium.go:109-135`, `internal/sources/solana/defi/raydium.go:164`; locked "Burn & Earn" positions not found `internal/sources/solana/defi/raydium.go:106-108`.
- **Jupiter:** (a) locked/staked JUP escrow PDA of the owner, amount plus partial-unstake amount `internal/sources/solana/defi/jupiter.go:171-203`, program/locker `internal/sources/solana/defi/jupiter.go:28-29`; (b) perps from the keyless perps API, falling back to on-chain accounts valued at collateral + PnL with Jupiter Price API marks `internal/sources/solana/defi/jupiter.go:263-273`. A perp is one USDC leg whose amount is its USD value `internal/sources/solana/defi/jupiter.go:310-315`; `Leg.USDValue` is not copied into the holding, so a perp prices as USDC `internal/sources/solana/positions.go:76-82`.
- **Not an adapter:** the Jupiter limit-order vault (kind `limit`) is read by the solana package from `Def.JupiterVault` `internal/sources/solana/solana.go:321-350`; the constant lives here for tagging only `internal/sources/solana/defi/defi.go:32`. The Find button on the source form looks the vault up in the wallet's history ([solana-source](solana-source.md), `internal/sources/solana/findvault.go:15-73`).
- **Glue in solana:** adapters run concurrently, 20 s each, panics recovered `internal/sources/solana/positions.go:15`, `internal/sources/solana/positions.go:39-50`. Each non-zero leg becomes a `core.Holding` with `Protocol/Position/PositionKind/Note`, `Verified=true`; wSOL becomes SOL; `locked` also sets `Staked` `internal/sources/solana/positions.go:76-99`. Activity treats the voter and perps programs as `position` moves `internal/sources/solana/activity.go:755`.

## Connected to

- **owns:** the protocol layouts and tick math (`internal/sources/solana/defi/clmm.go`), position labels ("SOL/USDC · in range").
- **owned-by:** [solana-source](solana-source.md) (only caller); enabled by [config](../storage/config.md) `solana.defi`.
- **joins:** [snapshot](../valuation/snapshot.md) (position legs are exempt from spam hiding, `internal/portfolio/aggregate.go:523`), [price](../valuation/price.md) (legs priced by known-mint ticker, `spl:<mint>` or SOL; perps as USDC).
- **looks-like-but-is-not:** plain LP/JLP tokens that are ordinary SPL balances (JLP is not handled here, `internal/sources/solana/defi/jupiter.go:149-150`); Coinbase `Vault`/`Staked` flags in [coinbase-source](coinbase-source.md).

## If you change this

- **Hits:** `internal/sources/solana/positions.go` (holding mapping); `docs/defi-sources.md` (layouts must stay in step); a new `PositionKind` needs its badge in `web/templates/view.go:261` ([templ-components](../web/templ-components.md)); rows group by `Protocol`+`Position` (`web/templates/view.go:247`) and [snapshot](../valuation/snapshot.md) exempts any row with a `Protocol` from spam hiding. A new adapter: name const, `AllAdapterNames` and the `Adapters` switch (`internal/sources/solana/defi/defi.go:110-147`), the default list (`internal/config/config.go:142`), `protocolName` (`internal/sources/solana/positions.go:120-130`); `main.go` passes the list through unchanged.
- **Does not hit:** [bitcoin-source](bitcoin-source.md), exchange sources, or the token metadata cache; do not add pricing here (prices come through [price](../valuation/price.md)).

## Surfaces

| Surface | Role |
|---|---|
| `Source.Fetch` (via refresher) | runs adapters each fetch, merges legs |
| `Source.Activity` | uses program ids to tag `position` transactions |
| tests | captured-account fixtures in `internal/sources/solana/defi/testdata` |

## See

- Source: `internal/sources/solana/defi/defi.go`; layouts: `docs/defi-sources.md`
