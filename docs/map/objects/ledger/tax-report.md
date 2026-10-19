---
type: object
cluster: ledger
universe: live
status: verified
verified: 2026-10-19 @ e280a97
entity: internal/tax/report/report.go
---

# Tax report

The year report shared by every tax download and print page (`internal/tax/report`), and the filler that writes it onto the official IRS Form 8949 and Schedule D PDFs (`internal/tax/irs`).

## Why this shape

One `YearReport` feeds all outputs so the CSVs, TXF, print pages and PDFs cannot disagree by a cent: Form 8949 rows are rounded to cents first and every total is the sum of rounded rows (`internal/tax/report/turbotax.go:152-156,256`). A closed year is built from its stored rows, an open one from a fresh `Compute`, so both reports come out of the same shape (`internal/tax/report/report.go:204-232`). The `irs` package only fills and never decides: it refuses any row whose box is not G..L, as `report.Form8949` (and so `ScheduleD`, `Totals`) does for a box outside G..L or of the wrong term, so a bad `walletBox` in `tax.yaml` surfaces as an error rather than a silently misfiled row (`internal/tax/irs/irs.go:63-70`, `internal/tax/report/turbotax.go:153-167`). `WriteTXF` does not check: an unknown box falls to code 712 (`turbotax.go:61-76`).

## Shape

**report**
- `Resolver` supplies display data (symbol, wallet label and type, `Box` overrides, year-end `Price`); every field may be nil (`internal/tax/report/report.go:14-31`). The server builds it in `resolver` (`internal/server/taxview.go:149`).
- `YearReport{Year, Source, Closed, AsOf, Loc, Rows, Income, GiftsLost, Holdings, Summary, Issues}`; `Row` = `tax.Disposal` plus symbol, wallet label and box (`internal/tax/report/report.go:32-64`).
- `BoxFor` picks the digital-asset box: short G/H/I (I for non-exchange wallets, G only for an exchange lot acquired in the same wallet on or after 2026-01-01, else H), long J/K/L; a `walletBox` override wins (`internal/tax/report/report.go:75-110`).
- `FromClosed`, `FromOpen`, `FromOpenRange` build reports; Koinly years take boxes from the Koinly wallet name (`internal/tax/report/report.go:204-232`).
- Writers: Koinly-style `WriteGains`, `WriteOtherGains`, `WriteIncome`, `WriteHoldings`, `WriteTransactions[Range]` (`internal/tax/report/koinlycsv.go:63-219`); `WriteTurboTaxCSV`, `WriteTXF`, `Form8949`, `ScheduleD`, `Totals` (`internal/tax/report/turbotax.go:42-256`). CSV text is formula-guarded by `safeText`.

**irs**
- `FillFormsContext` writes one PDF: Schedule D (2 pages) then 8949 copies (`internal/tax/irs/irs.go:57-62`). Name and SSN stay blank (`internal/server/taxdownloads.go:95-99`).
- Forms for 2025 are embedded; a later year uses `<data>/irs/<year>/` (downloaded from irs.gov if absent, retried after 24 h) only if its fields equal the 2025 ones, else the 2025 form is used with a note stamped on page one (`internal/tax/irs/irs.go:43`, `internal/tax/irs/revision.go:17-26,53-70`).

## Connected to

- **owns:** the report model, the PDF fill; no files of its own.
- **owned-by:** server `taxdownloads.go` serves it (`internal/server/taxdownloads.go:142-153`).
- **joins:** [tax-lot](tax-lot.md) (Year/Result in, `walletBox` from `tax.yaml`), [koinly-import](koinly-import.md) (`Source=="koinly"` years), [activity-entry](activity-entry.md) (transactions.csv), [templ-components](../web/templ-components.md) (print page `TaxReportPage`).
- **looks-like-but-is-not:** [csv-export](csv-export.md) (holdings/history/P&L/activity); [koinly-import](koinly-import.md) reads Koinly CSVs, `koinlycsv.go` writes Koinly-shaped ones.

## If you change this

- **Hits:** `internal/server/taxdownloads.go` and `taxview.go`; print page models and the 8949 templ in `web/templates` (they call `report.Form8949`); `internal/demo/tax.go` (builds demo reports); `irs` field maps if the box set or row shape changes; report tests including `final_test.go`.
- **Does not hit:** `tax.Compute` or `tax.Summarize` (the report reads them); `internal/export`.

## Surfaces

| Surface | Role |
|---|---|
| `/reports/tax/{year}/*.csv`, `turbotax.txf`, `irs-forms.pdf`, `8949`, `report`, `all.zip` | read; transactions.csv 404s for Koinly years (`internal/server/taxdownloads.go:28,116-125`) |
| complete report page summary | read `Totals` (`internal/server/taxdownloads.go:335`) |
| network | irs.gov form download via the server's HTTP client |
| user | sets box per wallet in `tax.yaml` `walletBox` |
| tests | `internal/tax/report/*_test.go`, `internal/tax/irs/irs_test.go` |

## See

- Source: `internal/tax/report/report.go`, `internal/tax/irs/irs.go`
- Doc: `docs/superpowers/specs/2026-10-16-koinly-tax-design.md`
