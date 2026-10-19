# How to walk this map

The map describes the code; the code stays the source of truth. Every card cites `path:line`,
and where a card and the code disagree, the code wins and the card is `stale`.

## Walk

1. Know the noun? Find it in [objects/_index.md](objects/_index.md) and open that one card.
2. Changing something? Open [effects/CONTEXT.md](effects/CONTEXT.md) first: it names the cards
   whose *Hits* you must read before editing.
3. Following a movement (a refresh, a sync, a tax run, a page load)? Open its card from
   [processes/_index.md](processes/_index.md).
4. Stop there. Read the cited source lines, not more cards. Never load a whole cluster.

## Universes

| Universe | Meaning |
|---|---|
| **live** | In force. Implement and cite against these. |
| **leftover** | Still present, no longer the main path. Touch only if that path is in scope. |
| **ghost** | Named or filed but not wired. Do not implement against these. |

## Name collisions

Product words and code names that disagree, or one word with two meanings:

| Word | Means | Not to be confused with |
|---|---|---|
| CryptKeeper | the app; the Go module and binary are `crypto-portfolio` | its earlier names CryptKeep and Vault, kept only as legacy fallbacks (`CRYPTKEEP_SECRET_KEY`/`VAULT_SECRET_KEY`, `cryptkeep.db`/`vault.db`, the `cryptkeep:` export tag, old localStorage keys) |
| vault | `core.Holding.Vault`: a Coinbase vault account or the Jupiter limit-order vault; also Solana pool/escrow token vaults | the app's old name, or the "Vault design system" boards in `docs/design/` |
| Wallet (UI) | any connected source, exchanges included: `sources.Def` + its `core.Source` | `portfolio.Wallet` (the aggregator's fetch input) and `portfolio.WalletView` (a priced row in the snapshot) |
| `portfolio` / `portfolios` | `portfolio`: aggregation (`Snapshot`, `Refresher`, SSE `Broker`, value `History`) | `portfolios`: the user's named views in `data/portfolios.yaml` |
| Snapshot | `portfolio.Snapshot`, the priced holdings | `store.DB.Snapshot`, a VACUUM INTO copy of the database for backups |
| store | `internal/store`, the SQLite wrapper | `portfolios.Store`, `RuleStore` and the other `*Store` types, which are YAML stores (except `activity.Store`, which wraps the SQLite `activity` tables) |
| Settings | the Settings page: display and API-key settings saved in the database `kv` table, plus backup export and restore | `config.yaml`, which the Settings page never edits |
| backup | a user-downloaded export file (`internal/backup`) | the automatic timestamped copies in `data/backups/`, written before a YAML store is overwritten or a closed tax year is reopened |
| history | portfolio value over time (`history` table) | daily price closes (`price_history`), or transaction history, which is *activity* |
| Point | `portfolio.Point`, a chart sample | `prices.Point`, a daily close |
| activity | a transaction, `core.Tx`, stored by `internal/activity` | `core.ActivitySource`, the interface a source implements to supply them |
| manual | the hand-typed source type (`manual.Holding`, reused by brokerage) | manual cost basis: `data/costbasis.yaml` (retired; migrated once into `tax.yaml` `manualLots`, kept for backups) |
| cost basis | the tax engine's FIFO lots (`internal/tax`, `data/tax.yaml`) | `internal/costbasis`: its exchange CSV parsers are live (they feed activity), its manual store is retired |
| queue | `internal/tax/queue.go`, the FIFO lot queue | the *Needs review* queue: `tax.Result.Issues` + `internal/server/review_handlers.go` |
| Koinly | `internal/tax/koinly`, which reads Koinly exports | `internal/tax/report/koinlycsv.go`, which writes Koinly-shaped CSVs |
| import | `/reports/import`: exchange CSVs into activity | `/reports/koinly/*` (Koinly wizard), and YAML imports (backup, portfolios) |
| hidden | `WalletResult.Hidden`: mints the user hid (Solana `hideMints`) | spam candidates the aggregator hides (unverified, unpriced, dust); `HiddenRows` holds both kinds |
| NormalizeAsset | `kraken.NormalizeAsset`: Kraken codes to ticker plus a staked flag | `manual.NormalizeAsset`: tidies typed input, also used by portfolios and alerts; Coinbase's `normalizeCurrency` is a third variant |
| coinbase, kraken | the live source types | the `coinbase-csv` / `kraken-csv` CSV pseudo-sources, and Coinbase/Kraken as price sources in `internal/prices` |
| stable | `core.Pegged`: USD, USDC, USDT | `core.IsStable`, a wider stablecoin list |
| export | CSV downloads (`internal/export`) and tax reports | YAML exports: backup, sources, portfolios (encrypted when `CRYPTKEEPER_SECRET_KEY` is set) |
| Server, render | `internal/server` | `cmd/uipreview`, which has its own small server and render and does not import `internal/server` |

## Shelves

| Folder | Holds |
|---|---|
| `objects/` | one card per noun, in six clusters ([objects/CONTEXT.md](objects/CONTEXT.md)) |
| `processes/` | one card per movement that really runs ([processes/CONTEXT.md](processes/CONTEXT.md)) |
| `effects/` | the change-impact index |
| `_meta/` | the schema and the build script for the generated files |
| `_templates/` | blank cards: a new card is a copy, not a blank page |
