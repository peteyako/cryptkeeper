# effects — what a change hits

**Reads:** the change you are about to make. **Writes:** nothing; it is a catalog. **Human
check:** before editing, the cards named here have been opened and their *If you change this*
sections read. If this index and a card disagree, the card wins: fix this row.

Open the cards in the order listed; the first is the owner. Paths are under `objects/` unless
they start with `processes/`.

## Sources

| If you are changing… | Open |
|---|---|
| a `sources.Def` field, a source type, `Validate`, or a new secret field | sources/source-def, the per-type card, storage/secret-key, web/server, web/templ-components, web/demo-fixtures, processes/connect-source |
| `core.Holding`, `core.WalletResult` or the `AssetKey` rules | sources/source-contract, valuation/snapshot, valuation/price, valuation/value-history, ledger/csv-export, web/page-model, every source card; for `AssetKey` rules also views/portfolio-view, views/alert-rule, ledger/activity-entry, ledger/tax-lot (asset keys are stored in history rows, `lastgood_price`, the YAML stores, activity legs and lots) |
| Esplora servers, fallbacks, gap limit, pacing or 429 handling (Bitcoin) | sources/bitcoin-source, sources/http-client, storage/config, web/server |
| the saved Bitcoin scan or any source's activity cursor `State` / `Budget` | the source card, ledger/activity-entry, storage/database, processes/sync-activity, processes/connect-source (delete removes the saved scan) |
| Solana spam rules (`hideUnpriced`, `minUsd`, hidden mints, spam candidates) | sources/solana-source, valuation/snapshot, ledger/tax-lot, processes/compute-taxes, processes/refresh-balances (spam candidates hidden on assembly), processes/sync-activity (hidden mints are never valued) |
| a Solana DeFi adapter or `PositionKind` | sources/solana-defi, sources/solana-source, web/templ-components, valuation/snapshot, storage/config |
| Coinbase staked/vault detection or Kraken asset normalization | the source card, valuation/price, valuation/snapshot, ledger/activity-entry, ledger/manual-cost-basis (the Kraken CSV parser calls `kraken.NormalizeAsset`) |
| the `stock:<TICKER>` key or the brokerage cash rule | sources/brokerage-source, valuation/price, valuation/snapshot, sources/manual-source, sources/source-def (`Validate` calls `brokerage.ValidAsset`) |
| `manual.NormalizeAsset` | sources/manual-source, views/portfolio-view, views/alert-rule, valuation/price |
| a `core.Kind` value | sources/source-contract, sources/source-def, web/templ-components, views/alert-rule |
| `httpx` pacing, the 429 hold, `StatusError`, or `New()` defaults | sources/http-client, sources/bitcoin-source, sources/solana-source, sources/coinbase-source, sources/kraken-source, valuation/price, processes/sync-activity, processes/evaluate-alerts (ntfy retries go through `Do`) |

## Valuation and views

| If you are changing… | Open |
|---|---|
| `Snapshot`, `WalletView` or `HoldingRow` fields, or `Status` meaning | valuation/snapshot, web/page-model, web/templ-components, web/demo-fixtures, views/portfolio-view, views/alert-rule, valuation/value-history, processes/refresh-balances, processes/compute-taxes (quiet set from `HiddenRows`, the `Warm` gate), processes/evaluate-alerts, processes/backfill-history, ledger/csv-export (`export.Holdings`), ledger/koinly-import (cutover balances) |
| price source order, `Want` keys, `StaleMax` or stale marking | valuation/price, valuation/snapshot, views/alert-rule, sources/brokerage-source |
| historical closes, `price_history`, `Series`, CoinGecko or Yahoo pacing | valuation/price, valuation/value-history, processes/backfill-history, processes/compute-taxes, processes/sync-activity (legs are valued via `Historical`), ledger/tax-report (year-end holdings are priced via `Historical`) |
| the `history` row format, `Record` or `Compact` | valuation/value-history, valuation/snapshot, views/alert-rule, ledger/csv-export, storage/database, processes/backfill-history, processes/refresh-balances (`Record`; `Compact` at startup), processes/evaluate-alerts (`portfolio_move` reads `ValueAt`) |
| the portfolio `Selector`, YAML shape, ids, `Apply` or `Include` | views/portfolio-view, storage/yaml-store, valuation/snapshot, valuation/value-history, views/alert-rule, storage/backup-file, ledger/tax-lot, processes/compute-taxes (the `tax.yaml` scope is a portfolio id; its wallets decide what is taxed) |
| an alert rule type, field or validation, or `alert_state` keys | views/alert-rule, views/portfolio-view, processes/evaluate-alerts, web/page-model, storage/backup-file |

## Ledger and taxes

| If you are changing… | Open |
|---|---|
| `core.Tx` / `Leg` fields or `TxKind` meaning | ledger/activity-entry, ledger/tax-lot, ledger/csv-export, the source cards with activity (bitcoin, solana, coinbase, kraken), ledger/koinly-import, processes/sync-activity, processes/compute-taxes, processes/backfill-history (rollback), processes/evaluate-alerts (`large_activity`), ledger/manual-cost-basis (the CSV parsers build `core.Tx`), ledger/tax-report (`transactions.csv`), valuation/value-history (`Backfill`), views/alert-rule |
| transfer matching (`MatchTransfers`, `PeerID`) | ledger/activity-entry, ledger/tax-lot, processes/sync-activity, processes/compute-taxes |
| `tax.Lot` / `Disposal` / `Year`, FIFO rules, or `tax.Input` / `tax.Result` | ledger/tax-lot, ledger/tax-report, ledger/koinly-import, processes/compute-taxes, processes/backfill-history (its rollback mirrors `tax.RollBack`), storage/backup-file (closed years ride in backups), ledger/manual-cost-basis (`migrateCostBasis` builds `tax.Lot`) |
| `data/tax.yaml` or `data/tax/<year>.yaml` fields | ledger/tax-lot, ledger/tax-report, ledger/koinly-import, storage/yaml-store, storage/backup-file |
| anything that feeds tax lots (activity, tags, closed years, tax settings) | ledger/tax-lot, web/server (the tax cache must be invalidated), processes/compute-taxes |
| Form 8949 boxes, rounding or `YearReport` | ledger/tax-report, ledger/tax-lot, web/page-model, web/templ-components (the 8949 template calls `report.Form8949`), web/demo-fixtures |
| Koinly CSV headers, wallet matching or the accept flow | ledger/koinly-import, ledger/tax-lot, storage/database (the `kv` draft) |
| the Coinbase/Kraken CSV parsers | ledger/manual-cost-basis, ledger/activity-entry, ledger/tax-lot, sources/coinbase-source, sources/kraken-source (dedupe matches their `ExternalID`), web/server (the import handlers hard-code `coinbase-csv` / `kraken-csv`) |
| an `/export/*.csv` column | ledger/csv-export, then the card that owns the data (snapshot, value-history, activity-entry, tax-lot) |

## Storage

| If you are changing… | Open |
|---|---|
| a table or column | storage/database, the owning package's card (restore is generic; a table to skip goes in `notRestored`, `internal/store/snapshot.go:20`) |
| a new YAML store file or backup section | storage/yaml-store, storage/backup-file, processes/backup-restore |
| `yamlfile` strictness or backup-copy naming | storage/yaml-store, every YAML store's card, storage/backup-file |
| a `config.yaml` key or env var | storage/config, the consuming card; `config.example.yaml` and `.env.example` move with it |
| the secret key, `enc:v1:` values or the encrypted-export envelope | storage/secret-key, sources/source-def, storage/backup-file, storage/database, processes/connect-source, processes/backup-restore |
| the backup file schema or version | storage/backup-file, processes/backup-restore |
| the database file name or its legacy names | storage/database |

## Web

| If you are changing… | Open |
|---|---|
| a `templates.*Page` field | web/page-model, web/templ-components, web/demo-fixtures, web/server |
| an element id or SSE patch target in a `.templ` | web/templ-components, web/server, web/demo-fixtures |
| a CSS class or design token | web/static-assets, web/templ-components, web/page-model (`format.go` writes token names into inline styles) |
| a route, `guard`, `basicAuth`, `render` or the page region ids | web/server, web/templ-components, processes/serve-page, processes/refresh-balances (`/stream`), processes/sync-activity (`/activity/sync`), web/demo-fixtures (a new page also wants a uipreview route) |
| number/date formatting or sync-error wording | web/page-model, web/templ-components |
| `datastar.js`, the manifest or icons | web/static-assets, web/templ-components, web/server (the unauthenticated icon and manifest routes) |
