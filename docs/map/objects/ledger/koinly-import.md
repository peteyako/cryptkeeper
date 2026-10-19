---
type: object
cluster: ledger
universe: live
status: verified
verified: 2026-10-19 @ d4899f7
entity: internal/tax/koinly/parse.go
---

# Koinly import

`internal/tax/koinly` parses Koinly's CSV exports and builds the opening tax lots; the upload, map, accept or discard wizard (`/reports/koinly`) in `internal/server/koinly_handlers.go` runs it and closes the imported year.

## Why this shape

CryptKeeper replaces Koinly but must start from Koinly's last filed year, so the import does not replay history: it freezes Koinly's gains and income as a closed `koinly` year and seeds the next year's FIFO lots from Koinly's year-end holdings (`internal/server/koinly_handlers.go:597-672`). Koinly names wallets and assets its own way, so the wizard maps them by evidence (addresses first), then reconciles Koinly's quantities against what CryptKeeper's wallets held at the cutover; the gap becomes `untracked` or missing-basis lots, never a silent drop (`internal/tax/koinly/opening.go:62-72`). The draft sits in the database between steps, so a half-done import survives restarts and accepting is atomic from the user's side.

## Shape

- `Detect` identifies a report by its line-3 header (the title is line 1): holdings, transactions, gains, income, other-gains; the upload skips other-gains (`internal/tax/koinly/parse.go:21-35,58`, `internal/server/koinly_handlers.go:184`). Parsers: `ParseHoldings`, `ParseTransactions`, `ParseGains`, `ParseIncome`; gains and income become `tax.Disposal` / `tax.Income` directly (`internal/tax/koinly/parse.go:183,233,268,312`). Dates in gains/income are read in the tax zone.
- `MatchWallets` ranks evidence: (1) the wallet's own transaction addresses (`TxSrc`/`TxDest`) belong to a source; (2) an address in the wallet name, whole or shortened like `abc123...xy`; (3) exactly one source label in the name, matching the chain; (4) a lone Coinbase/Kraken source; else `untracked` (`internal/tax/koinly/suggest.go:44-118,136`). Each match carries a `Why` shown to the user.
- `SuggestAssets` maps Koinly `SYM (Name)` to asset keys, flagging ambiguous symbols (`internal/tax/koinly/suggest.go:174`).
- `BuildOpening` -> `Opening{Rows, Lots, Parked, Problems}` from `OpeningInput` (holdings, txns, maps, `RollBack` balances, user-entered `ExtraBasis`) (`internal/tax/koinly/opening.go:28-104`).
- Draft: `koinlyDraft` stored as JSON in the `kv` table under `tax.koinly.draft` (`internal/server/koinly_handlers.go:33-61`). Uploads of one year only; a re-uploaded kind replaces the earlier one (`internal/server/koinly_handlers.go:132-230`). Limits 8 MB per file, 64 MB per request.
- Steps: `koinlyUpload` -> `suggestMappings` (saved `tax.yaml` maps reused unless rematch) -> `koinlyMap` (edits wallet/asset maps, `ExtraBasis`, `SkipOK`) -> `koinlyAccept` / `koinlyDiscard` (`internal/server/server.go:150-155`).
- Accept refuses until: holdings uploaded, year has ended, no later year closed, balances loaded, assets resolved (`internal/server/koinly_handlers.go:742-775`). It saves `tax.Year{Source:"koinly"}` first, then writes `walletMap`/`assetMap` into `tax.yaml`, then deletes the draft; a map-save failure keeps the draft for retry (`internal/server/koinly_handlers.go:619-672`). Accept holds `draftMu` then `closeMu`, then kicks the Syncer to widen the backfill.
- `Why` for a Koinly wallet is dropped when the user picks another wallet (`internal/server/koinly_handlers.go:58-60`, `:523`).

## Connected to

- **owns:** the draft (`kv` row); parsing and opening logic.
- **owned-by:** [database](../storage/database.md) (`kv` table); results land in [yaml-store](../storage/yaml-store.md) files.
- **joins:** [tax-lot](tax-lot.md) (writes closed year, `tax.yaml` maps; `RollBack`), [activity-entry](activity-entry.md) (`taxActivity` rolled back to the cutover), [snapshot](../valuation/snapshot.md) (today's balances), [tax-report](tax-report.md) (Koinly years report from stored rows), [source-def](../sources/source-def.md) (addresses via `koinlyWallets`).
- **looks-like-but-is-not:** `report.WriteGains` etc. (`internal/tax/report/koinlycsv.go`) write Koinly-shaped CSVs; this card reads real ones.

## If you change this

- **Hits:** `internal/server/koinly_handlers.go` and the `KoinlyWizard` templates in `web/templates`; `tax.Year`/`Settings` fields it fills; `internal/tax/koinly/testdata` and tests (keep the fixtures synthetic).
- **Does not hit:** the compute-taxes engine (it only sees the saved year and `tax.yaml`); [manual-cost-basis](manual-cost-basis.md).

## Surfaces

| Surface | Role |
|---|---|
| `/reports/koinly` and `upload`, `map`, `rematch`, `accept`, `discard` | read/write the draft; accept writes the year |
| `kv` table | holds the draft |
| `tax.yaml`, `data/tax/<year>.yaml` | written on accept |
| user | uploads Koinly CSVs; may hand-edit `walletMap`/`assetMap` in `tax.yaml` |
| tests | `internal/tax/koinly/*_test.go`, `internal/server/koinly_test.go` |

## See

- Source: `internal/tax/koinly/`, `internal/server/koinly_handlers.go`
- Doc: `docs/superpowers/specs/2026-10-16-koinly-tax-design.md`
