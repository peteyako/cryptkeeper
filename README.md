# CryptKeep

A self-hosted, **read-only** crypto portfolio monitor. One page for everything you hold:
Coinbase, Kraken, Bitcoin cold storage (xpub, descriptors, multisig) and Solana wallets
(SOL, every SPL token, staking), with live updates, stale-but-visible data when an API
hiccups, and a design that puts numbers first.

- Single Go binary, no database server: state lives in `data/` (YAML you can edit plus a SQLite file).
- Server-rendered with [templ](https://templ.guide) and [Datastar](https://data-star.dev): live updates over SSE, almost no JavaScript.
- Read-only by design. CryptKeep only ever holds **public** keys, addresses and **view-only** API keys. It cannot move funds.

> Never paste a seed phrase or private key into CryptKeep. It will refuse obvious mistakes, but the rule is yours to keep.

## Quick start

```sh
cp config.example.yaml config.yaml     # optional: defaults work
task run                               # or: go run . serve   (go-task: https://taskfile.dev)
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
2. Choose the **ECDSA** signature algorithm (Coinbase's v2 accounts API, which CryptKeep uses for vaults and staked balances, documents ECDSA keys only).
3. Permissions: **View** only. No trade, no transfer.
4. Paste the key name (`organizations/…/apiKeys/…`) and the private key (PEM).

### Kraken
1. Settings → API → Create API key.
2. Permissions: **Query Funds** and **Query Ledger Entries** only.
3. Paste the API key and private key.

### Bitcoin
Paste any of:
- An extended public key: **xpub** (legacy), **ypub** (nested SegWit), **zpub** (native SegWit).
  An xpub from a SegWit or Taproot account (Ledger shows an xpub for every account) needs its
  *Script type* set: Native SegWit (`bc1q…`, path 84'), Nested SegWit (`3…`, 49'), Taproot
  (`bc1p…`, 86'). Left on auto-detect, an xpub is read as Legacy (`1…`) and usually shows 0 BTC.
- An **output descriptor**, including multisig, e.g. `wsh(sortedmulti(2,[fp/48h/0h/0h/2h]xpub…/<0;1>/*,…))#checksum`.
- A single **address**.
- Or upload a **wallet file**: a Caravan/Unchained wallet config (JSON) or a Sparrow descriptor export.

Where to find them:
- **Sparrow**: Settings → Script Policy shows the descriptor; *Export* → *Output Descriptor*.
- **Ledger Wallet** (formerly Ledger Live): Account → wrench icon (Edit account) → Advanced. Paste
  the whole JSON shown there: CryptKeep takes the `xpub` and the script type from it. Pasting
  just the `xpub` works too; then set the script type (`freshAddressPath` `84'/…` Native SegWit,
  `49'/…` Nested SegWit, `86'/…` Taproot, `44'/…` Legacy). An xpub left on auto-detect whose
  Legacy addresses have no history gets a hint naming the script type that does.
- **Trezor Suite**: Account → Details → Show public key.
- **Unchained / Caravan**: download the wallet configuration JSON.
- **Nunchuk**: Wallet → ⋯ → Export wallet → descriptor / BSMS file.

CryptKeep derives receive and change addresses with a gap limit of 20 (configurable).

**Privacy:** balances are looked up on `mempool.space` by default, which lets that server see
your addresses together. Point `bitcoin.esploraUrl` at your own mempool/electrs node to avoid it.

Public Esplora servers allow a few hundred requests an hour per IP (blockstream.info: 700). So
CryptKeep reads every address of a wallet every 6 hours, and in between only the addresses that
can change: those holding a balance and the next 5 unused ones on each chain. A payment to an
old, emptied address shows at the next full scan. When there's no new block, a refresh reads
only the block height. Requests to each server are paced together across all wallets: a
"429 Too Many Requests" slows the pace and the request is tried again; a server that keeps
answering 429 is left alone for at least 5 minutes. A scan through a slow server runs on in the
background until it's done, and is kept across restarts.

While mempool.space isn't answering, CryptKeep asks `blockstream.info` instead (and the other way
round), and the wallet says so. List your own fallbacks, asked in order, under
`bitcoin.esploraFallbackUrls`; a server that stops answering is asked last for 10 minutes. A
private node gets no fallback unless you list one; `esploraFallbackUrl: "off"` turns fallbacks off.

```yaml
bitcoin:
  esploraUrl: https://mempool.emzy.de/api   # a community mempool instance
  esploraFallbackUrls:
    - https://blockstream.info/api
    - https://mempool.space/api
```

**Blockstream API key.** For far higher limits, create a key at
[dashboard.blockstream.info](https://dashboard.blockstream.info) → Manage Keys and enter its client
ID and secret under **Settings → Bitcoin API key** (stored encrypted; CryptKeep checks it with
Blockstream before saving), or set `BLOCKSTREAM_CLIENT_ID` and `BLOCKSTREAM_CLIENT_SECRET`. Wallets
on mempool.space or blockstream.info then go through `enterprise.blockstream.info`, with the public
servers standing by if it fails. A wallet on your own node never uses the key.

### Solana
Paste the wallet's public address. CryptKeep reads SOL, every SPL token (classic and Token-2022),
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

**DeFi:** Orca and Raydium concentrated-liquidity positions, Raydium LP tokens, locked/staked
JUP and Jupiter perps are read automatically (`solana.defi` in config) and shown as their
underlying tokens, grouped by position. Uncollected LP fees aren't included.

**Jupiter limit orders:** Jupiter keeps the funds of open limit orders in a vault account of
your wallet's own, which nothing on chain ties back to your address (Jupiter resolves it with a
signed message, which a watch-only app can't give). Paste the vault's address into the source's
"Jupiter limit-order vault" field: its SOL and tokens then show as a "Limit orders" position, and
Activity treats the vault as part of the wallet, so a deposit is a move, a fill is a swap and a
withdrawal a move. The vault is the owner of the token account your first deposit went to.

### Manual holdings
Coins held anywhere CryptKeep can't read, one per line: `BTC 0.25 owed by Sam`. Use
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

`wallets`/`assets` take `all` (the default when omitted) or a list; `[]` means none. **Export
all** also includes your alert rules (an `alerts:` section), and importing such a file restores
them too — one file backs up both. Asset keys
are exchange symbols (`BTC`) or `spl:<mint>` for Solana tokens. Save and restore locally from
**Portfolios → Save & restore** (export, import with merge or replace, restore any automatic
backup) or the CLI:

```sh
crypto-portfolio portfolios export -o portfolios.yaml [-id long-term]
crypto-portfolio portfolios import portfolios.yaml [-replace]
```

## Activity

Every source's transactions in one feed (**Activity**, and each wallet's Activity tab):
Bitcoin from Esplora, Solana from your RPC, Coinbase and Kraken from their APIs. New activity
syncs every `activityMinutes`; older history is backfilled gradually (down to
`activity.backfillDays`) so free APIs aren't hammered. A withdrawal from an exchange that lands
in one of your own wallets is shown once, as a **Transfer · Coinbase → Cold storage**.

## Taxes and cost basis

**Reports** is a Koinly replacement for US crypto taxes. It tracks cost basis with **FIFO per
wallet**: each wallet keeps its own tax lots, and moving coins between your own wallets carries
the lots (and their acquisition dates) along instead of selling them. From that it builds
capital gains, income (rewards, staking, airdrops), year-end holdings and a **Needs review**
list of anything it can't decide on its own. Inputs are your synced exchange and on-chain
activity (including Solana position moves and staking rewards), plus:

- **Manual lots** for history the APIs don't reach (in `data/tax.yaml`).
- **CSV import** of the Coinbase *Transaction history* report and the Kraken *Ledgers* export
  (Reports → CSV import). Re-importing the same file is safe. Rows the live API also synced are
  counted once. When you have exactly one Coinbase (or Kraken) source, the imported rows count
  as that wallet's history, so older buys fund its sales; otherwise they stay in a separate CSV
  wallet.

Tokens you hide in Settings are ignored for tax. Tokens the dashboard hides on its own
(unverified, unpriced or dust) still count in full; only an airdrop of one that arrived worth
nothing is left off the Needs review list.

Unrealized P/L per asset, realized P/L by year, and the holdings / history / P/L / activity CSV
exports stay on the Reports page.

### Switching over from Koinly

**Reports → Import from Koinly** moves a Koinly tax history into CryptKeep so it can take over:

1. In Koinly, export these reports for the year before you switch (CSV): **End of year holdings
   report**, **Transaction history**, **Capital gains report** and **Income report**. The wizard
   also recognises **Other gains report**.
2. In CryptKeep go to Reports → Import from Koinly and upload them.
3. Map each Koinly wallet to a CryptKeep source (or `untracked`), and each Koinly asset to an
   asset key (or skip it, e.g. spam).
4. Reconcile: CryptKeep compares Koinly's year-end quantities and cost with your wallets and lists
   the problems and warnings. Review them and fix what you can (map assets, add a basis for
   coins Koinly didn't know about). Accept is blocked only when the holdings report is missing,
   the year hasn't ended or a later year is already closed, an ambiguous asset is still on Skip
   without "Skip anyway", or nothing would carry over; other problems are listed but don't stop
   you.
5. Accept. The year is closed and CryptKeep carries its lots forward.

### What is stored

Your choices live in `data/tax.yaml`: wallet and asset maps, manual lots, tags, prices, and
per-wallet Form 8949 box overrides (`walletBox`, see below). Each closed year is frozen in `data/tax/<year>.yaml`. Both
are included in the Settings backup, so a restore brings back every closed year. Manual lots
from an older `costbasis.yaml` are copied into `tax.yaml` once, in the `untracked` wallet; edit
`tax.yaml` to assign wallets.

### Needs review

The **Tax** view (Reports → Tax, pick a year) has tabs for Capital gains, Income, Year-end
holdings and **Needs review**. Needs review appears only while the year is open. It lists what a human must decide, with controls to:

- **Tag** a transaction (e.g. purchase, sale, income, airdrop, gift received, gift, lost, own wallet, position, position gain, spam), singly or in bulk. An *own wallet* receive takes back coins held as `untracked` first, with their dates and cost. A transfer whose other half never arrived can be tagged like a plain send or receive.
- Set a **price** (USD) where none could be found.
- Add a **write-off** for a worthless or lost position.

Every change recomputes the reports.

### Closing a year

Close a year when its Needs review list is clear (or you accept what is left). A year with a
pending transaction can't be closed until it confirms or fails. A closed year is frozen in
`data/tax/<year>.yaml` with the tax timezone it was closed in; the next year starts at midnight
on Jan 1 in that zone, so changing the tax timezone later never counts a transaction twice or
drops one (the Tax view says so when the zones differ). Later years start from its lots. Only
the latest closed year can be **reopened**, which deletes its file; the next close writes a
fresh one.

### Form 8949 boxes

Rows go in the digital-asset boxes: **G/H/I** for short-term (Part I), **J/K/L** for long-term
(Part II). A Coinbase or Kraken wallet gets G (J) for a lot it bought itself on or after
Jan 1, 2026, else H (K); every other wallet gets I (L). For a Koinly-imported year the wallet is
a Koinly name, so only names containing "coinbase" or "kraken" count as exchanges.

To override a wallet, set its short-term letter in `tax.yaml`; long-term rows follow (G→J, H→K,
I→L). Keys are source IDs, or Koinly wallet names for a Koinly year. Only G, H and I are
accepted (any case): anything else makes `tax.yaml` fail to load with the wallet named.

```yaml
walletBox:
  cold: I
  Coinbase Pro: H   # a Koinly wallet name
```

### Downloads

Each year's Tax view offers:

- **Koinly-format CSVs:** capital gains, income, year-end holdings, transaction history and
  other gains. The transaction history isn't available for Koinly-imported years (use Koinly's
  own export).
- **TurboTax:** CSV and TXF. TXF has no digital-asset codes, so its rows map to the securities
  boxes A–F (G→A, H→B, I→C, J→D, K→E, L→F): check them in TurboTax Desktop, or prefer the CSV.
- **Form 8949 and Schedule D**, as a printable page (print to PDF).
- **Complete tax report** for your CPA, as a printable page.
- **Filled IRS PDFs:** official Form 8949 and Schedule D. Name and SSN are left blank for you to
  fill in. Newer form revisions are fetched from the IRS and cached in `data/irs/`.
- **Everything (zip)** with all of the above. If the IRS PDFs can't be filled, the zip comes
  without them and a `NOTE.txt` says why.

Not tax advice. Check the numbers, and the Form 8949 boxes, with a tax professional before filing.

## Alerts

Notifications go to [ntfy](https://ntfy.sh) (the app or self-hosted). Pick a long random topic
and add it to `config.yaml`:

```yaml
ntfy:
  url: https://ntfy.sh/cryptkeep-<long-random-string>
  redactAmounts: true   # notifications say "BTC balance dropped 12%", never amounts
```

Then **Alerts → New alert**: a cold-wallet balance drop (the security alert — never fires on
stale data), price above/below, portfolio moves, a source down, or a large send from a
self-custody wallet. **Send test notification** checks the setup. Rules live in
`data/alerts.yaml`.

## Secrets and your data directory

- Exchange secrets are encrypted at rest (AES-256-GCM) in `data/sources.yaml`.
- The key is `CRYPTKEEP_SECRET_KEY` (see `.env.example`) or, if unset, `data/secret.key`, generated on first run.
  **Back it up**: without it, stored exchange keys can't be decrypted and must be re-entered.
- Encryption at rest protects a leaked `sources.yaml` or backup. It does not protect against someone who can also read `data/secret.key`.
- Every change to a YAML file keeps a timestamped copy in `data/backups/` (newest 30).
- Back up the whole `data/` directory.

### Backup & restore

**Settings → Backup & restore** exports everything you set up in one YAML file — sources,
portfolios, alert rules, manual cost basis, tax settings and closed tax years — and restores it (merge, or replace the
sections the file contains). Exchange secrets are left out unless you choose "with encrypted
secrets"; those stay AES-256-GCM sealed and only restore into a CryptKeep with the same key
(`CRYPTKEEP_SECRET_KEY` or `data/secret.key`). Balances and activity aren't included: CryptKeep fetches
them again. Restore also accepts an older sources or portfolios export.

**Encrypted exports.** When `CRYPTKEEP_SECRET_KEY` is set, every YAML export (the Settings
backup, the portfolios export and the `backup`, `sources` and `portfolios export` commands) is
encrypted as a whole with that key, exchange secrets included. The file shows only a short
header (`cryptkeep: encrypted`), and import decrypts it with the same key; a different key is
refused. Keep a copy of the key somewhere other than the server. Without the variable (only
the generated `data/secret.key`, which would be lost with the disk) exports stay plain YAML.
CSV exports are never encrypted.

```sh
crypto-portfolio backup export -o cryptkeep-backup.yaml [-secrets]
crypto-portfolio backup import cryptkeep-backup.yaml [-replace]
```

An exchange source restored without its secrets shows "needs its API key" until you edit it.

## Remote access, password, phone app

CryptKeep is meant for your LAN or Tailscale. To reach it from anywhere on your tailnet with HTTPS:

```sh
tailscale serve --bg 8787
```

To require a password, set `PORTFOLIO_PASSWORD` (and optionally `PORTFOLIO_USER`) in `.env`.

**Install as an app:** on iOS, Safari → Share → *Add to Home Screen*. On Android, Chrome
needs HTTPS (use the `tailscale serve` URL) → menu → *Install app*.

## Configuration

See `config.example.yaml`. Token logos (instead of ticker monograms) can be switched on in **Settings**; they load from
the token issuers' servers. Dates and times are shown in the server's time zone; in Docker set
`TZ` (e.g. `TZ=America/Los_Angeles`). Highlights: `refreshMinutes` (default 5), `staleMaxHours`
(how long last-good data is shown when an API fails, default 24), Bitcoin Esplora URL,
Solana RPC and spam settings.
A free CoinGecko demo key (`COINGECKO_API_KEY`, or the **Settings → Historical prices** card, stored encrypted) prices Solana tokens Coinbase doesn't list, for the last 365 days.
For taxes, set `tax.timezone` (an IANA name such as `America/New_York`; empty means the server's local time) to decide where a tax day starts and ends. The CoinGecko key also prices Solana tokens by mint for tax reports; the environment variable wins over the one saved in Settings.

## Development

```sh
task --list       # everything available
task dev          # air: rebuild + restart on change; :8788 also reloads the browser
task preview      # every screen with demo data on :8790
task test         # go test -race ./...
task check        # generate (no drift) + vet + test
task build        # static binary (CGO off)
task linux-arm64
```

The design source of truth is the Vault design system (`docs/design/`). The full plan is in `docs/plan.md`.
