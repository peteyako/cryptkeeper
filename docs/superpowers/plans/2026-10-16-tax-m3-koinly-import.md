# Tax M3: Koinly import, reconciliation and FIFO switch-over — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- Import Koinly's 2025 exports and reconcile them against CryptKeep's own balances.
- Write the result as a closed 2025 year whose closing lots are the 2026 opening lots.
- Switch the dashboard, wallet tiles and Reports P&L from average cost to `tax.Compute`.

**Architecture:**
- **Stores** (in `internal/tax`, modelled on `costbasis.ManualStore`): `data/tax.yaml` holds mappings, manual lots, tags and price overrides. `data/tax/<year>.yaml` holds closed years.
- **Koinly code** (`internal/tax/koinly`, pure functions): parsers, mapping suggestions, and the opening-lot builder (back-fill plus reconciliation).
- **Roll-back** (`tax.RollBack`): derives each wallet's quantity at the cutover from the current holdings minus the activity since then.
- **Import wizard:** the page `/reports/koinly`, which keeps its upload draft in the `kv` table.
- **Switch-over:** the server builds `tax.Input` from the stores, activity, config timezone and the spam set. A new `taxCache` replaces `costCache`, and `avgcost.go` is deleted.

**Tech Stack:** Go 1.26, templ + Datastar SSE, modernc sqlite (`kv` table), `gopkg.in/yaml.v3` via `internal/yamlfile`.

**Spec:** `docs/superpowers/specs/2026-10-16-koinly-tax-design.md` §2 (import), §3 (storage, backup, sync horizon, timezone), and §1 "Dashboard replacement".

## Global Constraints

- **Koinly CSV layouts are exactly these.** Every file starts with a title line and a blank line, and the header is on line 3.
  - `end_of_year_holdings_report`:
    - Title `Balances as at MM/DD/YYYY HH:MM`.
    - Header `Asset,Quantity,Cost (USD),Value (USD),Description`. Asset is `SYM (Name)`.
    - The last row is `Total,-,...` and is skipped.
  - `transaction_history`:
    - Title `Transaction report YYYY`.
    - Header `Date,Type,Tag,Sending Wallet,Sent Amount,Sent Currency,Sent Cost Basis,Receiving Wallet,Received Amount,Received Currency,Received Cost Basis,Fee Amount,Fee Currency,Gain (USD),Net Value (USD),Fee Value (USD),TxSrc,TxDest,TxHash,Description`.
    - Date `2006-01-02 15:04:05 UTC`.
  - `capital_gains_report`:
    - Title `Capital gains report YYYY`.
    - Header `Date Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period`.
    - Dates `01/02/2006 15:04` in the tax timezone. Holding period is `Short term` / `Long term`.
  - `income_report`:
    - Title `Income report YYYY`.
    - Header `Date,Asset,Amount,Value (USD),Type,Description,Wallet Name`.
  - Numbers may be quoted and carry thousands separators (`"30,954.12"`). Empty cells are 0.
- **Cutover** = 00:00 on 1 January of the year after the imported year, in the tax timezone. For this user it is 2026-01-01.
- **Tax timezone:** config `tax.timezone` (an IANA name). An empty value means `time.Local`. **Ruling:** it lives only in `config.yaml`, not in `tax.yaml`. The spec lists it in both places; config wins because it is set once per install.
- **"Before the imported year" lots:** `Various: true`, `Acquired` = 1 January of the imported year 00:00 in the tax timezone, minus 1 second. For 2025 that is 2024-12-31 23:59:59. 8949 prints them as VARIOUS.
- **Missing-basis opening units** (CryptKeep holds more than Koinly): `Acquired` = cutover, `Flag: tax.FlagMissingBasis`, and cost = the user-entered extra basis, default 0.
- **Back-fill transfer-in lots:** `Flag: tax.FlagTransferDate`, dated at the transfer.
- **Invariants after building, per mapped asset:**
  - Σ lot amount == max(Koinly qty, CryptKeep qty).
  - Σ lot cost == Koinly cost + extra basis, unless clamped. A clamp is reported as a problem.
- **The untracked wallet is `tax.Untracked`.** Wallet-map values are a source ID or `untracked`. An asset-map value of `""` means "skip" (spam, zero-value NFTs).
- **Spam set** passed to `tax.Compute`: `spl:<mint>` for every `cfg.Solana.HideMints`, plus every asset key in the latest snapshot's `HiddenRows`.
- **Sync horizon:** activity `Since` = the earlier of `now − backfillDays` and the start of the open tax year.
- The UI follows the existing Reports components and CSS classes (`card`, `tbl`, `btn`). Each SSE handler goes through the guard, as the existing ones do.
- Do NOT `git add`, `git commit`, `git stash` or switch branches.
- Gate: `task generate && task vet && task test`.

## Review Focus

1. **Two Koinly assets share a symbol** (`JUP (Jupiter)` and `JUP (Jupiter Price)`). Back-fill of transaction-history rows (symbol only) must use the mapped holding with the larger Koinly cost, and the other must not steal its lots. Test: Task 5 `TestOpeningSymbolCollision`.
2. **A Koinly wallet maps to a source that has no snapshot row for the asset** (CryptKeep shows 0). All of Koinly's quantity goes to `untracked`, and no negative lot is ever produced. Test: Task 5 `TestOpeningAllUntracked`.
3. **The dated back-fill cost exceeds Koinly's total cost** for an asset. The remainder is clamped to 0 and a problem is reported; the result never has a negative cost. Test: Task 5 `TestOpeningNegativeRemainderClamped`.
4. **A re-upload after 2026 is already closed.** Accept is refused with a clear message. Test: Task 7 `TestKoinlyAcceptRefusedWhenLaterYearClosed`.
5. **A hand-edited `tax.yaml` that no longer parses.** The last good content keeps being served and the error is shown on Reports; nothing panics. Test: Task 1 `TestSettingsStoreBadFileKeepsLastGood`.

---

### Task 1: Tax config, `tax.yaml` store, closed-year store

**Files:**
- Modify: `internal/config/config.go` (`Tax struct { Timezone string yaml:"timezone" }`, `Config.Tax`, and `func (c Config) TaxLocation() (*time.Location, error)`), `config.example.yaml` (a commented `tax:` block)
- Create: `internal/tax/settings.go`, `internal/tax/years.go`
- Test: `internal/tax/settings_test.go`, `internal/tax/years_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:

```go
type TagEntry struct { Tag Tag `yaml:"tag"`; Note string `yaml:"note,omitempty"` }
type Settings struct {
	Version    int                 `yaml:"version"`
	WalletMap  map[string]string   `yaml:"walletMap,omitempty"`  // Koinly wallet name → source ID | Untracked
	AssetMap   map[string]string   `yaml:"assetMap,omitempty"`   // Koinly asset "SYM (Name)" → asset key | "" (skip)
	ManualLots []Lot               `yaml:"manualLots,omitempty"`
	Tags       map[string]TagEntry `yaml:"tags,omitempty"`       // by Tx.ID
	Prices     map[string]float64  `yaml:"prices,omitempty"`     // PriceKey / FeePriceKey → USD
	WalletBox  map[string]string   `yaml:"walletBox,omitempty"`  // source ID → 8949 box letter (M5)
}
func ParseSettings(raw []byte) (Settings, error)        // strict decode, version 0|1, validates tags (known Tag values), prices (finite ≥ 0), lots (amount > 0, cost ≥ 0, wallet non-empty)
func NewSettingsStore(path, backupDir string) (*SettingsStore, error)
func (s *SettingsStore) Get() Settings                   // deep copy, reloads on mtime/size change
func (s *SettingsStore) Err() error
func (s *SettingsStore) Set(v Settings) error            // atomic write + timestamped backup (keep 20)
func (s *SettingsStore) Update(fn func(*Settings)) error // read-modify-write under the lock

type Summary struct {
	ShortProceeds, ShortCost, ShortGain float64 `yaml:...`
	LongProceeds, LongCost, LongGain    float64
	Income, Fees                        float64
}
type Year struct {
	Version     int        `yaml:"version"`
	Year        int        `yaml:"year"`
	Source      string     `yaml:"source"` // "koinly" | "cryptkeep"
	ClosedAt    time.Time  `yaml:"closedAt"`
	Summary     Summary    `yaml:"summary"`
	Disposals   []Disposal `yaml:"disposals,omitempty"`
	Income      []Income   `yaml:"income,omitempty"`
	GiftsLost   []Disposal `yaml:"giftsLost,omitempty"`
	ClosingLots []Lot      `yaml:"closingLots,omitempty"`
	ClosingParked []Parked `yaml:"closingParked,omitempty"` // balances inside the user's own positions at year end (locked JUP, perps collateral)
}
func Summarize(disposals []Disposal, income []Income) Summary // Fees = Σ gain of Note "Fee" rows; Income = Σ ValueUSD
func NewYearStore(dir, backupDir string) (*YearStore, error)  // dir = <data>/tax
func (s *YearStore) Years() []int                              // ascending
func (s *YearStore) Get(year int) (Year, bool)
func (s *YearStore) LastClosed() (Year, bool)
func (s *YearStore) Save(y Year) error                         // refuses if a later year exists: "close or reopen years in order"
func (s *YearStore) Delete(year int) error                     // only the latest year; else an error
func OpenYearStart(last Year, ok bool, loc *time.Location) time.Time // Jan 1 of last.Year+1 in loc; zero time when !ok
```

- [ ] **Step 1: Write failing tests**

```go
func TestTaxLocationDefaultLocal(t *testing.T)  // Tax.Timezone "" → time.Local; "America/New_York" → that zone; "Nope/Zone" → error
func TestSettingsRoundTrip(t *testing.T)        // Set → file on disk → new store Get equal; unknown field in YAML → ParseSettings error with line number
func TestSettingsRejectsBadTag(t *testing.T)    // tags: {x: {tag: bogus}} → error
func TestSettingsStoreBadFileKeepsLastGood(t *testing.T) // after a valid Set, write garbage to the file → Get returns the last good content, Err() non-nil
func TestSettingsUpdate(t *testing.T)           // Update adds Tags["a"] → persisted
func TestYearStoreOrder(t *testing.T)           // Save 2025 then 2026 ok; Save 2025 again while 2026 exists → error; Delete 2025 while 2026 exists → error; Delete 2026 ok
func TestYearRoundTripPreservesTimes(t *testing.T) // Disposal Sold/Acquired with a non-UTC location round-trip to the same instant
func TestSummarize(t *testing.T)                // short gains 10 and −4, long 7, Fee row gain 1, income 3 → Short{Gain 6}, Long{Gain 7}, Fees 1, Income 3
func TestOpenYearStart(t *testing.T)            // last 2025, NY → 2026-01-01 00:00 NY; !ok → zero
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tax/ ./internal/config/ -run 'TestTaxLocation|TestSettings|TestYear|TestSummarize|TestOpenYear' -v`
Expected: FAIL.

- [ ] **Step 3: Implement.** Mirror `internal/costbasis/manual.go` (`reload`, `Get`, `Err`, `set`) for the settings store. The year store keeps one file per year, named `<year>.yaml`.

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

---

### Task 2: Koinly CSV parsers

**Files:**
- Create: `internal/tax/koinly/parse.go`
- Test: `internal/tax/koinly/parse_test.go`, plus `internal/tax/koinly/testdata/{holdings,transactions,gains,income}.csv` (synthetic, in the exact layouts above, using made-up wallets and amounts)

**Interfaces:**
- Produces:

```go
type Kind string // "holdings" | "transactions" | "gains" | "income" | "other-gains"
func Detect(raw []byte) (Kind, error) // by the header on line 3 (BOM-tolerant)
type Holding struct { Asset, Symbol, Name string; Quantity, CostUSD, ValueUSD float64 }
func ParseHoldings(raw []byte) (asOf time.Time, rows []Holding, err error) // asOf parsed from the title, in UTC wall time (the caller re-interprets it in the tax zone)
type Txn struct {
	Time time.Time; Type, Tag string
	SendWallet string; SentAmount float64; SentCurrency string; SentCost float64
	RecvWallet string; RecvAmount float64; RecvCurrency string; RecvCost float64
	FeeAmount float64; FeeCurrency string; Gain, NetValue, FeeValue float64
	TxSrc, TxDest, TxHash, Description string
}
func ParseTransactions(raw []byte) ([]Txn, error)
func ParseGains(raw []byte, loc *time.Location) ([]tax.Disposal, error) // Asset and Wallet verbatim; Term from Holding period; Note verbatim
func ParseIncome(raw []byte, loc *time.Location) ([]tax.Income, error)
```

- [ ] **Step 1: Write the fixtures and failing tests**

```go
func TestDetect(t *testing.T)            // each fixture → its Kind; a random CSV → error "not a Koinly report"
func TestParseHoldings(t *testing.T)     // fixture row `BTC (Bitcoin),0.71318207,"30,954.12","62,401.60",...` → {Asset "BTC (Bitcoin)", Symbol "BTC", Name "Bitcoin", Quantity 0.71318207, CostUSD 30954.12}; the Total row is skipped; asOf = 2025-12-31 23:59
func TestParseHoldingsNFTName(t *testing.T) // `TNFT#42 (TestPass)` → Symbol "TNFT#42", Name "TestPass"; `$ CLAIM AT EXAMPLE.COM ($ Example.com)` → Symbol "$ CLAIM AT EXAMPLE.COM", Name "$ Example.com" (split at the LAST " (")
func TestParseTransactions(t *testing.T) // an exchange row → SentAmount 0.12345678 SOL, SentCost 15, RecvAmount 31.250123 USDC, RecvCost 31.25, Gain 16.25; Time 2025-03-14 09:30:00 UTC; empty cells → 0
func TestParseGains(t *testing.T)        // `02/11/2025 14:27,08/19/2024 10:52,SOL,0.0025,0.30,0.50,0.20,"",Solana (SOL) - K1S,Short term` with loc NY → Sold 2025-02-11 14:27 NY, Term ShortTerm, Wallet "Solana (SOL) - K1S"
func TestParseIncome(t *testing.T)       // one row → tax.Income{Type verbatim, Wallet verbatim}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tax/koinly/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement `parse.go`** with a small `num(s string) (float64, error)` that strips `,`. Use `encoding/csv` with `FieldsPerRecord = -1`, starting at line 3.

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

---

### Task 3: `tax.RollBack`, wallet quantities at the cutover

**Files:**
- Create: `internal/tax/rollback.go`
- Test: `internal/tax/rollback_test.go`

**Interfaces:**
- Produces: `func RollBackParked(positions map[string]map[string]float64, txs []core.Tx, cutover time.Time) []Parked`. `positions` holds the current amount inside positions per source and asset (snapshot rows with `Position != ""`). For each tx with `!Time.Before(cutover)` that is a position move (`Kind == core.TxPosition`, or tagged `position` in `Settings.Tags`; the caller passes the tag set as a `func(txID string) bool`), add its signed legs: parked at the cutover = parked now + Σ signed legs. Negative results become 0. The result is sorted.
- Produces: `func RollBack(current map[string]map[string]float64, txs []core.Tx, cutover time.Time) map[string]map[string]float64`. The maps are source ID → asset key → amount.
  - For each tx with `!Time.Before(cutover)`, subtract its effect from its `SourceID`:
    - failed tx: the fee only
    - `stake` and `position`: no effect (the units stay inside the wallet's holdings)
    - otherwise: every leg plus `Fee`
  - The result is a fresh map.
  - Values within `dust` of 0 become 0. Negative results are kept, so the caller can show them.

- [ ] **Step 1: Write failing tests**

```go
func TestRollBackSubtractsLaterActivity(t *testing.T) // current sol/SOL 10; 2026-02 receive +3, 2026-03 send −1 with fee −0.01 → 8.01
func TestRollBackIgnoresStakeAndPosition(t *testing.T) // stake SOL −5 and position JUP −1000 after the cutover → unchanged
func TestRollBackFailedFeeOnly(t *testing.T)          // failed swap legs ignored, fee −0.000005 added back
func TestRollBackBeforeCutoverIgnored(t *testing.T)   // tx on 2025-12-31 → unchanged
func TestRollBackParked(t *testing.T)                 // positions sol/JUP 1500 now; 2026-03 position deposit JUP −500 → parked at cutover [{sol JUP 1000}]
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tax/ -run TestRollBack -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

---

### Task 4: Mapping suggestions

**Files:**
- Create: `internal/tax/koinly/suggest.go`
- Test: `internal/tax/koinly/suggest_test.go`

**Interfaces:**
- Consumes: Task 2 `Holding`, `Txn`.
- Produces:

```go
type Wallet struct { ID, Label, Type string; Addresses []string } // a CryptKeep source as the suggester sees it
type AssetInfo struct { Key, Symbol, Name string }              // an asset CryptKeep knows (snapshot rows + activity legs)
func SuggestWallets(txns []Txn, gains []tax.Disposal, wallets []Wallet) map[string]string
func SuggestAssets(holdings []Holding, known []AssetInfo) (m map[string]string, ambiguous map[string][]string)
func WalletNames(txns []Txn, gains []tax.Disposal) []string // every Koinly wallet name seen, sorted
```

- **SuggestWallets:** for each Koinly wallet name, collect its addresses: `TxDest` of rows where it is the receiving wallet, and `TxSrc` where it is the sending wallet.
  - The first wallet whose `Addresses` intersect (case-sensitive) wins.
  - Otherwise, a name containing "Kraken" maps to the single `kraken` source, and "Coinbase" to the single `coinbase` source.
  - Otherwise `tax.Untracked`.
- **SuggestAssets:**
  - Candidates are known assets with `strings.EqualFold(Symbol, h.Symbol)`.
  - Exactly one candidate: map to it. Several: prefer an equal `Name`; otherwise `""`, with the candidates in `ambiguous`.
  - None, with `CostUSD == 0 && ValueUSD == 0`: `""` (skip).
  - None, with value: `strings.ToUpper(h.Symbol)`. Such units end up in `untracked`.

- [ ] **Step 1: Write failing tests**

```go
func TestSuggestWalletByAddress(t *testing.T)  // Koinly "Solana (SOL) - K1S" receives with TxDest "4QVq…"; wallet {ID "sol-main", Addresses ["4QVq…"]} → "sol-main"
func TestSuggestWalletByName(t *testing.T)     // "Kraken (API keys)" and one kraken source "kraken" → "kraken"
func TestSuggestWalletUntracked(t *testing.T)  // "Ethereum (ETH & Tokens) Main" → tax.Untracked
func TestSuggestAssets(t *testing.T)           // BTC→"BTC"; "JUP (Jupiter)" with known JUP{Name "Jupiter"} and spl:X{Symbol "JUP", Name "Jupiter Price"} → "JUP"; "JUP (Jupiter Price)" → "spl:X"; XMR (no match, value>0) → "XMR"; "TNFT#42 (TestPass)" cost 0 value 0 → ""
func TestSuggestAssetsAmbiguous(t *testing.T)  // two known with Symbol "BOZO" and neither name matches → "" and ambiguous lists both keys
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tax/koinly/ -run TestSuggest -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

---

### Task 5: Opening-lot builder (back-fill and reconciliation)

**Files:**
- Create: `internal/tax/koinly/opening.go`
- Test: `internal/tax/koinly/opening_test.go`

**Interfaces:**
- Consumes: Task 2 `Holding`, `Txn`; Task 3 output for `Balances`; `tax.Lot`, flags, `tax.Untracked`.
- Produces:

```go
type OpeningInput struct {
	Year       int                           // the imported year (2025)
	Loc        *time.Location
	Holdings   []Holding
	Txns       []Txn
	WalletMap  map[string]string             // Koinly wallet → source ID | tax.Untracked
	AssetMap   map[string]string             // Koinly "SYM (Name)" → asset key | ""
	Balances   map[string]map[string]float64 // RollBack result: source ID → asset key → qty at cutover
	ExtraBasis map[string]float64            // asset key → USD for CryptKeep-excess units
	Parked     []tax.Parked                  // RollBackParked result
}
type ReconRow struct {
	KoinlyAsset, AssetKey                string
	KoinlyQty, KoinlyCost, CryptKeepQty  float64
	Untracked, MissingBasis              float64 // units sent to tax.Untracked / units with no Koinly basis
	Dated, Various                       float64 // units in dated 2025 lots / in "before 2025" lots
	Lots                                 []tax.Lot
}
type Opening struct { Rows []ReconRow; Lots []tax.Lot; Parked []tax.Parked; Problems []string } // Parked = OpeningInput.Parked passed through, for the closed year's ClosingParked
func BuildOpening(in OpeningInput) Opening
```

**Algorithm**, per holding with a non-empty mapped key `K` (Koinly quantity `Q`, cost `C`):

1. `W[w] = max(0, Balances[w][K])` for every source `w`. `CK = Σ W`. If `Q > CK + dust`, then `W[Untracked] = Q − CK`.
2. Map transaction-history symbols to `K`: a symbol belongs to the holding whose `Symbol` matches and whose mapped key is non-empty. If several holdings match, take the one with the larger `CostUSD`.
3. **Back-fill per wallet `w`** with `W[w] > 0`:
   - Take the `Txns` whose `RecvWallet` maps to `w` and whose `RecvCurrency` maps to `K`, with types `crypto_deposit`, `exchange` and `transfer`, in time order newest first. Stop when `W[w]` is covered or dated quantity reaches `Q` in total.
   - Each consumed row becomes a lot: `{Wallet w, Asset K, Amount take, CostUSD RecvCost×take/RecvAmount, Acquired row.Time}`, with `Flag FlagTransferDate` for `transfer` rows.
4. **Remainder per wallet:** `R[w] = W[w] − dated[w]`.
   - If `CK > Q`, the excess `E = CK − Q` becomes missing-basis lots, spread over the `R[w]` of real wallets pro-rata. Cost is `ExtraBasis[K] × share`, `Acquired` is the cutover, and the lot carries `FlagMissingBasis`.
   - What is left of each `R[w]` becomes one `Various` lot.
5. **Cost of the `Various` lots:** `C − Σ dated cost`, spread pro-rata by quantity. If that is negative, use 0 and append the problem `"<K>: dated 2025 cost exceeds Koinly's total cost; older lots set to $0"`.
6. Also append a problem when `|Q − CK| > max(dust, 1e-6×Q)`: `"<K>: Koinly <Q> vs CryptKeep <CK>"`. This is informational, so the UI can list differences.
7. `Opening.Lots` is all lots sorted by (Wallet, Asset, Acquired). `Rows` is sorted by `KoinlyCost` descending.

- [ ] **Step 1: Write failing tests**

```go
func TestOpeningBackfillNewestFirst(t *testing.T) // Q=10 C=1500 SOL; W[sol]=10; 2025 deposits into the Koinly wallet mapped to sol: Jun 3@450, Sep 4@800 → lots: Sep 4 @800, Jun 3 @450, Various 3 @250 (1500−1250); Σ amount 10, Σ cost 1500
func TestOpeningUntrackedExcess(t *testing.T)     // Q=1.17 ETH C=1966.25; no source holds ETH → one Various lot in Untracked, 1.17 @1966.25
func TestOpeningAllUntracked(t *testing.T)        // mapped wallet exists but Balances has no row for the asset → all to Untracked; no negative amounts
func TestOpeningMissingBasisExcess(t *testing.T)  // Q=99.657 C=24242.66; CK=101.0 across two wallets (60/41); ExtraBasis 0 → missing-basis lots total 1.343 split 60:41, cost 0, FlagMissingBasis, Acquired = 2026-01-01 00:00 loc; Σ cost 24242.66
func TestOpeningNegativeRemainderClamped(t *testing.T) // dated cost 2000 > C 1500 → Various cost 0; problem string present; Σ cost 2000 (dated lots kept exact)
func TestOpeningTransferFlag(t *testing.T)        // a `transfer` row into the wallet → lot Flag FlagTransferDate
func TestOpeningSymbolCollision(t *testing.T)     // holdings "JUP (Jupiter)" cost 10771 → JUP and "JUP (Jupiter Price)" cost 0 → spl:X; txn RecvCurrency "JUP" back-fills JUP only
func TestOpeningSkippedAsset(t *testing.T)        // AssetMap value "" → no row, no lots
func TestOpeningVariousDate(t *testing.T)         // Various lot Acquired == 2024-12-31 23:59:59 in loc, Various true
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tax/koinly/ -run TestOpening -v`
Expected: FAIL.

- [ ] **Step 3: Implement `opening.go`.**

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

---

### Task 6: Switch the dashboard and Reports from avg cost to `tax.Compute`

**Files:**
- Create: `internal/tax/views.go` (read helpers over `Result`), `internal/server/tax.go` (`taxCache`, input assembly)
- Modify:
  - `internal/server/server.go` (`Deps`: add `TaxSettings *tax.SettingsStore`, `TaxYears *tax.YearStore`, `TaxLoc *time.Location`; drop the `costs costCache` field in favour of `taxes taxCache`)
  - `internal/server/reports_handlers.go` (replace `costResult`, `pnlRows`, `pnlCells`, `walletCost`, `reportsPage` P&L parts and `exportPnL`; remove `POST /reports/basis` and `/reports/basis/{asset}/delete`)
  - `internal/server/handlers.go` as needed
  - `web/templates/reports.templ`, `web/templates/reports_view.go`, `web/templates/models.go` (remove `CostBasisPanel` and `setCostLink`; `PnLRow` drops `Overridden` and gains `MissingUnits float64`)
  - `internal/export/export.go` (`PnL` columns)
  - `main.go` (construct the stores at `<data>/tax.yaml` and `<data>/tax/`; pass `cfg.TaxLocation()`; set the activity `Since` per the sync horizon)
  - `internal/demo/reports.go` (keep demo compiling with the new view model)
- Delete: `internal/costbasis/avgcost.go`, `internal/costbasis/avgcost_test.go`
- Test: `internal/tax/views_test.go`, `internal/server/server_test.go` (adapt the existing reports and P&L tests; add new ones)

**Interfaces:**
- Produces (`internal/tax/views.go`):

```go
func (r Result) Holding(asset string, wallets func(string) bool) (amount, cost, missing float64)
// Σ open lots of asset in wallets (nil = every wallet except Untracked); missing = Σ amount of FlagMissingBasis lots
func (r Result) Unrealized(asset string, wallets func(string) bool, currentAmount, priceUSD float64) (basis, unrealized, pct float64, ok bool)
// covered = min(currentAmount, amount − missing); basis = cost of non-missing lots × covered/(amount − missing); ok=false when covered ≤ 0
func (r Result) RealizedByYear(loc *time.Location) map[int]float64
func Coverage(r Result, rows []portfolio.AssetRow) float64 // value-weighted share covered by non-missing lots (tracked wallets)
```

- Produces (`internal/server/tax.go`):
  - `func (s *Server) taxResult(ctx context.Context) (tax.Result, bool)`, cached for 1 minute like `costResult`, with `func (s *Server) invalidateTax()`.
  - It builds `tax.Input{From: tax.OpenYearStart(s.TaxYears.LastClosed(), loc), Loc, Opening: lastClosed.ClosingLots, OpeningParked: lastClosed.ClosingParked, Manual: settings.ManualLots, Txs: dedupeImported(all activity), Tags: (settings.Tags → map[string]tax.Tag), Prices: settings.Prices, Spam: (HideMints ∪ snapshot HiddenRows asset keys)}`.
- Realized P&L per year on Reports = the closed years' `Summary` (Short+Long gain) merged with `RealizedByYear` of the open years.

- [ ] **Step 1: Write failing tests**

```go
// internal/tax/views_test.go
func TestHoldingExcludesUntracked(t *testing.T) // lots: sol SOL 2 @200, untracked SOL 1 @50 → Holding("SOL", nil) = (2, 200, 0)
func TestUnrealizedSkipsMissingBasis(t *testing.T) // lots: SOL 2 @200 + SOL 1 @0 FlagMissingBasis; current 3 @ price 150 → basis 200, unrealized 100, pct 50
func TestRealizedByYear(t *testing.T) // disposals in 2026 (gain 5) and 2027 (gain −2) → {2026: 5, 2027: −2}
// internal/server/server_test.go
func TestReportsUsesTaxLots(t *testing.T) // closed 2025 year with closing lot sol/SOL 10 @1000; snapshot SOL 10 @ $150 → Reports shows basis $1,000 and unrealized +$500
func TestBasisRoutesRemoved(t *testing.T) // POST /reports/basis → 404 or 405
func TestWalletTilesFromLots(t *testing.T) // wallet page for "sol" shows Cost basis $1,000
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tax/ ./internal/server/ -run 'TestHolding|TestUnrealized|TestRealized|TestReportsUsesTaxLots|TestBasisRoutesRemoved|TestWalletTiles' -v`
Expected: FAIL.

- [ ] **Step 3: Implement.** Run `go tool templ generate`. Remove every reference to `costbasis.Compute` / `costbasis.Result`, but keep `costbasis.ManualStore`, the CSV parsers and `dedupeImported`.

- [ ] **Step 4: Run the gate.** `task generate && task vet && task test`. Expected: PASS.

---

### Task 7: Koinly import wizard (`/reports/koinly`)

**Files:**
- Create: `internal/server/koinly_handlers.go`, `web/templates/koinly.templ`, `web/templates/koinly_view.go`
- Modify: `internal/server/server.go` (routes), `web/templates/reports.templ` (a "Import from Koinly" link card), `web/templates/models.go`
- Test: `internal/server/koinly_test.go`

**Interfaces:**
- Consumes: Tasks 1–5 and the server `taxResult` / `invalidateTax` from Task 6.
- **Draft:** kv key `tax.koinly.draft`, holding JSON `{Year int; AsOf time.Time; Holdings []koinly.Holding; Txns []koinly.Txn; Gains []tax.Disposal; Income []tax.Income; Files []string; WalletMap, AssetMap map[string]string; ExtraBasis map[string]float64}`.
- **Routes** (all POSTs SSE, through the guard):
  - `GET /reports/koinly`: full page. Step 1 is the upload form. Once a draft exists, it also shows Step 2 (the wallet mapping table and asset mapping table, pre-filled from the draft or the suggestions), Step 3 (the reconciliation table from `BuildOpening` with its problems list) and the Accept button.
  - `POST /reports/koinly/upload`: multipart, any number of `file` fields, 8 MB each. Each file is `Detect`-ed and parsed into the draft; a holdings file is required before Accept.
    - The year is taken from the holdings `asOf` year, or else the title year. Mismatched years are an error.
    - Suggestions are computed from `SuggestWallets` / `SuggestAssets`. The wallet list comes from `Registry.Defs()` plus `core.AddressLister.Addresses` where available; the asset list comes from snapshot rows and activity legs.
    - It patches `#koinly-wizard`.
  - `POST /reports/koinly/map`: fields `w.<koinly wallet>` = source ID | `untracked`, `a.<koinly asset>` = asset key | `""`, `x.<asset key>` = extra basis. It saves them to the draft and patches `#koinly-wizard`.
  - `POST /reports/koinly/accept`:
    - Refused when a year after the draft's year is already closed: `"2026 is already closed; reopen it first"`.
    - Otherwise it builds `Opening`, saves `tax.Year{Year: draft.Year, Source: "koinly", ClosedAt: now, Disposals: draft.Gains, Income: draft.Income, Summary: tax.Summarize(draft.Gains, draft.Income), ClosingLots: opening.Lots, ClosingParked: opening.Parked}`, writes the `WalletMap` and `AssetMap` into `tax.yaml`, deletes the draft, calls `invalidateTax()`, and redirects (Datastar `@get` / location patch) to `/reports`.
  - `POST /reports/koinly/discard`: deletes the draft.
- **Roll-back input:**
  - `current` = the latest snapshot's `Wallets[i].Rows` summed by `AssetKey` per `WalletView.ID`.
  - `txs` = all activity, after `dedupeImported`.
  - `cutover` = Jan 1 of `draft.Year+1` in `TaxLoc`.
  - Wallets whose `activity_state.backfill_done` is false, or whose earliest stored tx is after the cutover, get a warning row: "activity history for <label> doesn't reach <cutover>; its quantity may be off".

- [ ] **Step 1: Write failing tests** (use the server test harness in `server_test.go`, with temp stores and the synthetic fixtures from Task 2's `testdata` read via a relative path)

```go
func TestKoinlyUploadDetectsFiles(t *testing.T)  // upload holdings+transactions → page shows both file names and a wallet mapping row per Koinly wallet name
func TestKoinlyMapPersistsDraft(t *testing.T)    // POST map w.X=sol → GET shows sol selected
func TestKoinlyAcceptWritesYear(t *testing.T)    // after upload+map, accept → TaxYears.Get(2025) exists, Source "koinly", ClosingLots Σ cost == holdings Σ cost of mapped assets; tax.yaml WalletMap saved; draft deleted
func TestKoinlyAcceptRefusedWhenLaterYearClosed(t *testing.T) // TaxYears has 2026 → accept → message "2026 is already closed; reopen it first"; nothing written
func TestKoinlyUploadRejectsOtherCSV(t *testing.T) // a Coinbase CSV → error message "not a Koinly report"
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/server/ -run TestKoinly -v`
Expected: FAIL.

- [ ] **Step 3: Implement.** Run `go tool templ generate`. Follow `csvImport` (`reports_handlers.go:228`) for multipart handling and `patchReports` for SSE patches.

- [ ] **Step 4: Run the gate.** `task generate && task vet && task test`. Expected: PASS.

---

### Task 8: Backup sections and migrating `costbasis.yaml` lots

**Files:**
- Modify:
  - `internal/backup/backup.go` (`File.Tax *tax.Settings yaml:"tax,omitempty"`, `File.TaxYears []tax.Year yaml:"taxYears,omitempty"`, `Stores.Tax`, `Stores.TaxYears`, `Parse`, `check`, `Import`, `Result.Tax bool`, `Result.TaxYears int`, `Summary`)
  - both `backupStores()` (`main.go`, `internal/server/settings_handlers.go`)
  - the backupImport empty-result check
  - `web/templates/settings.templ` (BackupIO text: "…cost basis, tax settings and closed tax years")
  - `main.go` (the one-time migration)
  - `README.md` (the Taxes section: the import flow, `tax.yaml`, `tax/<year>.yaml`, and the not-tax-advice line)
- Test: `internal/backup/backup_test.go`, `main_test.go`

**Interfaces:**
- **Merge rules** (`replace=false`):
  - Tax: incoming map entries override per key; manual lots are appended unless an identical lot already exists.
  - TaxYears: a year not present locally is added in ascending order. An existing year is kept, and the result says it was skipped.
- **`replace=true`:** Tax replaces everything; TaxYears replaces all local years.
- **Migration** (`func migrateCostBasis(cb *costbasis.ManualStore, ts *tax.SettingsStore, log *slog.Logger)` in main.go):
  - Runs only when `tax.yaml` does not exist and `costbasis.yaml` has lots.
  - Each manual lot becomes `tax.Lot{Wallet: tax.Untracked, Asset, Amount, CostUSD, Acquired: lot date (zero → 2000-01-01 UTC)}`. `tax.Lot` has no note field, so notes are dropped. The migration logs the warning: "moved N manual lots from costbasis.yaml to tax.yaml (wallet: untracked); edit tax.yaml to assign wallets".
  - Asset overrides are not migrated, and a warning is logged.

- [ ] **Step 1: Write failing tests**

```go
func TestBackupRoundTripTax(t *testing.T)       // export with Tax+TaxYears(2025) → import into empty stores → equal
func TestBackupMergeKeepsExistingYear(t *testing.T) // local 2025 (source cryptkeep) + incoming 2025 (koinly) merge → local kept; Result says 0 years imported
func TestBackupReplaceTaxYears(t *testing.T)    // replace → local years == incoming
func TestMigrateCostBasisLots(t *testing.T)     // costbasis.yaml with one lot and no tax.yaml → tax.yaml has 1 manual lot in untracked; second run does nothing
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/backup/ . -run 'TestBackup|TestMigrate' -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run the gate.** `task generate && task vet && task test`. Expected: PASS.
