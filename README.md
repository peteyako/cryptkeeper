# Vault

A self-hosted, **read-only** crypto portfolio monitor. One page for everything you hold:
Coinbase, Kraken, Bitcoin cold storage (xpub, descriptors, multisig) and Solana wallets
(SOL, every SPL token, staking), with live updates, stale-but-visible data when an API
hiccups, and a design that puts numbers first.

- Single Go binary, no database server: state lives in `data/` (YAML you can edit plus a SQLite file).
- Server-rendered with [templ](https://templ.guide) and [Datastar](https://data-star.dev): live updates over SSE, almost no JavaScript.
- Read-only by design. Vault only ever holds **public** keys, addresses and **view-only** API keys. It cannot move funds.

> Never paste a seed phrase or private key into Vault. It will refuse obvious mistakes, but the rule is yours to keep.

## Quick start

```sh
cp config.example.yaml config.yaml     # optional: defaults work
make run                               # or: go run . serve
```

Open http://localhost:8787 and click **Add source**.

Docker / Podman:

```sh
cp config.example.yaml config.yaml && touch .env && mkdir -p data
docker compose up -d        # or: podman compose up -d
```

The container listens on `:8787`, stores everything in `./data`, and reports health via
`crypto-portfolio healthcheck` (declared in `docker-compose.yml`).

On Linux, the image runs as the distroless non-root user (uid 65532), so make `data/`
writable by it first: `sudo chown 65532:65532 data` (rootless Podman on macOS needs nothing).

## Adding sources

Sources are managed in the web UI (**Add source**) and stored in `data/sources.yaml`.
Use **Test connection** before saving.

### Coinbase
1. Go to the [Coinbase Developer Platform](https://portal.cdp.coinbase.com/) → API keys → Create API key.
2. Choose the **ECDSA** signature algorithm (Coinbase's v2 accounts API, which Vault uses for vaults and staked balances, documents ECDSA keys only).
3. Permissions: **View** only. No trade, no transfer.
4. Paste the key name (`organizations/…/apiKeys/…`) and the private key (PEM).

### Kraken
1. Settings → API → Create API key.
2. Permissions: **Query Funds** and **Query Ledger Entries** only.
3. Paste the API key and private key.

### Bitcoin
Paste any of:
- An extended public key: **xpub** (legacy), **ypub** (nested SegWit), **zpub** (native SegWit). Taproot xpubs: choose *Taproot* under script type.
- An **output descriptor**, including multisig, e.g. `wsh(sortedmulti(2,[fp/48h/0h/0h/2h]xpub…/<0;1>/*,…))#checksum`.
- A single **address**.
- Or upload a **wallet file**: a Caravan/Unchained wallet config (JSON) or a Sparrow descriptor export.

Where to find them:
- **Sparrow**: Settings → Script Policy shows the descriptor; *Export* → *Output Descriptor*.
- **Ledger Live**: Account → ⋯ → Edit account → Advanced → extended public key.
- **Trezor Suite**: Account → Details → Show public key.
- **Unchained / Caravan**: download the wallet configuration JSON.
- **Nunchuk**: Wallet → ⋯ → Export wallet → descriptor / BSMS file.

Vault derives receive and change addresses with a gap limit of 20 (configurable).

**Privacy:** balances are looked up on `mempool.space` by default, which lets that server see
your addresses together. Point `bitcoin.esploraUrl` at your own mempool/electrs node to avoid it.

### Solana
Paste the wallet's public address. Vault reads SOL, every SPL token (classic and Token-2022),
native stake accounts, and counts NFTs without valuing them.

Tokens that aren't on Jupiter's verified list, have no price, or are worth less than `minUsd`
are collapsed into "N hidden tokens" and left out of totals: scam airdrops often carry a fake
price. If you really hold an unverified token, add its mint to `solana.allowMints`.

The public RPC is heavily rate-limited and rejects stake-account lookups. A free
[Helius](https://www.helius.dev/) key is recommended:

```yaml
solana:
  rpcUrl: https://mainnet.helius-rpc.com/?api-key=YOUR_KEY
```

### Manual holdings
Coins held anywhere Vault can't read, one per line: `BTC 0.25 owed by Sam`. Use
`spl:<mint>` for Solana tokens.

## Portfolios

Group what you hold into named portfolios (sidebar → **New portfolio**, or **Manage**):

- **By coin or token** — e.g. "Memecoins: BONK, WIF" across every wallet and exchange.
- **By wallet** — e.g. "Kids' wallets" = everything in two specific sources.
- **Both** — only those coins inside those wallets.
- **Watch list** — coins shown with a live price but not counted.

Each portfolio gets its own tab with total, chart, allocation and holdings. Portfolios live in
`data/portfolios.yaml`, which you can edit by hand (typos are reported with line numbers):

```yaml
version: 1
portfolios:
  - { id: long-term, name: Long-term, wallets: all, assets: [BTC, SOL], watch: [ETH] }
  - { id: kids, name: "Kids' wallets", wallets: [kids-phantom, kids-cash], assets: all }
  - { id: watch, name: Watchlist, wallets: [], watch: [ETH, AVAX] }
```

`wallets`/`assets` take `all` (the default when omitted) or a list; `[]` means none. Asset keys
are exchange symbols (`BTC`) or `spl:<mint>` for Solana tokens. Save and restore locally from
**Portfolios → Save & restore** (export, import with merge or replace, restore any automatic
backup) or the CLI:

```sh
crypto-portfolio portfolios export -o portfolios.yaml [-id long-term]
crypto-portfolio portfolios import portfolios.yaml [-replace]
```

## Secrets and your data directory

- Exchange secrets are encrypted at rest (AES-256-GCM) in `data/sources.yaml`.
- The key is `VAULT_SECRET_KEY` (see `.env.example`) or, if unset, `data/secret.key`, generated on first run.
  **Back it up**: without it, stored exchange keys can't be decrypted and must be re-entered.
- Encryption at rest protects a leaked `sources.yaml` or backup. It does not protect against someone who can also read `data/secret.key`.
- Every change to a YAML file keeps a timestamped copy in `data/backups/` (newest 30).
- Back up the whole `data/` directory.

```sh
crypto-portfolio sources export -o sources.yaml        # without secrets
crypto-portfolio sources import sources.yaml [-replace]
```

## Remote access, password, phone app

Vault is meant for your LAN or Tailscale. To reach it from anywhere on your tailnet with HTTPS:

```sh
tailscale serve --bg 8787
```

To require a password, set `PORTFOLIO_PASSWORD` (and optionally `PORTFOLIO_USER`) in `.env`.

**Install as an app:** on iOS, Safari → Share → *Add to Home Screen*. On Android, Chrome
needs HTTPS (use the `tailscale serve` URL) → menu → *Install app*.

## Configuration

See `config.example.yaml`. Highlights: `refreshMinutes` (default 5), `staleMaxHours`
(how long last-good data is shown when an API fails, default 24), Bitcoin Esplora URL,
Solana RPC and spam settings.

## Development

```sh
make dev     # templ watch + live reload
make test    # go test -race ./...
make build   # static binary (CGO off)
make linux-arm64
```

The design source of truth is the Vault design system (`docs/design/`). The full plan is in `docs/plan.md`.
