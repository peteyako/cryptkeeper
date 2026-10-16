# Solana DeFi sources

Spike for implementation step 11 (`internal/sources/solana/defi/`). Every program id, layout,
PDA seed and endpoint below was checked on **2026-10-16** against the protocol's own source code
on GitHub **and** against live mainnet accounts (public RPC `https://api.mainnet-beta.solana.com`,
`getAccountInfo` / `getMultipleAccounts` / `getProgramAccounts`). The same constants are pinned
in the Go code next to where they're used, with the same source links.

Anchor account discriminators are `sha256("account:<Name>")[:8]`. Every one listed here matched
the first 8 bytes of the real accounts.

## Summary

| Protocol | What we read | Program / endpoint | Status |
| --- | --- | --- | --- |
| Orca Whirlpools | Position (NFT) → Whirlpool → mint decimals | `whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc` | confirmed |
| Raydium CLMM | PersonalPositionState (NFT) → PoolState | `CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK` | confirmed |
| Raydium AMM v4 / CPMM | LP token → pool reserves and LP supply | `GET https://api-v3.raydium.io/pools/info/lps?lps=…` | confirmed, keyless |
| Jupiter locked/staked JUP | Escrow PDA of the owner | `voTpe3tHQ7AjQHMapgSue2HJFAh2cGsdokqN3XqmVSj` | confirmed |
| Jupiter perps | Positions of the owner | `GET https://perps-api.jup.ag/v2/positions?walletAddress=…` (keyless); on-chain fallback `PERPHjGBqRHArX4DySjwM6UJHiR3sWAatqfdBS2qQJu` | confirmed |

Uncollected fees and rewards are **not** included in any LP value (v1). The UI should say so.

## Orca Whirlpools

- **Program:** `whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc`.
  [lib.rs `declare_id!`](https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/lib.rs).
  Live: executable, owned by the upgradeable BPF loader.
- **Position PDA:** `["position", position_mint]`.
  [open_position.rs](https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/instructions/open_position.rs).
  Checked: NFT `HdfvtgvhBBus2xPnAwUyH63Hw4M8aF1ssKvLHAgU9Ebk` derives to position `4N7woWHDb5MmFSv6STA1J4asde7xa2zkMEgSz7GG2Ram`.
- **Position account:** 216 bytes, discriminator `aabc8fe47a40f7d0`.
  [state/position.rs](https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/state/position.rs).

  | Offset | Field | Type |
  | --- | --- | --- |
  | 0 | discriminator | [u8; 8] |
  | 8 | whirlpool | Pubkey |
  | 40 | position_mint | Pubkey |
  | 72 | liquidity | u128 LE |
  | 88 | tick_lower_index | i32 |
  | 92 | tick_upper_index | i32 |
  | 96… | fee checkpoints, fees owed, 3 reward infos | (unused) |

- **Whirlpool account:** 653 bytes, discriminator `3f95d10ce1806309`.
  [state/whirlpool.rs](https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/state/whirlpool.rs).
  Fields used: `sqrt_price` u128 @65, `tick_current_index` i32 @81, `token_mint_a` @101,
  `token_mint_b` @181. The full prefix is: `whirlpools_config` @8, `whirlpool_bump` @40,
  `tick_spacing` u16 @41, `fee_tier_index_seed` @43, `fee_rate` u16 @45, `protocol_fee_rate` u16 @47,
  `liquidity` u128 @49, …, `token_vault_a` @133, `fee_growth_global_a` @165, `token_vault_b` @213.
- **Decimals:** the Whirlpool doesn't store them. We read both mints (`getMultipleAccounts`); SPL
  and Token-2022 mints share the base layout, with `decimals` u8 @44 and `is_initialized` @45
  ([token state.rs](https://github.com/solana-program/token/blob/main/interface/src/state.rs)).
- **Tick math:** port of `sqrt_price_from_tick_index`
  ([math/tick_math.rs](https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/math/tick_math.rs)).
  Positive ticks use Q32.96 factors with `mul_shift_96` then `>> 32`; negative ticks use Q64.64
  factors. Ticks run from −443636 to 443636. `MIN_SQRT_PRICE_X64 = 4295048016`,
  `MAX_SQRT_PRICE_X64 = 79226673515401279992447579055`.
- **Amounts / range:** the same as the Orca SDK
  ([rust-sdk/core quote/liquidity.rs](https://github.com/orca-so/whirlpools/blob/main/rust-sdk/core/src/quote/liquidity.rs),
  [math/position.rs](https://github.com/orca-so/whirlpools/blob/main/rust-sdk/core/src/math/position.rs)):
  - Below range when `sqrt_price ≤ sqrt(lower)`; above range when `sqrt_price ≥ sqrt(upper)`.
  - `Δa = floor((L·Δ√P) << 64 / (√Pl·√Pu))` and `Δb = floor(L·Δ√P >> 64)`, rounding down
    (`decrease_liquidity_quote`).
- **Out of scope:** position bundles (`PositionBundle`, seeds `["bundled_position", bundle_mint, index]`),
  and fees and rewards.

## Raydium CLMM

- **Program:** `CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK`.
  [lib.rs](https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/lib.rs). The devnet id
  `DRayAUgENGQBKVaX8owNhgzkEDyoHTGVEGHVJT1E9pfH` is also in that file and isn't used.
- **Personal position PDA:** `["position", nft_mint]`.
  [SDK pda.ts `getPdaPersonalPositionAddress`](https://github.com/raydium-io/raydium-sdk-V2/blob/master/src/raydium/clmm/libraries/pda.ts).
  The *protocol* position `["position", pool, tickLower(be), tickUpper(be)]` is a different account.
  Checked: NFT `HaRZVFCxgBs7XB1DW5mnVkoZSfGczaoMR73w1kVbN9UE` derives to `Faq8aCWF19Jyc8pcPQXHF7EnZNJe42mbuDodHm4ScCKL`.
- **PersonalPositionState:** 281 bytes, discriminator `466f967ee60f1975`.
  [states/personal_position.rs](https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/states/personal_position.rs).
  Layout: `bump` @8, `nft_mint` @9, `pool_id` @41, `tick_lower_index` i32 @73,
  `tick_upper_index` i32 @77, `liquidity` u128 @81, then fee growth, fees owed, rewards,
  `recent_epoch` and padding.
- **PoolState:** 1544 bytes, `#[repr(C, packed)]` zero-copy, discriminator `f7ede3f5d7c3de46`.
  [states/pool.rs](https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/states/pool.rs).
  - Layout: `bump` @8, `amm_config` @9, `owner` @41, `token_mint_0` @73, `token_mint_1` @105,
    `token_vault_0/1` @137/@169, `observation_key` @201, `mint_decimals_0/1` u8 @233/@234,
    `tick_spacing` u16 @235, `liquidity` u128 @237, `sqrt_price_x64` u128 @253, `tick_current` i32 @269.
  - Decimals come from the pool, so no mint fetch is needed.
- **Tick math:** port of `get_sqrt_price_at_tick`
  ([libraries/tick_math.rs](https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/libraries/tick_math.rs)).
  - **It differs from Orca's.** Raydium uses one table of truncated Q64.64 factors for |tick|,
    then `u128::MAX / ratio` for positive ticks.
  - `MAX_SQRT_PRICE_X64 = 79226673521066979257578248091` (Orca's is …515401279992447579055).
    Each protocol must use its own implementation to match on-chain values exactly.
- **Amounts:** `get_delta_amount_0/1_unsigned`
  ([libraries/liquidity_math.rs](https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/libraries/liquidity_math.rs))
  give the same formulas as Orca. The range test uses sqrt prices, like SDK
  `LiquidityMathUtil.getAmountsForLiquidity`.
- **Out of scope:** locked "Burn & Earn" positions (the NFT is held by the lock program, not the
  wallet), fees and rewards.

## Raydium AMM v4 / CPMM LP tokens (Raydium API v3)

- Programs, used only for labels: AMM v4 `675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8`, CPMM
  `CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C`.
  [SDK programId.ts](https://github.com/raydium-io/raydium-sdk-V2/blob/master/src/common/programId.ts).
- **Endpoint (keyless):** `GET https://api-v3.raydium.io/pools/info/lps?lps=<mint>,<mint>,…`
  ([API docs](https://api-v3.raydium.io/docs/)). The response shape, as confirmed:
  ```json
  {"id":"…","success":true,"data":[
    {"type":"Standard","programId":"675kPX…","id":"58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2",
     "mintA":{"address":"So111…","symbol":"WSOL","decimals":9}, "mintB":{"address":"EPjF…","symbol":"USDC","decimals":6},
     "mintAmountA":105620.259193858, "mintAmountB":10902200.233423,
     "lpMint":{"address":"8HoQnePLqPj4M7PUDzfw8e3Ymdwgc7NLGnaTUapubyvu","decimals":9},
     "lpAmount":89026.010206269, "lpPrice":245.11, "price":103.22, "tvl":…},
    null]}
  ```
  - `data` has one entry per requested mint, `null` when the mint isn't a Raydium LP token. We
    match entries by `lpMint.address` and never by array position.
  - `mintAmountA/B` (reserves) and `lpAmount` (LP supply) are in whole tokens (UI units). We
    parse them as exact decimals (`json.Number` → `big.Rat`).
  - **One invalid mint fails the whole batch** (`{"success":false,"msg":"query lps value error"}`),
    so we only send valid 32-byte base58 mints.
  - 150 mints were accepted in one call. We chunk at 100, so a normal wallet makes one call.
  - CPMM pools come back from the same endpoint. Example: `gyYigqG8vdEMkDNTTVL6at2msbHdTDWyb6bccyR238U`
    (NEAR/USDC, LP `ABuX9ddQpXps92PyMxeuWimTeeCyL4WD176P9AZhpxm2`).
- **Valuation:** `share = balance / lpAmount`, and each leg is `share × reserve`, truncated to the
  mint's decimals. Checked against the API's own `lpPrice`: `legA·price + legB` came within 0.1%
  of `balance × lpPrice`.

## Jupiter locked / staked JUP

- **Program (locked voter):** `voTpe3tHQ7AjQHMapgSue2HJFAh2cGsdokqN3XqmVSj`. The source is Jupiter's
  ("TeamRaccoons") fork of Tribeca's locked-voter:
  [WAGMI programs/locked-voter/src/lib.rs](https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/lib.rs).
  It's also listed as "Locker Vote" in
  [jup-ag/platform-list](https://github.com/jup-ag/platform-list/blob/main/src/platforms/jupiter-governance.ts).
- **JUP locker:** `CVMdMd79no569tjc5Sq7kzz8isbfCcFyBS5TLGsrZ5dN`, which is the PDA `["Locker", base]` with
  base `bJ1TRoFo2P6UHVwqdiipp6Qhp2HaaHpLowZ5LHet8Gm`.
  - Live: discriminator `4af60671f9e44ba9`, `token_mint` (@41) = JUP, ~1.125B JUP locked, 892,469 escrows.
  - LockerParams: `max_stake_vote_multiplier` = 1, `min_stake_duration` = 1 day,
    `max_stake_duration` = 7 days (604800 s).
- **Escrow PDA:** `["Escrow", locker, owner]`.
  [instructions/new_escrow.rs](https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/instructions/new_escrow.rs).
- **Escrow account:** 322 bytes, discriminator `1fd57bbbba16da9b`.
  [state.rs](https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/state.rs).

  | Offset | Field | Type |
  | --- | --- | --- |
  | 8 | locker | Pubkey |
  | 40 | owner | Pubkey |
  | 72 | bump | u8 |
  | 73 | tokens (escrow's JUP vault) | Pubkey |
  | 105 | amount | u64 |
  | 113 | escrow_started_at | i64 |
  | 121 | escrow_ends_at | i64 |
  | 129 | vote_delegate | Pubkey |
  | 161 | is_max_lock | bool |
  | 162 | partial_unstaking_amount | u64 |
  | 170 | padding u64, buffers [u128; 9] | |

- **Semantics:**
  - **Staked JUP** is `is_max_lock = true`. These escrows have no end date (`escrow_ends_at` is
    0 or ignored), and voting power is always full.
  - **Unstaking everything** toggles max-lock off and sets `escrow_ends_at = now + 7 days`. The
    label is then "Locked JUP · unlocks <date>", or "unlocked" once that date has passed.
  - **Partial unstakes** move JUP from `amount` to `partial_unstaking_amount`
    ([state.rs `accumulate_partial_unstaking_amount`](https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/state.rs)).
    The tokens stay in the vault until they're withdrawn.
  - **Owned JUP** is therefore `amount + partial_unstaking_amount`. Checked on 3 live escrows:
    this sum equals the vault token account's balance exactly. One example is
    `2JLxx53s7znxgL4u4JgJLbanKERDo5XfMN6rATWXtBpB`: 10,002 staked + 1,998 unstaking = 12,000 in the vault.
- JUP decimals (6) are a constant.

## Jupiter perps

- **Program:** `PERPHjGBqRHArX4DySjwM6UJHiR3sWAatqfdBS2qQJu`. JLP pool: `5BUwFW4nRbftYTDMbgxykoFWqWHPzahFSNAaaaJtVKsq`.
  - Docs: [jup-ag/docs perps/position-account.mdx](https://github.com/jup-ag/docs/blob/main/perps/position-account.mdx).
  - IDL and PDA seeds: [julianfssen/jupiter-perps-anchor-idl-parsing](https://github.com/julianfssen/jupiter-perps-anchor-idl-parsing),
    which Jupiter's docs link to as the reference.
- **Custodies:** the mint of each custody was read from its account at offset 40.

  | Custody | Mint |
  | --- | --- |
  | SOL `7xS2gz2bTp3fwCC7knJvUWTEU9Tycczu6VhJYKgi1wdz` | wSOL |
  | ETH `AQCGyheWPLeo6Qp9WpYS9m3Qj479t7R636N9ey1rEjEn` | `7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs` |
  | BTC `5Pv3gM9JrFFH883SWAhvJC9RPYmo8UNxuFtv5bMMALkm` | `3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh` |
  | USDC `G18jKKXQwBbrHeiK3C9MRXhkHsLHf7XgCSisykV46EZa` | USDC |
  | USDT `4vkNeXiYEUizLdrpdPS1eC2mccyM4NUPRtERrk6ZETkk` | USDT |

- **Primary: perps API (keyless).** `GET https://perps-api.jup.ag/v2/positions?walletAddress=<owner>`
  → `{"dataList":[…],"count":n}`. This is the endpoint Jupiter's own CLI uses
  ([jup-ag/cli PerpsClient.ts](https://github.com/jup-ag/cli/blob/main/src/clients/PerpsClient.ts)).
  - Fields: `asset`, `assetMint`, `collateralMint`, `positionPubkey`, `side`, `leverage`,
    `sizeUsd`, `collateralUsd`, `valueUsd`, `entryPriceUsd`, `markPriceUsd`, `pnlBeforeFeesUsd`,
    `pnlAfterFeesUsd`, fees, `tpslRequests`.
  - USD fields are integer strings with 6 decimals.
  - Confirmed on 4 live positions: `valueUsd = collateralUsd + pnlAfterFeesUsd`. We report this
    as the leg: USDC amount = `valueUsd`, with `USDValue` set.
  - An address without positions gets `{"dataList":[],"count":0}`.
- **Fallback: on-chain positions.** Used only when the API fails.
  - Position PDA: `["position", owner, pool, custody, collateral_custody, [side]]`, with side
    1 = long, 2 = short. Checked against live positions `GvJ3DnBx…` (BTC long) and `47JhaEuH…`
    (BTC short, USDC collateral).
  - Each owner has 9 possible positions: a long on SOL/ETH/BTC, and a short on each with USDC or
    USDT collateral. All 9 are fetched with one `getMultipleAccounts`. **Not** `getProgramAccounts`:
    the program has ~880k position accounts, and the public RPC returned a 400 MB response for a
    dataSize scan.
  - Position layout (IDL): `owner` @8, `pool` @40, `custody` @72, `collateral_custody` @104,
    `open_time` i64 @136, `update_time` i64 @144, `side` u8 @152, `price` u64 @153,
    `size_usd` u64 @161, `collateral_usd` u64 @169, `realised_pnl_usd` i64 @177,
    `cumulative_interest_snapshot` u128 @185, `locked_amount` u64 @201, `bump` @209.
  - Discriminator: `aabc8fe47a40f7d0` (the same "Position" name as Orca's).
  - **Live accounts are 216 bytes**, while the IDL struct is 210 bytes. The extra space is
    trailing, so the decoding reads only up to 177.
  - Closed positions keep their account with `size_usd = 0` and are skipped.
  - Value = `collateral_usd + uPnL`, with `uPnL = size_usd·(mark − entry)/entry`, negated for
    shorts (the formula from the repo's get-position-pnl.ts). The mark price comes from Jupiter
    Price API v3 (`https://lite-api.jup.ag/price/v3?ids=…`, keyless).
  - This is **before fees**, so the label gets "· est.". On the captured data it agreed with the
    API's `collateralUsd + pnlBeforeFeesUsd` to within 0.17% of position size. That gap is the
    difference between the two sources' mark prices.

## Test fixtures (`internal/sources/solana/defi/testdata/`)

The account fixtures are real mainnet accounts from one `getMultipleAccounts` call per file, so
each pool and its positions come from the same slot. The format is
`{"slot", "capturedAt", "accounts": {pubkey: {"owner","data"(base64)} | null}}`. The API fixtures
are raw responses.

| File | Contents |
| --- | --- |
| `orca_accounts.json` (slot 457756320) | 3 positions in SOL/USDC whirlpool `Czfq3x…` (in / below / above range), the whirlpool, wSOL and USDC mints, and a non-existent position PDA |
| `raydium_clmm_accounts.json` (slot 457756321) | 3 personal positions in SOL/USDC pool `3ucNos…` (in / below / above), and the pool |
| `jup_escrow_accounts.json` (slot 457756995) | 3 escrows (staked, unlocking Oct 23 2026, staked + 1,998 unstaking), their JUP vaults, and the locker |
| `perps_accounts.json` (slot 457756325) | the 9 perps position PDAs of `H3ogGm2QeDiELWVVo5pyn9ddZhZyxpoGzgEFe6Wng4BQ` (4 open, 2 closed, 3 missing) |
| `perps_api_positions.json` | perps API response for the same wallet |
| `jupiter_price_perps.json` | Price API v3 for SOL/ETH/BTC at capture time |
| `raydium_lps_api.json` | LP API response for SOL-USDC (AMM v4), NEAR-USDC (CPMM) and USDC (→ `null`) |

**How the expected values were made independent of our code:**
- Orca amounts come from the official **@orca-so/whirlpools-core 3.1.1** (`decreaseLiquidityQuote`,
  slippage 0) over the captured accounts. Extra tick vectors come from its `tickIndexToSqrtPrice`.
- Raydium amounts come from the official **@raydium-io/raydium-sdk-v2 0.2.74-alpha**
  (`TickUtil.getSqrtPriceAtTick` + `LiquidityMathUtil.getAmountsForLiquidity`, roundUp=false).
  So do its tick vectors.
- The tick math is also checked against the live pools: each captured pool's `sqrt_price` lies in
  `[sqrt(tick_current), sqrt(tick_current+1))`.
- SDK test vectors:
  - Orca `test_exact_bit_values` (every tick bit, ±).
  - Orca and Raydium MIN/MAX tick.
  - Orca `test_decrease_liquidity_quote` (L = 1e6, ticks ±10 → 999/0, 499/499, 0/999).
  - Orca `test_position_status` boundaries.
  - Orca `test_get_amount_delta`.
- JUP amounts are checked against the escrow vault's token balance.
- LP legs come from Python `decimal` arithmetic and are cross-checked with the API's `lpPrice`.
- Perps values are checked against the fixture's own `collateralUsd + pnlAfterFeesUsd`, and the
  fallback against `pnlBeforeFeesUsd`.

## Integration notes for the solana source

- **RPC adapter.** Adapt the solana client to `defi.RPC`. `GetMultipleAccounts` gets at most 100
  keys per call and must return `nil` for missing accounts. `defi.NewHTTPRPC` is a standalone
  implementation, and it scrubs API keys from errors.
- **Partial results.** `Positions` may return positions **and** an error together. Keep the
  positions and show the error as a warning.
- **`Input.Symbol`** (an optional `func(mint) string`) lets labels use the caller's token
  identity (e.g. `solana.KnownMint`). Without it, a small built-in table is used, then a short mint.
- **`ClaimedMints`.** Remove these from the plain token list and the NFT count. They're the
  CLMM position NFTs and the LP mints.
- **Legs.** A leg can be zero: a CLMM position outside its range holds only one token.
- **Perps.** A perp has one USDC leg whose amount is the USD value, and `USDValue` is set too.
  The value can be negative.
- **`Adapters(nil, c)`** returns all three adapters in the order jupiter, orca, raydium.

## Not verified / limitations

- **Owner wallets.** The public RPC rate-limits `getTokenLargestAccounts` ("Too many requests
  for a specific RPC call"). So the fixtures' CLMM position NFTs and LP balances aren't tied to
  their real holders. The tests pass the NFT mints and a chosen LP balance in `Input` directly.
  The full chain from wallet to token accounts belongs to the solana package.
- **Live API values.** The Raydium API reserves are floats that the API caches. They aren't
  reconciled against the pool vaults on chain.
- **Perps fallback.** It ignores borrow and close fees, and it uses the Price API rather than the
  perps program's own oracle (Doves/Pyth custody oracles), so its numbers are estimates.
- **Not handled:** Orca position bundles, Raydium locked positions, and fee/reward accrual for
  all protocols.
