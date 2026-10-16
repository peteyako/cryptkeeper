# CryptKeep: self-hosted crypto portfolio monitor (Go + templ + Datastar)

## Context
You hold crypto in **Coinbase** and **Kraken**, plus offline **Bitcoin** wallets (xpub/zpub, descriptors, multisig) and **Solana** wallets (SOL, SPL tokens, DeFi). You want one self-hosted, read-only web app that shows everything, groups it into portfolios, and tracks change, P&L, activity and alerts. The project directory is empty (greenfield).

**Decisions made:**
- **Stack:** Go 1.26.1 single binary, templ (a-h/templ) + Datastar (server-rendered HTML, live updates over SSE, near-zero hand-written JS).
- **Design:** the **Vault design system** (https://claude.ai/artifact/Nh1wTG5iHF4ngUD6grk8jM), with its example screens as the layout reference. The UI uses CryptKeep's name and branding.
- **Sources:** managed **in the UI** (the design's "Add source" flow). Stored in `data/sources.yaml`, with API secrets encrypted at rest.
- **Coverage:**
  - Coinbase, including staked balances and vaults.
  - Kraken.
  - BTC via xpub/ypub/zpub, output descriptors, multisig, wallet files, plain addresses.
  - Solana SOL, every SPL token, native stake, and DeFi: Jupiter (locked JUP, perps) and Raydium/Orca LPs.
  - Hand-entered holdings and watch-only coins.
- **Portfolios:** named groups of assets and/or wallets, edited in the UI. Saved, exported, imported and restored as **YAML**.
- **Activity:** one feed across BTC, Solana, Coinbase and Kraken, which also feeds cost basis.
- **Insights:** 24h/7d/30d change, cost basis + P&L (from activity + manual + CSV import), alerts to ntfy, CSV export.
- **Ops:** stale-but-visible data on API failures, LAN/Tailscale with an optional password, `/healthz`, installable PWA.

**Safety principles** (also the design's): read-only always, so the app never moves funds. Only public keys/addresses and view-only API keys. No seed phrases or private keys, ever, and the UI says so where you paste keys.

Scope is large, so it is delivered in **six milestones**, each a usable app (see Implementation order).

## Architecture
```
crypto-portfolio/
  go.mod                       # module crypto-portfolio, go 1.26; templ CLI via `tool` directive
  main.go                      # subcommands: serve (default), portfolios export|import, sources export|import, healthcheck
  config.example.yaml          # server settings only (sources live in data/sources.yaml)
  .env.example                 # CRYPTKEEP_SECRET_KEY, PORTFOLIO_PASSWORD, NTFY_TOKEN (all optional; see below)
  docs/plan.md, docs/defi-sources.md
  internal/
    config/                    # config.yaml (server settings) + env
    secrets/                   # AES-256-GCM seal/open for API secrets; key from env or data/secret.key (0600)
    store/                     # SQLite (modernc.org/sqlite, pure Go → CGO off): history, activity, price_history, txns, alert log
    sources/
      registry.go              # loads data/sources.yaml, builds Source instances, hot-reloads on change
      model.go                 # SourceDef{ID, Type, Label, Kind(exchange|hot|hardware|manual), Config, SealedSecrets}
      coinbase/                # ES256 JWT; Advanced Trade + v2 accounts (reconciled); v2 transactions
      kraken/                  # HMAC-SHA512; Balance; Ledgers; asset normalization
      bitcoin/                 # derive.go, descriptor.go (BIP380), walletfile.go, esplora.go (balances, txs, utxos)
      solana/                  # solana.go, tokens.go, stake.go, knownmints.go, activity.go
      solana/defi/             # defi.go, clmm.go, orca.go, raydium.go, jupiter.go
      manual/                  # manual wallets as a Source
    portfolio/                 # types.go, aggregate.go, refresher.go, broker.go (pub/sub), lastgood.go (stale cache)
    portfolios/                # model.go, store.go (YAML, atomic, backups), filter.go
    prices/                    # spot (Coinbase → Kraken → Jupiter), fiat FX, historical daily candles (cached in SQLite)
    activity/                  # model.go, sync.go (incremental per source), transfers.go (own-wallet matching), classify.go
    change/                    # 1D/1W/1M/1Y/All deltas from history
    costbasis/                 # avgcost.go, manual (data/costbasis.yaml), coinbase_csv.go, kraken_csv.go
    alerts/                    # model.go (data/alerts.yaml), engine.go, ntfy.go
    export/                    # CSV writers
    httpx/                     # shared client, timeouts, 429 backoff
    server/                    # chi router, handlers per area, guard.go, auth.go, health.go
  web/
    templates/
      layout.templ             # <html data-theme>, head (fonts, tokens.css, manifest), app shell: sidebar / bottom nav
      components.templ         # Vault components (see Design system) as templ funcs
      overview.templ           # portfolio overview (Portfolio.dc.html)
      wallet.templ             # wallet detail (Wallet.dc.html)
      sources.templ            # add/edit source flow (MobileConnect.dc.html)
      activity.templ, portfolios.templ, reports.templ (P&L + exports), alerts.templ, settings.templ
      format.go                # number rules (fiat, coin, change, mask, address, stale tilde), svgPath, monogram colors
      *_templ.go               # generated, committed
      templates_test.go        # render tests + Datastar guard checks
    static/
      tokens.css               # Vault tokens as CSS custom properties, dark + light
      app.css                  # component styles built only on tokens
      fonts/                   # IBM Plex Sans 400/500/600 + IBM Plex Mono 400/500 woff2 (OFL), self-hosted
      datastar.js              # vendored v1.0.4
      manifest.webmanifest, icons/
  Dockerfile, docker-compose.yml, Taskfile.yml, .gitignore, README.md
```
Tests (`*_test.go`) sit next to each package and use `httptest` fakes for every external API, plus `testdata/` fixtures.

### Dependencies
- `gopkg.in/yaml.v3`.
- `btcd`: `btcutil`/`hdkeychain`/`base58`, `chaincfg`, `txscript`, `btcec/v2`.
- `golang-jwt/jwt/v5`.
- `golang.org/x/sync` (errgroup, singleflight).
- `a-h/templ`.
- `starfederation/datastar-go` v1.2.2.
- `go-chi/chi/v5`.
- `modernc.org/sqlite` (pure Go, keeps `CGO_ENABLED=0` and distroless).
- `rsc.io/qr` (receive-address QR as SVG).
- Everything else is stdlib, including `crypto/aes` + `cipher` for GCM.

## Design system: Vault
The source of truth is the canvas boards: Foundations (`Main.dc.html`), Components, Light mode tokens, and the example screens (Portfolio, Wallet, MobilePortfolio, MobileConnect, plus their light variants).

**Tokens → `web/static/tokens.css`.** Theme defaults to `prefers-color-scheme`; `[data-theme=light|dark]` on `<html>` is a manual override.

| Token | Dark | Light | Use |
| --- | --- | --- | --- |
| `--bg` | #0D0F12 | #F6F7F9 | app ground |
| `--surface-1` | #14171C | #FFFFFF | cards, panels |
| `--surface-2` | #1B1F26 | #F0F2F5 | hover, inputs, secondary buttons |
| `--border` / `--border-strong` | #262B33 / #343A44 | #E1E4E9 / #CDD2D9 | dividers / control borders |
| `--row` (table dividers, chart grid) | #1B1F26 | #EEF0F3 | |
| `--text-1/2/3` | #ECEEF1 / #A3AAB5 / #7C8592 | #12151A / #4A525E / #69717D | figures / body / meta |
| `--accent` / `--on-accent` / `--accent-soft` / `--link` | #6E9BFF / #0D0F12 / #1E2A44 / #8FB2FF | #2F5FD9 / #FFFFFF / #E6EDFC / #2F5FD9 | actions, focus, selected |
| `--gain` / `--gain-soft` | #4FD69C / #12291F | #0E7A4E / #E3F4EC | up, synced |
| `--loss` / `--loss-soft` | #FF8A7A / #2A1717 | #C23B2B / #FBE8E5 | down, errors, destructive |
| `--warn` / `--warn-soft` | #F2B84B / #2E2410 | #9A6400 / #FBF0D9 | stale, pending |
| `--src-exchange` / `--src-hot` / `--src-hardware` / `--src-other` | #6E9BFF / #C08CFF / #F2B84B / #5E6672 | #2F5FD9 / #7A3FD1 / #C48A12 / #A3AAB5 | source rails, dots, allocation |
| disc pairs `--disc-{btc,eth,sol,usd,neutral}-{bg,fg}` | per Portfolio.dc.html | per light map | coin monograms |
| `--card-shadow` | none | 0 1px 2px rgba(18,21,26,.06) | light cards |

- **Spacing:** `--s1…--s8` = 4, 8, 12, 16, 24, 32, 48, 64.
- **Radius:** `--r-sm` 6 (chips), `--r-md` 10 (inputs/buttons), `--r-lg` 14 (cards), `--r-xl` 20 (sheets).
- **Elevation:** a lighter surface plus a border, never a glow. Sheets get `0 16px 40px rgba(0,0,0,.45)`.
- **Typography:** IBM Plex Sans for UI, IBM Plex Mono with `tabular-nums` for every number. Self-hosted woff2, so nothing loads from outside on the LAN.

| Role | Font | Size | Weight |
| --- | --- | --- | --- |
| display | Mono | 40/48 | 500, −0.02em |
| title-lg | Sans | 28/36 | 600 |
| title | Sans | 20/28 | 600 |
| body | Sans | 15/24 | 400 |
| label | Sans | 13/20 | 500 |
| figure | Mono | 15/24 | 500 |
| micro | Mono | 12/16 | 400 |

**Number rules** (`format.go`, unit-tested):

| Case | Example | Rule |
| --- | --- | --- |
| Fiat | `$12,480.20` | 2 decimals, thousands separators, currency prefix |
| Coin amount | `0.84213907` | up to 8 decimals; trailing zeros trimmed past 4 |
| Change | `▲ +4.21%` / `▼ −1.08%` | arrow + sign + color, never color alone; true minus `−` (U+2212); zero is `– 0.00%` in `--text-3` |
| Hidden (privacy mode) | `$ ••••••`, `•••• BTC` | every fiat and coin amount masked. Each figure renders both a value span and a mask span; `body.amounts-hidden` swaps them in CSS. No blur, and the hidden values are removed for screen readers too. |
| Address | `bc1q8f…a7x4` | first 6 + last 4, never truncating the ends; copy button beside it; full value in `title` |
| Stale price | `~$3,912.00` in `--warn` | price older than 15 min (the design rule). Last-good is used up to `staleMaxHours` (24 h), then shown as "—" and excluded. |

**Components** (templ funcs in `components.templ`, mirroring the Components board):
- **Buttons:** 44 px high. Primary (accent, one per view), secondary (`--surface-2` + `--border-strong`), ghost, destructive (outlined `--loss` on `--loss-soft`, never solid red), icon-only 44×44 with `aria-label`, and disabled.
- **Source chip:** the dot *shape* encodes type, so it reads without color: circle = exchange, diamond = hot wallet, square = hardware, hollow ring = manual (an addition, in `--src-other`).
- **Status chips** (mono 12): `● Synced`, `◐ Stale 3h`, `✕ Key expired` / `✕ Error`, `Read-only`, `Watch-only`, `2 conf` (pending, warn).
- **Segmented range control** `1D 1W 1M 1Y All`: selected = `--surface-1` + an inset ring + `aria-pressed="true"`. **Underline tabs** for views: a 2 px accent inset shadow.
- **Asset row / holdings table:** columns Asset (monogram disc + name + "N sources"), Held in, Price, Holdings, Value, 24h. Scrolls inside its box below 760 px.
- **Coin marks:** ticker monograms on tinted discs (BTC amber, ETH/accent blue, SOL purple, stables green, others neutral). No third-party logos by default; a Settings toggle can show Jupiter logos for SPL tokens.
- **Stat tile**, and **account card** (3 px rail in the source color, name, kind, status chip, value, meta; the whole card links to wallet detail).
- **Inputs:** 44 px, `--surface-2`, `--border-strong`. Focus = accent border + a 3 px `--accent-soft` halo. Always a visible `<label>`.
- **Address row** with copy and QR buttons. **Allocation bar** with 2 px gaps and a legend that always carries the %.
- **Notices:** info (`--accent-soft` + shield icon) and warning (`--warn-soft` + triangle).
- **Icons:** inline 2 px-stroke SVG, `aria-hidden` when decorative.
- **Allocation colors keep a fixed order in both themes:** BTC always first and warm (`--src-hardware`), then accent, `--src-hot`, `--gain`, then "Other" (`--src-other`) for everything past the top 4.
- **Chart:** an area fill of the accent at 8% opacity plus a 2 px accent line and 3 horizontal gridlines in `--row`. Server-rendered SVG with `vector-effect: non-scaling-stroke` and `role="img"` + `aria-label`.

**Screens** (layout from the boards):
- **App shell:** at desktop, a 232 px sidebar (CryptKeep mark; nav Overview, Wallets, Activity, Reports, Alerts, Settings; a "Portfolios" list with source counts plus a dashed "New portfolio" button). Under 760 px it becomes the bottom nav (Overview, Wallets, Activity, Settings) and the portfolio name becomes a dropdown button.
- **Overview** (`Portfolio.dc.html`):
  - Header: "Portfolio" label + name, a sync chip, hide and refresh icon buttons, and the "Add source" primary button.
  - Total value card: display figure, change for the selected range, segmented range control, area chart.
  - Allocation card (bar + legend with values).
  - "Sources" grid of account cards with a "Manage" link.
  - Holdings table with search and a "Held in" column; DeFi legs are grouped under their position.
  - Plus, from the plan: a "Watching" list and the P&L columns toggle.
- **Wallet detail** (`Wallet.dc.html`):
  - Back link to the portfolio; a header with a type icon tile, name, and chips (type, `Watch-only`, `● Synced · block 866,412`).
  - Actions: **Edit** and **⋯** (Remove lives in the Connection panel). The design's "Send with device" is **dropped**, because the app is read-only.
  - Stat tiles: Balance (+ native amount), Cost basis (avg per coin), Unrealized P/L.
  - Tabs: **Holdings** (tokens + DeFi), **Activity**, **Addresses** (derived addresses with balance and used/unused, BTC), **UTXOs** (BTC).
  - Aside:
    - **Receive address:** the next unused receive address from the xpub/descriptor scan, with a QR SVG, a copy button, and the warning notice "Verify this address on your device before sharing it" for hardware wallets.
    - **Connection panel:** type, imported via, derivation/descriptor, addresses used "7 of 27", node, last sync, and a destructive "Remove wallet" button.
  - Exchanges show Holdings + Activity, plus key status in Connection.
- **Add a source** (`MobileConnect.dc.html`):
  - A two-step flow with a progress bar. Step 1 groups: Exchanges (Coinbase, Kraken) and Self-custody (Bitcoin wallet, Solana wallet, Manual holdings).
  - Step 2 is the form for that type, with a "Test connection" check before saving (details under Sources).
  - Desktop shows it as a centered sheet (`--r-xl`); mobile shows it full screen.
- **Activity, Portfolios manager, Reports (P&L + CSV exports + cost-basis import), Alerts, Settings:** built from the same components, following the design's density and rules.

**Accessibility (from the design):** touch targets ≥ 44 px; real `<button>`/`<a>`/`<label>`; `aria-label` on icon-only buttons; text contrast ≥ 4.5:1 (the token pairs are chosen for it); color never carries meaning alone (arrows, signs, dot shapes, text labels).

## Sources (managed in the UI)
**Storage: `data/sources.yaml`**
```yaml
version: 1
sources:
  - { id: coinbase, type: coinbase, label: Coinbase, kind: exchange,
      keyName: "organizations/…/apiKeys/…", secret: "enc:v1:<base64 nonce|ciphertext>" }
  - { id: kraken, type: kraken, label: Kraken, kind: exchange, apiKey: "enc:v1:…", secret: "enc:v1:…" }
  - { id: cold, type: bitcoin, label: "Cold storage", kind: hardware, key: "zpub6r…" }
  - { id: vault, type: bitcoin, label: "2-of-3 multisig", kind: hardware, descriptor: "wsh(sortedmulti(2,…))#chk" }
  - { id: phantom, type: solana, label: "Phantom", kind: hot, address: "7xKX…" }
  - { id: mom-safe, type: manual, label: "Paper wallet at mom's", kind: manual,
      holdings: [ { asset: BTC, amount: 0.25 }, { asset: ETH, amount: 2, note: "…" } ] }
```
**Secrets:**
- API secrets (and the Coinbase PEM) are sealed with AES-256-GCM. The key comes from `CRYPTKEEP_SECRET_KEY` (base64, 32 bytes). If that is unset, a key is generated once into `data/secret.key` (mode 0600) and the README tells you to back it up.
- Secrets are **never rendered back** into HTML. The UI shows `kr_live_••••3f9a` (last 4), and editing requires re-entering the secret.
- **Limits of encryption at rest:** it protects a leaked `sources.yaml` or backup. It doesn't protect against someone with the key file on the same disk, and the README says so plainly.

**Add / edit / remove flow:**
- Create, edit and delete go through the mutation guard.
- "Test connection" runs that source's fetch once and shows the result inline: balances found / error. Save stays disabled until a test passes; there's an explicit "Save anyway" for offline cases.
- Saving writes `sources.yaml` atomically, with a backup to `data/backups/`. The registry hot-reloads, and the refresher picks the change up immediately and pushes it over SSE.
- Removing a source asks for confirmation and also offers to remove it from portfolios.
- Each wallet `kind` (hardware/hot) is chosen in the form. Defaults: xpub/descriptor → hardware; Solana address → hot.
- **Export/import of sources:** CLI and Settings, YAML. Secrets are excluded by default ("include encrypted secrets" re-imports only with the same key).
- **Seeding:** on first run, `config.yaml` can carry a `seedSources:` list for unattended setup.

**`config.yaml`** now holds only server settings:
- `refreshMinutes`, `staleMaxHours`, `activityMinutes` (default 15)
- `bitcoin.esploraUrl`, `bitcoin.gapLimit`
- `solana.rpcUrl` (Helius recommended), `solana.includeStakeAccounts`, `solana.hideUnpriced`, `solana.minUsd`, `solana.hideMints`, `solana.tokenOverrides`, `solana.defi`
- `ntfy.url`, `ntfy.redactAmounts`
- `activity.backfillDays` (default 365)
- `showTokenLogos`

**Wallet refs** used by portfolios are `<source id>`, e.g. `cold`, `coinbase`, `mom-safe`. Ids are immutable once created; labels can change freely.

### Per-source details
- **Coinbase** (View-only CDP key):
  - Auth: an ES256 JWT per request. Header `kid` = key name plus a random `nonce`; claims `sub`, `iss:"cdp"`, `nbf`, `exp` = +120 s, `uri:"<METHOD> api.coinbase.com<path>"`. The PEM is parsed as an EC key, with a PKCS8 fallback.
  - Balances come from Advanced Trade `GET /api/v3/brokerage/accounts` (cursor) **and** `GET /v2/accounts` (`next_uri`), reconciled by UUID:
    - Where both list an account, the v2 balance wins.
    - v2-only vaults and staking accounts are added.
    - `ETH2` and `*-staked` map to the base asset with `Staked=true`.
    - Any mismatch with the Coinbase app is shown on a debug panel.
  - Activity: `GET /v2/accounts/{id}/transactions` (buy, sell, send, receive, trade/convert, advanced_trade_fill, staking/inflation rewards). `native_amount` gives USD at the time; `network.hash` is used to match transfers to your own wallets.
- **Kraken** (key permissions: **Query Funds + Query Ledger Entries**):
  - Signing: `API-Sign` = base64(HMAC-SHA512(b64dec(secret), path + SHA256(nonce + body))). The nonce is `atomic` max(now, last+1).
  - A non-empty `error[]` counts as a failure.
  - `Balance`: legacy codes are normalized (`XXBT`→BTC, `ZUSD`→USD…), and Earn suffixes (`.S .M .F .B .P`, `ETH2`) are merged as staked.
  - `Ledgers` (paged `ofs`, 50 at a time) are grouped by `refid` into trades, deposits, withdrawals and rewards.
- **Bitcoin:**
  - xpub/ypub/zpub: version bytes → script type (BIP44/49/84), plus a `p2tr` override.
  - **Descriptors:** `pkh`, `wpkh`, `sh(wpkh)`, `tr(KEY)`, `wsh`/`sh(wsh)` of `multi`/`sortedmulti` (BIP67 sort). Supports origins, `/<0;1>/*` multipath, `h`/`'`, and the BIP380 checksum.
  - **Wallet files:** Caravan/Unchained JSON or a Sparrow descriptor export, uploaded in the Add-source form.
  - Gap-limit scan over receive and change through the shared `addressAt(chain, i)`. Esplora `/address/{a}`: balance = chain + mempool funded − spent. Concurrency is 4, with 429 backoff and per-key address caching.
  - **Activity:** `/address/{a}/txs` (+ `/txs/chain/{last}` paging), re-fetched only for addresses whose `tx_count` changed. Net wallet effect = outputs to wallet − inputs from wallet, so receive/send/self-transfer and the fee all fall out. Confirmations come from `/blocks/tip/height`.
  - UTXOs come from `/address/{a}/utxo`. The next unused receive address is the receive address after the last used one.
  - Privacy note: a public Esplora server sees your addresses together; point `esploraUrl` at your own node.
- **Solana:**
  - SOL via `getBalance`. SPL tokens via `getTokenAccountsByOwner` (jsonParsed) for Token **and** Token-2022, summed per mint. wSOL is merged into SOL.
  - Native stake via `getProgramAccounts` (withdrawer memcmp at offset 44); if the RPC rejects it, that's a soft warning. NFTs are counted, not valued.
  - Token identity, cached in SQLite: Jupiter token API → Token-2022 metadata extension → Metaplex PDA → short mint.
  - Pricing is Jupiter by mint (USDC/USDT pegged as a fallback), with **no symbol fallback** for unverified tokens.
  - Asset key is `spl:<mint>`, except well-known mints, which map to exchange symbols (USDC, USDT, JUP, BONK, WIF, PYTH, RAY, JTO…).
  - Spam control: `hideUnpriced`/`minUsd` move tokens into a collapsed "N hidden tokens" row; `hideMints` and `tokenOverrides` adjust the rest.
  - **Activity:** `getSignaturesForAddress` for the owner **and each token account**. Incoming SPL transfers to an existing token account don't reference the owner, so this second query is needed. Paged with `until` the last known signature, backfilled up to `backfillDays`.
    - Then `getTransaction` (jsonParsed, `maxSupportedTransactionVersion: 0`). The owner's legs come from pre/post SOL and token balances.
    - Classification: multiple opposite-sign legs = swap; single leg = send/receive; stake program = stake. Failed transactions are shown but don't count.
  - **DeFi** (`solana/defi/`): adapters return underlying-asset holdings tagged `Protocol`/`Position`/`PositionKind`, and claim their position NFTs.
    - **Orca Whirlpool** and **Raydium CLMM**: position PDA from the NFT mint, then `getMultipleAccounts` for positions and pools, then amounts via `clmm.go`.
      - `clmm.go` does Q64.64 `math/big`, exact tick math, and the below/in/above-range formulas.
      - Fees are not included in v1, and the UI says so.
    - **Raydium AMM/CPMM** LP tokens: value from the pool API's reserves/supply.
    - **Jupiter:** locked JUP from the vote-escrow account (`PositionKind=locked`); perps via the perps API, with account decoding as a fallback (collateral + uPnL). JLP is an SPL token.
    - Program IDs, layouts and endpoints are pinned as constants with source links. An adapter failure is a per-wallet warning.
- **Manual:** holdings typed in the UI, priced through the normal chain. Badged "manual", with a hollow-ring dot.

## Prices, stale handling, history
- **Spot price chain:**
  - Crypto: Coinbase `/v2/prices/{SYM}-USD/spot` → Kraken public `Ticker` (`c[0]`) → Jupiter (by mint).
  - Fiat FX: Coinbase `/v2/exchange-rates`. USD/USDC/USDT are pegged.
  - Fetched in parallel (limit 8).
- **24h change before history exists:** Coinbase Exchange `/products/{SYM}-USD/stats`, or Jupiter's 24h change.
- **Historical daily prices** (for activity fiat values and Kraken reward cost basis): Coinbase Exchange candles (`granularity=86400`), cached in the `price_history` table. SPL tokens have no free history source, so the value at the time is "—" unless the exchange supplied it.
- **Stale handling:** every successful price and every wallet's holdings go into a last-good cache (SQLite). When a fetch fails, the last-good value is used with `Stale`/`AsOf`.
  - Prices older than 15 min get the `~` tilde in warn color. Sources show `◐ Stale 3h`.
  - After `staleMaxHours`, values are excluded and the source shows its error.
  - The header notes "includes stale data". Alerts never fire on stale data.
- **Refresh and history:**
  - A background refresher runs every `refreshMinutes` (singleflight; forced refresh at most once per 30 s) and publishes to the broker.
  - History rows `{ts, prices{asset:usd}, amounts{walletRef:{asset:amount}}}` go into SQLite at most every 30 min. Rows older than 30 days are compacted to daily.
  - Any portfolio's chart is computed retroactively from these rows. Ranges are 1D/1W/1M/1Y/All.

## Portfolios (YAML: `data/portfolios.yaml`)
```yaml
version: 1
portfolios:
  - { id: long-term, name: Long-term, wallets: all, assets: [BTC, SOL, "spl:J1toso1…"], watch: [ETH, LINK] }
  - { id: kids, name: "Kids' wallets", wallets: [kids-phantom, kids-cash], assets: all }
  - { id: memes, name: Memecoins, assets: [BONK, WIF] }
  - { id: watch, name: Watchlist, wallets: [], watch: [ETH, AVAX] }
```
- **Membership:** a holding is included iff its wallet ∈ `wallets` and its asset ∈ `assets` (`all` matches everything; the default is `all`). Watch entries show live price only.
- Manual holdings are now a source type, so they're grouped like any other wallet.
- **Storage and file safety:**
  - Mutex plus atomic tmp + fsync + rename.
  - A backup before every write (newest 30 kept in `data/backups/`).
  - mtime reload picks up hand edits.
  - `KnownFields(true)` gives line-numbered errors.
- **Save & restore:**
  - Export all or one portfolio (download), and import with Merge or Replace (validated first, backup first, with a summary).
  - The backups list has Restore.
  - CLI: `portfolios export|import [--replace]`.
- **UI:**
  - Sidebar list + "New portfolio".
  - Editor: name; wallet checkboxes or "All"; asset picker with debounced search, held assets first, and any symbol or mint allowed; watch rows.
  - Overview filtered to the selected portfolio, with a "% of net worth" line.

## Activity
- **Model** (SQLite `activity` table):
  - Fields: `{id, sourceId, time, kind(receive|send|transfer|trade|swap|reward|stake|fee|other), legs[{asset, amount±, usdAtTime?}], fee?, counterparty?, txHash?, explorerURL?, status(pending|confirmed|failed), confirmations?, note}`.
  - Unique on `(sourceId, externalId)`.
- **Sync** (`activity/sync.go`):
  - Runs per source every `activityMinutes`, incrementally from a stored cursor (Esplora last txid per address, Solana last signature per account, Coinbase newest id, Kraken newest ledger time).
  - Backfill runs gradually in small batches per tick until `backfillDays` is reached, to respect rate limits. Progress shows in Settings → Sources.
- **Own-wallet transfer matching** (`transfers.go`):
  - A send on one source is paired with a receive on another when the asset matches, and either the tx hash matches (Coinbase `network.hash`, Kraken withdrawal txid, on-chain hash) or the amount matches within the fee and time is within 2 h.
  - The pair becomes one "Transfer · Coinbase → Cold storage" (the design's "Received from Coinbase") and doesn't change cost basis.
- **UI:**
  - Global Activity page and the wallet-detail Activity tab. Rows follow the Wallet board: a direction disc (`↓` gain-soft / `↑` loss-soft / `⇄` for swap and transfer), title, mono detail (date · short hash · fee), status chip (`2 conf` warn / `Confirmed` / `Failed`), signed mono amount plus fiat at the time.
  - Filters: portfolio, wallet, asset, kind, date (Datastar signals plus `@get` that patches the list).
  - "Load older activity" button, with `data-on-intersect` for auto-load. Explorer links: mempool.space, Solscan.

## Change, cost basis & P&L, alerts, exports
- **Change:** value change (1D/1W/1M/1Y/All) per portfolio and wallet, and price change per asset, all from history. The tooltip notes that value change includes deposits; P&L is the cost-aware number.
- **Cost basis:**
  - Average cost per asset, **global across wallets**. Transfers don't change basis.
  - Inputs:
    - **Activity:** Coinbase buys/sells/converts with `native_amount`; Kraken trades via ledger pairs; rewards at FMV; on-chain receives without a matched source become "unknown basis" until set.
    - **Manual** (`data/costbasis.yaml`): lots or total invested, with an optional `override`.
    - **CSV import** for history beyond API reach: Coinbase transaction report (preamble skipped) and Kraken Ledgers. De-duplicated against API activity by external id.
  - Output:
    - Per asset: avg cost, basis, unrealized P&L $/%, realized P&L per year.
    - Per portfolio: basis allocated pro-rata.
    - A "basis coverage 82%" line.
    - The wallet-detail stat tiles.
  - Disclaimer: not tax advice.
- **Alerts (ntfy):**
  - Rules live in `data/alerts.yaml`, edited on `/alerts`:
    - `balance_drop`: cold wallet decreased between two non-stale snapshots; urgent, re-fires only on a new drop.
    - `price` above/below, with 2% hysteresis.
    - `portfolio_move` ±X% over a window.
    - `source_down` for more than N minutes.
    - `large_activity`: a new send above $X from any self-custody wallet, from the activity feed. Added because it pairs well with `balance_drop`.
  - Cooldowns, with state in SQLite.
  - Delivery: ntfy `POST` with `Title`/`Priority`/`Tags`/`Click` headers and an optional `Bearer` token. `redactAmounts` is on by default.
  - The page has a test button and the last 50 alerts.
- **CSV export:** holdings, history, P&L and activity (filtered), from Reports and Activity.

## Routes (chi; no `/api/` prefix; all POSTs pass the guard)
| Route | What it does |
| --- | --- |
| `GET /`, `GET /p/{id}` | Overview, full render. |
| `GET /stream?pf=` | Persistent SSE that patches `#dashboard` (and `#sources`) on each snapshot, with keep-alive. |
| `GET /view?pf=` | Portfolio switch: patches the dashboard. |
| `GET /chart?pf=&range=` | Patches the chart and its change line. |
| `POST /refresh` | Forced refresh. |
| `GET /wallets`, `GET /w/{id}` | Wallet detail, full render. |
| `GET /w/{id}/tab/{holdings\|activity\|addresses\|utxos}` | Tab patch. |
| `GET /sources/new`, `GET /sources/new/{type}` | Add-source flow steps. |
| `POST /sources/test` | Test connection. |
| `POST /sources`, `POST /sources/{id}`, `POST /sources/{id}/delete` | Create, update, remove. |
| `GET /activity` | Activity page. |
| `GET /activity/list?filters&before=` | List patch / load older. |
| `GET /portfolios`, `POST /portfolios…` | Portfolio CRUD. |
| `GET /portfolios/export[/{id}]` | Export. |
| `POST /portfolios/import` | Import. |
| `POST /portfolios/restore/{b}` | Restore a backup. |
| `GET /reports`, `POST /reports/basis`, `POST /reports/import` | P&L, manual basis, CSV import. |
| `GET /alerts`, `POST /alerts…`, `POST /alerts/test` | Alert rules + test. |
| `GET /settings`, `POST /settings/theme` | Settings. The theme is client-side; the POST is a no-op stub kept only if needed. |
| `GET /export/{holdings\|history\|pnl\|activity}.csv` | CSV downloads. |
| `GET /assets/search?q=` | Asset picker. |
| `GET /portfolio.json?pf=` | JSON snapshot. |
| `GET /healthz` | Health; exempt from auth. |
| `GET /manifest.webmanifest`, `/static/*` | PWA and assets. |

- **Guard:** requires `Datastar-Request: true` and a same-origin `Origin`. Restore and backup names are checked against the directory listing.
- **Optional auth:** `PORTFOLIO_PASSWORD` turns on basic auth (constant-time compare) for everything except `/healthz` and the manifest/icons.

## Datastar wiring
- **Signals on `<body>`** (never patched): `{hide, theme, range:'1W', pf, showHidden_<wallet>…}`.
- **Hide amounts:** `data-class="{'amounts-hidden': $hide}"` on body.
- **Theme:** the Settings segmented control sets `$theme`, and a `data-effect` writes `document.documentElement.dataset.theme` plus localStorage. A guarded `data-init` reads it back.
- **Portfolio switching:** sidebar items set `$pf`, call `history.replaceState`, then `@get('/view?pf=…')`.
- **Live stream:** a hidden, never-patched element holds `data-effect="@get('/stream?pf=' + $pf)"`. When `$pf` changes, the previous request is auto-cancelled; verify this in the bundle, and the fallback is noted in the steps.
- **Range control:** `$range='1M'; @get('/chart?pf='+$pf+'&range=1M')`. Pressed state via `data-attr:aria-pressed="$range === '1M' ? 'true' : 'false'"`, selected style via `data-class` object form.
- **Copy buttons:** `data-on:click="navigator.clipboard && navigator.clipboard.writeText('<escaped>')"`.
- **Forms:** the add-source and editor forms are `<form method="post">` with Datastar `contentType:'form'`. File uploads (wallet file, CSV, YAML import) need multipart; verify it in the bundle, and the fallback is a native multipart post + redirect for those three forms only.
- **Silent-failure rules:**
  - Colon event syntax and `__` modifiers.
  - Bare `data-indicator` names.
  - `data-show`, not `data-attr:hidden`.
  - Unique ids on patch targets.
  - templ comments only.
  - `data-init` for the stream.
  - No `<script>` blocks beyond the Datastar module.
  - Every browser API guarded by truthiness.

## Deployment
- `task generate|dev|run|build` (go-task; was a Makefile). `task dev` runs air (rebuild + restart; browser reload via its proxy on :8788). Cross-compiles for linux/amd64 and arm64.
- Listens on `0.0.0.0:8787`.
- **Docker:** `golang:1.26-alpine` (templ generate + build, `CGO_ENABLED=0`) → distroless. Mounts `config.yaml`, `.env` and `data/`. `HEALTHCHECK` runs `crypto-portfolio healthcheck`. Works with podman compose.
- **README:**
  - View-only Coinbase CDP key, and the Kraken key permissions.
  - Exporting a zpub or descriptor from Sparrow, Ledger, Trezor, Unchained or Nunchuk.
  - Helius RPC.
  - The privacy note.
  - Backing up `data/` (especially `secret.key`).
  - CSV downloads and ntfy setup.
  - `tailscale serve` for HTTPS, password and PWA install.
  - The YAML references for sources and portfolios.

## Implementation order (milestones; test-first for crypto, money and parsing)
- **M1: Core app in Vault design.** Steps 1–8.
- **M2: Portfolios + change.** Step 9.
- **M3: Activity.** Step 10.
- **M4: Solana DeFi.** Step 11.
- **M5: Cost basis, P&L, CSV.** Step 12.
- **M6: Alerts.** Step 13.

1. **Scaffold.** `go mod init`, save this plan to `docs/plan.md`, then config, `secrets` (seal/open round-trip; wrong key fails; `enc:v1:` format), `store` (SQLite migrations), httpx.
2. **Bitcoin.** BIP vectors (mnemonic "abandon ×11 about", account 0, first receive address), each confirmed against its spec text:
   - BIP44 `xpub6BosfCnifzxcFwrSzQiqu2DBVTshkCXacvNsWGYJVVhhawA7d4R5WSWGFNbi8Aw6ZRc1brxMyWMzG3DSSSSoekkudhUd9yLb6qx39T9nMdj` → `1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA`
   - BIP49 `ypub6Ww3ibxVfGzLrAH1PNcjyAWenMTbbAosGNB6VvmSEgytSER9azLDWCxoJwW7Ke7icmizBMXrzBx9979FfaHxHcrArf3zbeJJJUZPf663zsP` → `37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf`
   - BIP84 `zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs` → `bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu`
   - BIP86 from its spec.
   - Descriptors: BIP380–386 examples and Bitcoin Core `descriptor_tests.cpp` multisig vectors (`sh(multi)`, `wsh(sortedmulti)`, `sh(wsh(multi))`, `<0;1>`), plus a Caravan JSON fixture.
   - Esplora scan, UTXOs and next-unused address against httptest.
3. **Kraken.** Signature against the docs example: path `/0/private/AddOrder`, nonce `1616492376594`, body `nonce=1616492376594&ordertype=limit&pair=XBTUSD&price=37500&type=buy&volume=1.25`, expected `4/dpxb3iT4tp/ZCVEwSnEsLxx0bqyhLpdfOpc6fn7OR8+UClSV5n9E6aSS8MPtnRfp32bAb0nmbRn6H8ndwLUQ==` (confirm on the live docs page). Then normalization and `error[]` handling.
4. **Coinbase.** JWT with a throwaway P-256 key; pagination for both APIs; reconciliation (overlap → v2 wins; vault only in v2; `ETH2` → staked ETH).
5. **Solana.** Fake RPC holding:
   - SOL + wSOL.
   - Two USDC accounts (summed).
   - A Token-2022 token with a metadata extension.
   - A Metaplex-named token.
   - A fake "USDC" mint (stays `spl:`, hidden).
   - An NFT (counted).
   - A stake account.
   - Plus a `getProgramAccounts` rejection, which gives a warning.
6. **Prices, aggregation, stale, refresher, broker, history.**
   - Stale: price failure → last-good with tilde; > 24 h → excluded; wallet failure → last-good holdings; survives restart.
   - Broker unsubscribe doesn't leak (checked under `-race`).
7. **Sources registry + UI.**
   - `sources.yaml` round-trip; secrets never rendered (the render test greps the output for the plaintext); hot reload; test-connection endpoint; seeding from `config.yaml`; sources export without secrets.
8. **CryptKeep UI.**
   - `tokens.css` and `app.css` transcribed from the boards; Plex woff2 self-hosted; vendored Datastar v1.0.4 (check the version string, `data-effect` request cancellation, multipart form posts).
   - templ components with render tests; `format.go` number-rule tests (every row of the table above).
   - App shell, Overview, Wallet detail (Holdings / Addresses / UTXOs; QR via `rsc.io/qr`), Add-source flow, Settings (theme, logos toggle).
   - Guard, optional auth, `/healthz` + `healthcheck`, manifest/icons, Docker, README.
9. **Portfolios + change.** YAML round-trip; `KnownFields`; filter semantics (assets, wallets, intersection, watch, `all`, missing ref); merge/replace import; backups pruned to 30; atomic save under injected failure; retroactive history; CLI. Then the editor UI, sidebar list, and change % from history plus the stats fallback.
10. **Activity.**
    - Parsers and classifiers against fixtures: Esplora txs (receive, send with change, self-transfer, fee); Solana transactions (SOL send, SPL receive to an existing token account, swap with 2 legs, failed tx, Token-2022); Coinbase v2 transaction types; Kraken ledger refid groups.
    - Incremental cursors: a re-sync adds nothing duplicate.
    - Transfer matching: hash match, amount + time match, no false match across different assets.
    - Historical price cache.
    - Then the feed UI, the wallet Activity tab and filters.
11. **Solana DeFi.**
    - Spike first: confirm program IDs, IDL layouts and endpoints, written into `docs/defi-sources.md`.
    - `clmm.go` against SDK vectors.
    - Orca/Raydium/Jupiter decoders against captured base64 fixtures in `testdata/`.
    - UI grouping under positions.
12. **Cost basis + P&L + CSV.**
    - Avg-cost engine: buy/buy/sell, convert, rewards at FMV, transfers neutral, realized P&L per year.
    - Activity → lots.
    - Coinbase/Kraken CSV parsers (preamble, refid pairing), de-duplicated against API activity, idempotent re-import.
    - Then the Reports UI, wallet stat tiles, dashboard P&L columns, CSV exports.
13. **Alerts.**
    - Engine: balance drop fires once and never on stale data; price hysteresis; cooldown; portfolio move; source down; large activity.
    - ntfy client against httptest (headers, token, redaction).
    - Then the `/alerts` UI and test button.

## Verification
- After a clean `go tool templ generate`, `git status` shows no changes.
- `go test ./...` and `go test -race ./...` cover every case listed in the steps above, plus:
  - Template guard checks: no hyphenated or dot-modifier Datastar attributes, no `<!--` comments, no `$` in `data-indicator`, required ids present.
  - Every icon-only button has an `aria-label`.
  - No rendered secret plaintext.
- `go vet ./...` passes clean.
- **Smoke test with no secrets:**
  - Add the genesis address and a token-rich public Solana address through the **Add source UI**.
  - `curl /portfolio.json | jq` shows balances, prices and totals.
  - `curl -N … /stream` shows patch events and keep-alives.
  - The Activity feed fills for both addresses, and backfill progresses.
- **Design fidelity:**
  - Screenshot Overview, Wallet detail and Add source at 1440 px and 390 px, in dark and light.
  - Compare each side by side with the canvas boards (Portfolio, Wallet, MobilePortfolio, MobileConnect, and the light variants): tokens, type scale, spacing, chips, rails, number formatting.
  - Check contrast on the light theme.
- **Behavior in the browser:**
  - Hide toggled twice survives a reload and an SSE patch.
  - Range control changes only the chart.
  - Portfolio switch, then a refresh, patches the selected portfolio.
  - Stale: break the Esplora URL and confirm the last-good balance shows with the tilde and stale chip, and the total holds.
  - Add, test, edit and remove a source, and confirm it hot-reloads without a restart.
  - Portfolio export → delete → import → restore round-trip.
  - The console has no `ExecuteExpression` errors.
- `docker build` / `podman build` succeeds and the container reports healthy. The PWA installs on iOS, and on Android via the `tailscale serve` HTTPS URL.
- **With your real data, per milestone:**
  - M1: Coinbase total matches the Coinbase app (staked and vaults included); xpub/multisig totals match Sparrow; Solana tokens match Phantom/Solscan.
  - M3: activity matches the explorers and exchange histories for a sample month; a Coinbase → cold-storage withdrawal shows as one transfer.
  - M4: DeFi values within about 1% of the Orca/Raydium/Jupiter UIs.
  - M5: avg cost matches Coinbase for one asset.
  - M6: ntfy test arrives; a deliberately low price rule fires once.


## Implementation notes (M1, 2026-10-16)
Decisions made during M1 that refine or deviate from the plan above:
- **Live stream URL is fixed** (`@get('/stream', {filterSignals: …})`, portfolio/range sent as signals): the vendored Datastar v1.0.4 only auto-cancels a previous request to the *same URL*. The stream runs on the overview only; wallet pages refresh on navigation.
- **Pricing is mint-first:** anything with a Solana mint (including exchange holdings of mapped tokens like JUP on Kraken) is priced by mint on Jupiter, then the symbol chain. A Coinbase spot price is ignored when the Coinbase Exchange product isn't `online` (Coinbase still quotes delisted JUP ~1000× low).
- **Unverified SPL tokens are hidden** (and excluded from totals) unless in `solana.allowMints`; Jupiter's verified list (one request/day) names most tokens locally. Per-fetch caps (search batches, on-chain lookups, a 20 s budget) keep spam-heavy wallets inside the fetch deadline; Jupiter price lookups cap chunks per call and negative-cache unpriced mints for an hour.
- **Charts start when every current wallet has history,** so adding/removing a source doesn't draw a cliff.
- **Static assets are content-hash versioned** (`?v=`), cached immutably.
- **Coinbase:** use an ECDSA CDP key; Ed25519 keys work for Advanced Trade but Coinbase's v2 API (vaults, staking) documents ECDSA only.
