# Tax M5: Koinly-style tax reports — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce everything the user hands to TurboTax and their CPA:
- an in-app Tax view per year
- Koinly-format CSVs
- TurboTax Online CSV and TXF
- a printable Form 8949 with Schedule D totals
- a printable complete tax report

**Architecture:**
- A report model package `internal/tax/report` turns a year's data into rows: a closed `tax.Year`, or the open year computed with `From`/`To`.
  - Its inputs are an asset-symbol resolver, wallet labels and types, and year-end prices.
- Pure writers produce the CSV, TXF and 8949 groupings.
- Server handlers under `/reports/tax/{year}/…` serve CSVs (`csvHeaders`) and print pages (a templ page with print CSS).
- The Tax view replaces nothing. It is a new section on Reports, with a year selector, and hosts M4's `ReviewPanel`.

**Tech Stack:** Go 1.26, `encoding/csv`, templ.

**Spec:** `docs/superpowers/specs/2026-10-16-koinly-tax-design.md` §4.

## Global Constraints

- **Koinly CSV headers must match byte for byte**, preceded by a title line and a blank line, as Koinly does:
  - capital gains:
    - Title `Capital gains report <Y>`.
    - Header `Date Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period`.
    - Dates `MM/DD/YYYY HH:MM` in the tax timezone. "VARIOUS" is written for Various lots.
    - Amount has 8 decimals; money has 2.
    - Holding period is `Short term` / `Long term`.
  - income:
    - Title `Income report <Y>`.
    - Header `Date,Asset,Amount,Value (USD),Type,Description,Wallet Name`.
    - Type mapping: `reward → Reward`, `income → Income`, `airdrop → Airdrop`, `position → Other income`.
  - end-of-year holdings:
    - Title `Balances as at 12/31/<Y> 23:59`.
    - Header `Asset,Quantity,Cost (USD),Value (USD),Description`.
    - Asset is `SYM (Name)`. Description is `@ $<price> per SYM`.
    - The last row is `Total,-,<cost>,<value>,-`.
  - transaction history:
    - Title `Transaction report <Y>`.
    - Header `Date,Type,Tag,Sending Wallet,Sent Amount,Sent Currency,Sent Cost Basis,Receiving Wallet,Received Amount,Received Currency,Received Cost Basis,Fee Amount,Fee Currency,Gain (USD),Net Value (USD),Fee Value (USD),TxSrc,TxDest,TxHash,Description`.
    - Dates are `YYYY-MM-DD HH:MM:SS UTC`.
    - Types: `trade/swap → exchange`, `receive/reward → crypto_deposit`, `send → crypto_withdrawal`, `transfer → transfer`. Tags: `reward → Reward`, `income → Income`, `airdrop → Airdrop`, `gift → Gift`, `lost → Lost`.
  - Closed Koinly years are written from their stored rows verbatim (Asset and Wallet already in Koinly terms).
- **TurboTax Online CSV:**
  - Header exactly `Currency Name,Purchase Date,Cost Basis,Date Sold,Proceeds`.
  - Dates `MM/DD/YYYY`; `VARIOUS` for Various lots.
  - Money has 2 decimals, without a currency symbol.
  - One row per disposal row.
  - Gifts/lost removals are excluded.
- **TXF** (v042):
  - The header is `V042`, `ACryptKeep`, `D<MM/DD/YYYY today>`, `^`.
  - Each record is `TD`, `N<code>`, `C1`, `L1`, `P<amount> <symbol>`, `D<acquired or VARIOUS>`, `D<sold>`, `$<cost>`, `$<proceeds>`, `^`.
  - Codes: short `712` / long `714` when the box is I/L (no 1099-DA); short `711` / long `713` when the box is H/K; short `321` / long `323` when the box is G/J.
- **8949 boxes:** Part I (short) is G/H/I; Part II (long) is J/K/L. A wallet's short box comes from:
  1. `tax.yaml` `walletBox[wallet]` (a short letter G/H/I; long = G→J, H→K, I→L).
  2. Else, for a `coinbase` or `kraken` wallet (or a Koinly wallet name containing "Coinbase" or "Kraken"): `G` when the disposed lot was acquired on or after 2026-01-01 in the same wallet, which needs `Disposal.LotTxID` to belong to that wallet; else `H`.
  3. Else `I`.
- **Schedule D lines:** G → 1b, H → 2, I → 3, J → 8b, K → 9, L → 10. Columns are (d) proceeds, (e) cost, (g) adjustments = 0, (h) gain.
- **8949 column (a)** description: `<amount, up to 8 decimals, trailing zeros trimmed> <SYMBOL>`.
- The print pages use their own minimal templ layout (no app shell) with `@media print` CSS, plus a "Print / Save as PDF" button (hidden in print). The CPA report ends with: "Prepared by CryptKeep from the user's own records. Not tax advice."
- Do NOT `git add`, `git commit`, `git stash` or switch branches.
- Gate: `task generate && task vet && task test`.

## Review Focus

1. **Open-year report for the current year.** Year-end holdings value uses current prices, labelled "as of <today>" instead of 12/31. Test: Task 2 `TestHoldingsOpenYearUsesToday`.
2. **An asset key with no known symbol** (`spl:<mint>`). It is shown as the first 4 and last 4 characters of the mint (`AbCd…WxYz`) in every report, never as the raw key. Test: Task 1 `TestSymbolFallback`.
3. **A CSV field containing a comma or quote** (wallet label "Cold, Ledger"). It must be quoted by `encoding/csv`, not hand-joined. Test: Task 3 `TestCSVQuotesLabels`.
4. **A year with no disposals.** Every report renders with headers and zero totals, never an error. Test: Task 4 `TestEmpty8949`.
5. **A Koinly-closed year (2025) in TurboTax/8949 exports.** Boxes come from the Koinly wallet names. Test: Task 4 `TestKoinlyYearBoxes`.

---

### Task 1: Report model (`internal/tax/report`)

**Files:**
- Modify: `internal/tax/types.go` (`Disposal.LotTxID string yaml:"lotTxId,omitempty"`), `internal/tax/engine.go` and `transfer.go` (carry the consumed lot's `TxID` into each disposal row; a transfer keeps the lot's original `TxID`)
- Create: `internal/tax/report/report.go`
- Test: `internal/tax/report/report_test.go`, `internal/tax/engine_test.go` (`TestDisposalCarriesLotTxID`)

**Interfaces:**
- Produces:

```go
type Resolver struct {
	Symbol      func(assetKey string) (symbol, name string) // falls back to short mint form for spl: keys
	WalletLabel func(walletID string) string                // tax.Untracked → "Untracked (from Koinly)"
	WalletType  func(walletID string) string                // "coinbase" | "kraken" | "bitcoin" | "solana" | "manual" | ""
	Box         map[string]string                           // tax.yaml WalletBox
	Price       func(assetKey string, day time.Time) (float64, bool) // year-end price
}
type Row struct { tax.Disposal; Symbol, WalletLabel, Box string }     // Box is one of G..L
type HoldingRow struct { AssetKey, Symbol, Name string; Quantity, CostUSD, ValueUSD, Price float64; Priced bool }
type YearReport struct {
	Year int; Source string; Closed bool; AsOf time.Time; Loc *time.Location
	Rows []Row; Income []tax.Income; GiftsLost []Row; Holdings []HoldingRow
	Summary tax.Summary; Boxes map[string]BoxTotal; Issues []tax.Issue
}
type BoxTotal struct { Proceeds, Cost, Gain float64; Count int }
func FromClosed(y tax.Year, r Resolver, loc *time.Location) YearReport
func FromOpen(year int, res tax.Result, r Resolver, loc *time.Location, now time.Time) YearReport // filters disposals and income to `year`; AsOf = min(Dec 31 23:59, now)
func BoxFor(d tax.Disposal, walletType string, lotWallet string, override string, koinly bool) string
```

- [ ] **Step 1: Write failing tests**

```go
func TestDisposalCarriesLotTxID(t *testing.T) // buy tx "b" in wallet cb, sell → disposal LotTxID == "cb:b"; after a transfer to wallet x and a sale there → LotTxID still "cb:b"
func TestBoxRules(t *testing.T)    // coinbase wallet, lot acquired 2026-02-01 in same wallet, ShortTerm → "G"; acquired 2025-06 → "H"; solana wallet → "I"; long-term solana → "L"; override "H" on solana → short "H" / long "K"; Koinly wallet "Kraken (API keys)" → "H"
func TestSymbolFallback(t *testing.T) // asset "spl:AbCdEfGh123WxYz" with no symbol → Row.Symbol "AbCd…WxYz"
func TestFromClosedTotals(t *testing.T) // closed year with 2 short rows (gain 10, −4) and 1 long (7) → Boxes["I"]{Gain 6, Count 2}, Boxes["L"]{Gain 7}
func TestHoldingsFromClosingLots(t *testing.T) // closing lots SOL 2 @200 + 1 @50 across wallets → one HoldingRow SOL qty 3 cost 250 value 3×price
```

- [ ] **Step 2–4:** `go test ./internal/tax/... -run 'TestDisposalCarriesLotTxID|TestBox|TestSymbol|TestFromClosed|TestHoldingsFrom' -v` → FAIL; implement; PASS.

---

### Task 2: Tax view on Reports (year selector, tiles, tabs)

**Files:**
- Create: `internal/server/taxview.go` (`func (s *Server) yearReport(ctx, year int) (report.YearReport, error)`; `func (s *Server) taxYears() []int` (closed years plus every open year up to the current one); the resolver built from snapshot rows, activity legs, `token_meta` and `Registry.Defs()`; year-end prices via a new `Deps.Historical` interface)
- Modify:
  - `internal/server/server.go` (`Deps.Historical interface { Historical(ctx, symbol string, day time.Time) (float64, bool, error); HistoricalMint(ctx, mint string, day time.Time) (float64, bool, error) }`; routes `GET /reports/tax` (`?year=`) and `POST /reports/tax/year` (SSE switch))
  - `main.go` (pass `a.prices`)
  - `web/templates/tax.templ` (new: `TaxSection(p TaxPage)`, id `tax-section`)
  - `web/templates/reports.templ` (include `@TaxSection` above the P&L table)
  - `web/templates/models.go`
- Test: `internal/server/taxview_test.go`

**Interfaces:**
- `templates.TaxPage{Years []int; Year int; Report report.YearReport (or a view copy); Tab string ("gains"|"income"|"holdings"|"review"); Review ReviewPage}`.
- **Tiles:** Short-term gain, Long-term gain, Income, Fees, plus proceeds/cost under each gain.
- **Tabs:**
  - Capital gains: date sold, date acquired, asset, amount, cost, proceeds, gain, term, wallet, box. 200 rows per page; `?page=` is a SSE patch.
  - Income.
  - Year-end holdings.
  - Needs review: M4's `ReviewPanel`, shown only for the open year.
- **Downloads row:** links to Task 3/4/5 URLs.
- **Close/Reopen buttons:** M4.

- [ ] **Step 1: Write failing tests**

```go
func TestTaxViewYears(t *testing.T)           // closed 2025 + now 2026-10-16 → Years [2025, 2026], default Year 2026
func TestTaxViewClosedKoinlyYear(t *testing.T) // year=2025 → shows "Koinly" source badge, Koinly wallet names in rows, no Needs review tab
func TestHoldingsOpenYearUsesToday(t *testing.T) // open 2026 → holdings header "as of 10/16/2026"
func TestTaxViewTabs(t *testing.T)            // POST /reports/tax/year year=2026 tab=income → patch contains id="tax-section" and the income table
```

- [ ] **Step 2–4:** `go test ./internal/server/ -run TestTaxView -v` and `TestHoldingsOpenYear` → FAIL; implement (`go tool templ generate`); PASS.

---

### Task 3: Koinly-format CSV exports

**Files:**
- Create: `internal/tax/report/koinlycsv.go`
- Modify: `internal/server/taxview.go` (routes `GET /reports/tax/{year}/capital-gains.csv`, `/income.csv`, `/holdings.csv`, `/transactions.csv`, through `csvHeaders` with filename `koinly-<kind>-<year>`)
- Test: `internal/tax/report/koinlycsv_test.go`

**Interfaces:**
- Produces: `func WriteGains(w io.Writer, r YearReport) error`, `WriteIncome`, `WriteHoldings`, and `func WriteTransactions(w io.Writer, year int, txs []core.Tx, res tax.Result, rs Resolver, loc *time.Location) error`.
  - For the transactions export, cost basis and gain per tx are the sums of that tx's disposal rows (by `TxID`).
  - Received Cost Basis = the leg's USD value.
  - For a closed CryptKeep year, transactions come from activity in that year. For a Koinly year, the endpoint returns 404 with "Koinly years: use Koinly's own transaction export".

- [ ] **Step 1: Write failing tests**

```go
func TestGainsHeaderExact(t *testing.T)      // first 3 lines == "Capital gains report 2026", "", the exact header
func TestGainsRowFormat(t *testing.T)        // disposal 0.5 BTC sold 2026-03-02 15:04 NY acquired Various → "03/02/2026 15:04,VARIOUS,BTC,0.50000000,1000.00,1500.00,500.00,,Coinbase,Long term"
func TestIncomeTypeMapping(t *testing.T)     // reward → "Reward"
func TestHoldingsTotalRow(t *testing.T)      // last line "Total,-,250.00,450.00,-"
func TestTransactionsKinds(t *testing.T)     // swap → "exchange" with sent/received columns; reward → crypto_deposit tag "Reward"
func TestCSVQuotesLabels(t *testing.T)       // wallet label `Cold, Ledger` → quoted field
```

- [ ] **Step 2–4:** `go test ./internal/tax/report/ -run 'TestGains|TestIncome|TestHoldings|TestTransactions|TestCSV' -v` → FAIL; implement; PASS.

---

### Task 4: TurboTax CSV, TXF, and printable 8949 with Schedule D

**Files:**
- Create: `internal/tax/report/turbotax.go`, `web/templates/form8949.templ`, `web/templates/print.templ` (a minimal print layout shared with Task 5)
- Modify: `internal/server/taxview.go` (routes `GET /reports/tax/{year}/turbotax.csv`, `/turbotax.txf`, `/8949`)
- Test: `internal/tax/report/turbotax_test.go`, `internal/server/taxview_test.go`

**Interfaces:**
- Produces:
  - `func WriteTurboTaxCSV(w io.Writer, r YearReport) error`
  - `func WriteTXF(w io.Writer, r YearReport, today time.Time) error`
  - `func Form8949(r YearReport) []Form8949Part`, where `Form8949Part{Box string; Long bool; Rows []Form8949Row; Total BoxTotal}`, `Form8949Row{Description, Acquired, Sold string; Proceeds, Cost, Gain float64}`, and parts are in box order G, H, I, J, K, L, omitting empty boxes.
  - `func ScheduleD(r YearReport) []ScheduleDLine`, where `ScheduleDLine{Line, Box string; Proceeds, Cost, Adjustments, Gain float64}`, with all six lines always present.

- [ ] **Step 1: Write failing tests**

```go
func TestTurboTaxHeaderAndRow(t *testing.T) // header exact; row "BTC,VARIOUS,1000.00,03/02/2026,1500.00"
func TestTXFRecord(t *testing.T)            // long-term I/L row → "TD\nN714\nC1\nL1\nP0.5 BTC\nDVARIOUS\nD03/02/2026\n$1000.00\n$1500.00\n^"
func TestForm8949Grouping(t *testing.T)     // rows in boxes I and L → two parts in order I, L; totals equal the row sums
func TestScheduleDLines(t *testing.T)       // box I total gain 6 → line "3" gain 6; line "1b" zeros present
func TestEmpty8949(t *testing.T)            // no rows → Form8949 empty slice; GET /reports/tax/2026/8949 → 200 with "No disposals in 2026"
func TestKoinlyYearBoxes(t *testing.T)      // Koinly year rows with wallet "Kraken" short → box H; "Solana (SOL) - K1S" long → L
```

- [ ] **Step 2–4:** `go test ./internal/tax/report/ ./internal/server/ -run 'TestTurboTax|TestTXF|TestForm8949|TestScheduleD|TestEmpty8949|TestKoinlyYearBoxes' -v` → FAIL; implement; PASS.

---

### Task 5: Complete tax report (CPA package)

**Files:**
- Create: `web/templates/taxreport.templ`
- Modify: `internal/server/taxview.go` (route `GET /reports/tax/{year}/report`)
- Test: `internal/server/taxview_test.go`

**Interfaces:**
- Page sections, in order:
  1. title "Crypto tax report <Y>" with the generated date
  2. summary (short-term and long-term proceeds, cost and gain; income; fees)
  3. method statement
  4. Schedule D table (Task 4 `ScheduleD`)
  5. Form 8949 detail (Task 4 `Form8949`, reused component)
  6. income detail
  7. year-end holdings
  8. wallets (label, type, box)
  9. open review issues (count per kind, open year only)
  10. disclaimer
- The method statement:
  - For a CryptKeep year: "Cost basis method: FIFO, tracked per wallet (Rev. Proc. 2024-28). Opening basis on <cutover> imported from Koinly's <Y−1> year-end holdings; acquisition dates of <Y−1> purchases back-filled from Koinly's transaction history; older units are reported as VARIOUS (long-term)."
  - For a Koinly year: "Figures for <Y> are Koinly's, imported verbatim."

- [ ] **Step 1: Write failing tests**

```go
func TestTaxReportSections(t *testing.T)   // GET /reports/tax/2026/report → contains each section heading in order, the method statement, and "Not tax advice."
func TestTaxReportKoinlyYear(t *testing.T) // year 2025 → "imported verbatim"
```

- [ ] **Step 2–4:** `go test ./internal/server/ -run TestTaxReport -v` → FAIL; implement; then `task generate && task vet && task test` → PASS.
