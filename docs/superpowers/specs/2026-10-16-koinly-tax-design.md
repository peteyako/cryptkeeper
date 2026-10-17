# Replace Koinly: opening basis import, per-wallet FIFO lots, Koinly-style tax reports

## Context

The user pays for Koinly to do their US crypto taxes. They want CryptKeep to take over: Koinly did 2025 (exports are in `~/Downloads/koinly_2025_*`), and CryptKeep does 2026 onward from its own activity feeds. That needs two things:

1. **A one-time import** of Koinly's closing position, reconciled against what CryptKeep sees, as the 2026-01-01 opening cost basis.
2. **Koinly-equivalent tax output** from then on: what the user hands to TurboTax and their CPA (capital gains with short/long-term, Form 8949 / Schedule D, income report, year-end holdings, complete tax report).

Today `internal/costbasis` is one global **average-cost** engine (`avgcost.go:74`). That isn't an accepted US method for crypto. It also has no per-disposal records and no holding periods, so it can't produce any of these reports.

## Decisions the user made

- **Replace Koinly**, not mirror it. CryptKeep becomes the tax record from **2026-01-01** (the cutover).
- **US filer, FIFO, tracked per wallet**, matching what Koinly did for 2025 under Rev. Proc. 2024-28.
- **Opening basis = what all the wallets hold at the start of the year**: Koinly's year-end holdings (quantity and cost per asset) as of 2025-12-31 23:59.
- **Acquisition dates are back-filled** from the 2025 transaction history. FIFO keeps the newest units, so the most recent 2025 acquisitions keep their real dates and cost, and the rest becomes one "acquired before 2025" lot, which is long-term for any 2026 sale.
- **FIFO lots replace average cost everywhere**: dashboard P/L column, wallet tiles, Reports.
- **Filing outputs needed:** TurboTax import, Form 8949 / Schedule D, income report, and a complete package for the CPA.
- **Running CryptKeep already syncs every active wallet** with full history since the cutover.
- **Koinly may hold wallets CryptKeep doesn't sync** (for example EVM chains or old addresses). These are treated as dormant: their lots matter, but no new activity happens there.
- **The user stakes SOL natively and uses locked JUP and Jupiter perps.**
  - Native staking rewards are income.
  - Moving tokens into one of their own positions (JUP lock, perps collateral, LP) is **non-taxable**: the lots stay in that wallet with their dates and cost.
- **Unpriced Solana tokens:** add a free historical price source (CoinGecko demo key, by mint), plus a per-transaction price override in the review queue.
- **Years get closed.** Once filed, a year's gains, income and closing lots are frozen. The closing lots become the next year's opening lots, so the Koinly import is just "2025, closed by Koinly".

## What the Koinly exports actually contain (checked against the user's files)

| File | Header line (after a 2-line title preamble) | Use |
| --- | --- | --- |
| `end_of_year_holdings_report` | `Asset,Quantity,Cost (USD),Value (USD),Description`. Asset is `SYM (Name)`. Ends with a `Total` row. Numbers use thousands separators. | Opening quantity and cost per asset; the `Total` row gives the overall cost to reconcile against. |
| `transaction_history` | `Date,Type,Tag,Sending Wallet,Sent Amount,Sent Currency,Sent Cost Basis,Receiving Wallet,Received Amount,Received Currency,Received Cost Basis,Fee Amount,Fee Currency,Gain (USD),Net Value (USD),Fee Value (USD),TxSrc,TxDest,TxHash,Description`. Dates are UTC. | Back-filling dates per wallet, wallet auto-mapping (TxSrc/TxDest addresses), and a parity reference. |
| `capital_gains_report` | `Date Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period`. Local time `MM/DD/YYYY HH:MM`. | The 2025 realized history (read-only) and the **output format to reproduce**. |
| `income_report` / `other_gains_report` | `Date,Asset,Amount,Value (USD),Type,Description,Wallet Name` / `Date,Asset,Amount,Value (USD),Type,Wallet Name` | Empty for 2025. Income is still an output format to reproduce. |
| `complete_tax_report.pdf` | multi-page PDF | The model for the CPA package. |

**Koinly has no export of open lots** (open feature request since 2023), which is why opening dates are back-filled.

**How Koinly treated 2025 activity.** This is the parity target for forward rules:

- Untagged `crypto_deposit`: an acquisition at market value. No gain, not income, and `Received Cost Basis = Net Value`.
- Untagged `crypto_withdrawal`: a **disposal at market value** (rows without a gain were zero-value).
- `Cost` / `Other fee`-tagged withdrawals and transfer fees: disposals at market value. `Fee` appears in the Notes column.
- `exchange`: a disposal of the sent asset plus an acquisition of the received asset.
- `transfer`: moves basis between wallets. Its fee is a disposal.
- `Missing cost basis` (Notes): a disposal with no lots, at cost 0.

## Goals / non-goals / anti-goals

- **Goals:**
  - Exact, reconciled 2026-01-01 opening lots per wallet.
  - Per-wallet FIFO lots with holding periods from then on.
  - Koinly-format CSVs, so the CPA and TurboTax workflow doesn't change.
  - Form 8949 / Schedule D, an income report and a CPA package.
  - A review queue for things a human must decide (Koinly's "needs review").
- **Non-goals:**
  - HIFO, LIFO or Spec-ID: FIFO only, and no method switch is designed in.
  - Wash sales (they don't apply to crypto).
  - Full perps / derivatives accounting ("other gains"). When a position returns more or less than went in, that difference becomes a review issue the user tags.
  - Yield inside LP positions (fees accruing in the position). Only deposits and withdrawals are tracked.
  - Recomputing years before 2026.
  - EVM chains.
  - Entering manual transactions for dormant wallets.
- **Anti-goals:**
  - Numbers that silently disagree with Koinly at the cutover. Every difference must be visible in the reconciliation and accepted by the user.
  - Inventing long-term status. When a date is uncertain, err toward **short-term** and flag it.
  - Committing the user's real Koinly files: test fixtures are synthetic.

## Design

### 1. Tax-lot engine: new package `internal/tax` (replaces `costbasis/avgcost.go`)

- **Years:**
  - The **open year** is the year after the last closed year.
  - The engine only computes the open year. It starts from the closing lots of the last closed year.
  - Closed years are frozen files (§3) and are never recomputed.
- **Input:**
  - Opening lots: the closing lots of the last closed year.
  - Activity txs from the start of the open year (tax timezone), from `activity.Store.All` after `dedupeImported` (`server/reports_handlers.go:43`).
  - Per-tx tags.
  - The set of hidden/spam mints.
  - The tax timezone.
- **Output:**
  - Open lots per `(wallet, asset)`: `{Acquired date | BeforeCutoverYear, Amount, CostUSD, SourceTxID}`.
  - `[]Disposal{Sold, Acquired | "VARIOUS", Asset, Amount, CostUSD, ProceedsUSD, Gain, Wallet, Term (short|long), Note (""|Fee|Missing cost basis), TxID, Box8949}`.
  - `[]Income{Date, Asset, Amount, ValueUSD, Type, Wallet, TxID}`.
  - `[]Issue` for the review queue.
- **Wallet:** wallet = `core.Tx.SourceID` (`core/activity.go:43`). A Coinbase source is one wallet, which is also how Koinly and the IRS treat an exchange account. An xpub source is one wallet even though Koinly listed its addresses separately.
- **Untracked wallet:** a tax-only pseudo-wallet `untracked` holds lots from Koinly's dormant wallets. It has no activity and doesn't appear in dashboard holdings.
- **FIFO:** per wallet, by acquisition date. Ties are broken by source order.
- **Holding period:** long-term only when the sale date (tax timezone) is after the one-year anniversary of the acquisition date. A sale on the exact anniversary is short-term. Lots marked "before 2025" are always long-term for 2026+ sales.
- **Forward rules** (Koinly parity, from the table above):

  | Activity | Treatment |
  | --- | --- |
  | `trade` / `swap` | Each negative leg is a disposal (proceeds = leg USD). Each positive leg is an acquisition (cost = leg USD). Reuse the single-unvalued-leg fill rule (`sideValue` / `fillSide`, `avgcost.go:306-335`). |
  | `transfer` (PeerID set) | Move FIFO lots from the sending to the receiving wallet, **keeping acquisition date and cost**. sent − received is a fee disposal on the sending wallet. Process each pair once. |
  | `receive` (unmatched) | Acquisition at market value, not income. Opens a review item. |
  | `send` (unmatched) | Disposal at market value. Opens a review item. |
  | `reward` | Income at market value, plus an acquisition at the same value. |
  | `Tx.Fee` and kind `fee`, including fees on failed txs | Disposal at market value, Note `Fee`. |
  | `stake` (native SOL delegate/deactivate/withdraw) | No-op (same wallet). |
  | **Position move** (JUP lock/unlock, Jupiter perps collateral in/out, LP add/remove) | Non-taxable. Lots stay in the wallet, parked under the position. Detected by program ID in the Solana classifier (`internal/sources/solana/activity.go`, which today emits these as plain `send`). Anything not detected can be tagged `position` in the review queue. When a withdrawal differs from what went in, the difference becomes an issue that the user tags as a gain or loss. |
  | **Native staking reward** | Income at market value on the epoch's end time, plus an acquisition. This needs a new fetcher: Solana RPC `getInflationReward` per stake account per epoch, emitted as `reward` txs into activity. Nothing fetches these today. |
  | Leg without a USD value | Price lookup order: per-tx override in `tax.yaml`, then `price_history`, then Coinbase candles, then **CoinGecko by Solana mint** (new, free demo key stored as an encrypted secret). If nothing is found: value 0 plus a "missing price" issue. The review queue has a price field that writes the override. |
  | Hidden/spam mint (`solana` `HideMints`) | Ignored. |
  | Disposal larger than the open lots | The excess is at cost 0 with Note `Missing cost basis`, plus an issue. |

- **Tags** (stored per tx ID). Each tag changes one tx's treatment:
  - Receive: `purchase` (the default), `income`, `airdrop` (income), `gift-received`, `own-wallet` (cost 0, plus a prompt to enter basis), `position`, `spam`.
  - Send: `sale` (the default), `gift`, `lost`, `own-wallet` (moves lots to `untracked`), `position`, `spam`.
  - Txs in closed years can't be tagged.
  - Exact tax semantics per tag follow Koinly's tag definitions. The builder checks Koinly's docs.
- **8949 box:** a rule table maps wallet type and year to Form 8949's digital-asset checkboxes (Part I G/H/I, Part II J/K/L). Coinbase and Kraken issue 1099-DA; self-custody wallets are "not reported on 1099-DA". Each wallet can override its box in `tax.yaml`. **The builder verifies the current IRS 8949 instructions** for 2025/2026 covered-basis rules.
- **Dashboard replacement:**
  - Per-asset basis = Σ cost of open lots in tracked wallets.
  - Unrealized = value − basis.
  - Wallet tiles use that wallet's lots.
  - Realized per year = Σ disposals.
  - Coverage = the share of value covered by lots, excluding missing-basis units.
  - The new engine replaces `costResult` / `pnlRows` / `pnlCells` / `walletCost` (`reports_handlers.go:61-135`), behind the same 1-minute `costCache`.
  - `avgcost.go` is deleted.
  - The Coinbase and Kraken CSV importers stay: they feed activity, and rows before the cutover are simply ignored by the engine.
  - **My call:** `costbasis.yaml` manual `lots` are migrated into `tax.yaml` as manual acquisitions with a wallet. `assets` overrides are retired, because opening lots replace them.

### 2. Koinly opening import and reconciliation

The import is a wizard on the Reports page (upload → map → reconcile → accept).

1. **Upload.** The user uploads the year-end holdings CSV and the transaction history CSV, plus optionally the capital gains and income CSVs (for 2025 display).
   - Parsers go in `internal/tax/koinly`.
   - They tolerate the 2-line preamble and thousands separators, and reuse `costbasis/csv.go` helpers (`parseMoney`, `parseTime`, `headerIndex`, `newCSVInput`).
   - The upload uses the existing multipart pattern (`reports_handlers.go:228`, 8 MB limit).
2. **Map wallets.** Each Koinly wallet name maps to a CryptKeep source ID or to `untracked`.
   - The mapping is auto-suggested by matching `TxSrc` / `TxDest` addresses against `sources.Def.Address` / `Key`, and by matching labels.
   - The user confirms it.
3. **Map assets.** Each Koinly `SYM (Name)` maps to a CryptKeep asset key.
   - The match is auto-suggested by symbol and name against the holdings and `token_meta` of mapped wallets. Ambiguous pairs (e.g. two different tokens that both use the symbol `JUP`) are left to the user.
   - Zero-cost, zero-value spam and NFTs are skipped by default.
4. **Opening quantity per wallet (roll-back).** For each mapped wallet and asset: current holding minus the net signed 2026 activity (legs and fees, non-failed).
   - Current holdings include stake accounts and position underlyings, such as locked JUP.
   - Position moves and `stake` count as zero-net inside the wallet.
   - 2026 staking rewards must already be fetched, or SOL comes out overstated.
   - Before using a wallet, the import confirms its activity backfill covers the cutover. If it doesn't, the import says so.
5. **Reconcile, per asset:**
   - Show Koinly quantity and cost, CryptKeep's rolled-back quantity summed across mapped wallets, and the difference.
   - Koinly holds more: the excess goes to `untracked`, which is the dormant wallets.
   - CryptKeep holds more: the extra units become cost-0 lots flagged "missing basis".
     - **Expect SOL here.** Koinly's 2025 income report is empty, so 2025 staking rewards were never recorded.
     - For those units, the reconcile screen lets the user enter a basis (for example, fair value as income), with cost 0 as the default.
   - Every difference beyond dust is listed for the user to accept.
6. **Back-fill dates, per wallet and asset.**
   - Walk that wallet's 2025 acquisition rows, newest first: deposits, the received side of exchanges, and transfer-ins.
   - Each row covers `min(remaining, amount)` at the row's unit cost (`Received Cost Basis / Received Amount`).
   - A transfer-in row is dated at the transfer and flagged, which is conservative. Such rows are rare.
   - The leftover becomes a `before-2025` lot.
   - **Cost allocation:**
     - Dated lots keep their exact Koinly cost.
     - The asset's remaining cost (Koinly total − Σ dated cost) is spread over all `before-2025` lots pro-rata by quantity, including `untracked`.
     - A negative remainder is clamped to 0 and flagged.
   - **Invariant:** Σ opening cost per asset == Koinly year-end cost, and Σ quantity == Koinly quantity, after accepted adjustments.
7. **Accept.** The import writes **closed year 2025** (`data/tax/2025.yaml`):
   - closing lots = the reconciled opening lots
   - disposals and income = Koinly's 2025 capital gains and income rows, kept verbatim
   - summary and `source: koinly`
   - The mappings go to `data/tax.yaml`.
   - 2025 shows in the year selector as "2025 (Koinly)".
   - Re-importing replaces it, but only while 2026 is still open.

### 3. Storage, closing a year, backup

- **`data/tax.yaml` (live settings):**
  - Versioned, strict decode, atomic writes with backups. It follows `costbasis/manual.go`'s `ManualStore` pattern (mtime reload, `yamlfile.WriteAtomic`).
  - Contents: `timezone`, `walletMap`, `assetMap`, `manualLots[]`, `tags{txID: {tag, note}}`, `prices{txID/leg: usd}`, `walletBox` overrides.
- **`data/tax/<year>.yaml` (closed years, frozen):**
  - Contents: `closedAt`, `source` (`koinly` | `cryptkeep`), `summary`, `disposals[]`, `income[]`, `closingLots[]`.
  - "Close <year>" on the Tax view writes this file from the engine's result, after a confirmation that lists any open review issues.
  - Only the latest closed year can be reopened, which deletes its file.
- **Backup:** both go into `backup.File` as new fields (`Tax`, `TaxYears`) with merge rules (`internal/backup/backup.go`).
  - A restore with no activity database still gives every closed year plus the opening lots of the open year. Rebuilds never need activity from before the open year.
- **Sync horizon (my call):** activity backfill reaches back to at least the start of the open year: `Since = min(now − backfillDays, open-year start)` (`internal/activity/syncer.go`, `main.go`).
  - Without this, a rebuild in early 2027 couldn't reach 2026-01-01.
- **Tax timezone (my call):** it comes from the `tax.timezone` config entry, defaulting to the process's local zone. It sets the tax-year boundaries and 8949 dates; Koinly used local time.

### 4. Reports (Koinly parity)

The in-app Tax view lives on Reports, with a year selector (`2025 (Koinly)`, `2026`, …):

- Summary tiles: short-term and long-term proceeds, cost and gain; total income; fees.
- Tabs: **Capital gains** (per-disposal rows), **Income**, **Year-end holdings** (open lots per wallet with unrealized gain), **Needs review** (issues plus tag controls).

Downloads, written with `internal/export` (`writer` + `csvHeaders`, as `exportActivity` does):

- Koinly-format CSVs with identical headers:
  - capital gains
  - income
  - end-of-year holdings
  - transaction history
- **TurboTax:** the TurboTax Online crypto CSV, plus TXF for TurboTax Desktop. **My call: both.** The builder verifies the current formats.
- **Form 8949 + Schedule D, printable:**
  - An HTML page with print CSS.
  - Part I / Part II grouped by box, with per-box totals feeding the Schedule D lines.
  - Lots marked "before 2025" print "VARIOUS" as the acquired date.
  - **My call:** print-to-PDF from the browser, not a Go PDF library.
- **Complete tax report (CPA package):** one printable page modeled on Koinly's PDF. It contains:
  - summary
  - method statement (FIFO per wallet; how the opening basis was built from Koinly)
  - Schedule D totals
  - 8949 detail
  - income detail
  - year-end holdings
  - wallet list
  - open review issues
  - the "not tax advice" disclaimer

The UI follows the Vault design system (`docs/design/`) with existing components. The layout details are the builder's.

## Milestones

1. **Engine:** `internal/tax` FIFO per wallet, with the forward rules, tags, price overrides and issues. Pure code, written test-first with synthetic fixtures. Then the dashboard, wallet tiles and Reports P&L move to it, and `avgcost.go` is deleted.
2. **Solana inputs:**
   - Staking-reward fetcher (`getInflationReward`), emitting `reward` txs.
   - Position-move classification for JUP lock and Jupiter perps (program IDs).
   - CoinGecko-by-mint historical prices, wired into the activity `Valuer` (`internal/activity/syncer.go:34`).
3. **Koinly import:**
   - Parsers, wallet/asset mapping, roll-back, reconciliation, back-fill.
   - Accept writes closed year 2025 and `tax.yaml`.
   - Backup fields; sync horizon; migration of `costbasis.yaml` lots.
4. **Review queue and closing:** the Needs-review tab with tag and price controls (each change invalidates `costCache`); close/reopen a year.
5. **Reports:** Tax view, Koinly-format CSVs, TurboTax CSV and TXF, printable 8949 / Schedule D, CPA package.
6. **Wrap-up:**
   - Demo data (`internal/demo/reports.go`).
   - A new section in `docs/plan.md` (the M5 avg-cost text is superseded).
   - README.


## Critical files

- **New:**
  - `internal/tax/` (engine, store, rules, issues)
  - `internal/tax/koinly/` (parsers, back-fill, reconcile)
  - `web/templates/tax*.templ`
  - `internal/server/tax_handlers.go`
- **Changed:**
  - `internal/server/reports_handlers.go`
  - `internal/server/server.go` (routes)
  - `internal/server/handlers.go` (wallet tiles)
  - `web/templates/reports.templ`, `reports_view.go`
  - `internal/backup/backup.go`
  - `internal/config/config.go` (tax timezone)
  - `internal/export/export.go`
  - `main.go` (wiring, sync horizon)
  - `internal/activity/syncer.go` (Valuer: SPL pricing)
  - `internal/prices/` (CoinGecko by mint)
  - `internal/sources/solana/activity.go` (position moves, staking rewards)
- **Removed:**
  - `internal/costbasis/avgcost.go` (and its test)
  - the manual `assets` override UI (`CostBasisPanel`)
- **Reused:**
  - `activity.MatchTransfers` (`internal/activity/transfers.go:51`)
  - `activity.Store.All`
  - `prices.Service.Historical` (`internal/prices/history.go:36`)
  - `store.AddMigration` (`internal/store/store.go:103`)
  - `yamlfile.WriteAtomic`
  - `costbasis/csv.go` helpers
  - `export.writer`

## Verification

- **Unit tests** (`task test`). These cover:
  - FIFO order per wallet.
  - A transfer carrying lots with dates.
  - A fee on a transfer.
  - The holding-period boundary (the exact anniversary is short-term; the next day is long-term).
  - Missing cost basis.
  - The unvalued-leg fill rule.
  - Tags changing treatment.
  - Back-fill (newest-first, remainder, negative-remainder clamp, transfer-in flag).
  - The reconciliation invariants.
  - Position moves leave the lots unchanged, and an unequal return raises an issue.
  - A staking reward becomes income plus a lot.
  - The price-override order.
  - Close-year round trip: the closing lots of year N equal the opening of N+1, and a closed year is unchanged by later tags.
  - A backup restore without the activity DB keeps every closed year.
  - Each Koinly parser, against synthetic CSVs in the exact header layouts above (preamble, thousands separators, `Total` row).
- **Real-data check, local only, nothing committed:**
  - Import the user's `~/Downloads/koinly_2025_*` files.
  - Opening Σ cost must equal the year-end report's `Total` cost, minus only skipped spam rows whose cost is 0.
  - Per-asset quantities must match the year-end report, with every difference shown in reconciliation.
  - Spot-check the lot splits of two assets by hand.
- **Forward check:** pick 3 real 2026 disposals (a swap, a fee and a transfer) and verify cost, proceeds and term by hand against the lots.
- **Output check:**
  - The Koinly-format capital gains CSV opens with byte-identical headers to Koinly's.
  - The user test-imports the TurboTax CSV into TurboTax.
  - The 8949 print view's box totals equal the sum of its rows.
- **App check:** run `templ generate` and `task dev` (air), then click through the import wizard → reconciliation → accept → Tax view → downloads. Run `go vet` and the existing test suite.

## Calls I made (please check)

- **Untracked wallet:** Koinly's dormant wallets fold into one tax-only `untracked` wallet. It isn't shown in dashboard holdings, and selling from it is out of scope.
- **Reconciliation direction:** where Koinly and CryptKeep disagree on quantity, CryptKeep's on-chain quantity is the truth for synced wallets. Koinly's excess goes to `untracked`; CryptKeep's excess becomes cost-0 lots.
- **Transfer-ins in the imported year:** these are dated at the transfer during back-fill, which means short-term if uncertain, and they are flagged.
- **Manual basis entries:** `costbasis.yaml` manual lots become wallet-scoped manual acquisitions. Asset-level overrides are retired.
- **Printable reports:** 8949, Schedule D and the CPA package are print-to-PDF HTML pages, not generated PDFs.
- **TurboTax:** both the TurboTax Online CSV and TXF for Desktop.
- **Settings:** the tax timezone defaults to local time, and activity sync always reaches back to the start of the open year.
