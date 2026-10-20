---
type: object
cluster: ledger
universe: live
status: verified
verified: 2026-10-20 @ 936c434
entity: internal/core/activity.go
---

# Activity entry

One transaction of one source: `core.Tx` (the UI and docs call it an "activity" entry), kept in the `activity` table by `activity.Store` and refreshed by `activity.Syncer`.

## Why this shape

An entry is the single ledger the tax engine, the activity feed, alerts and CSV export all read, so it is source-neutral: signed legs from the user's point of view, an optional fee leg held apart, and a `Kind` the sources assign (`internal/core/activity.go:11-22`). Sources never link the two halves of a move between the user's own wallets; the activity package does it afterwards and writes `PeerID`/`PeerSource` (`internal/core/activity.go:59-62`), because only the whole store can see both sides. The tax engine treats any `PeerID` as a transfer whatever the kind (`internal/tax/engine.go:284-290`), so matching quality decides what is taxable. The whole entry lives as JSON in one column; other columns exist only to filter and order (`internal/activity/store.go:21-31`).

## Shape

- `Tx`: `ID` = `SourceID:ExternalID` (`core.TxID`, `internal/core/activity.go:46,92`), `Legs []Leg`, `Fee *Leg` (negative, not in Legs), `TxHash`, `Status` pending/confirmed/failed (`internal/core/activity.go:44-63`).
- `Leg.USD` is the value at the time, signed like Amount, nil when unknown; the syncer fills it from historical prices (`internal/core/activity.go:35-41`, `internal/activity/syncer.go:282`).
- `ActivitySource` is the optional source interface; a source may return an error plus a result with non-nil `State` and that progress is saved (`internal/core/activity.go:82-89`, `internal/activity/syncer.go:392-407`).
- Tables `activity` (id, source_id, time ms, kind, status, assets, json) and `activity_state` (cursor blob, backfill_done, last_sync, last_error, synced_count) (`internal/activity/store.go:23-42`).
- `Store.Upsert` keeps a stored row's `PeerID`/`PeerSource` (and `kind=transfer`) when a source re-reports the entry (`internal/activity/store.go:73-77`). `All` is oldest first (what the tax engine wants), `List` newest first, limit 50 max 500 (`internal/activity/store.go:44-47,216-234`).
- `MatchTransfers`: pairs a send and a receive of one asset on different sources, strong by equal `TxHash`, weak by amount within 0.5% (or the send's fee) and receive stamped -10 min to +6 h; failed txs and trade/swap/reward/stake/fee/position never match (`internal/activity/transfers.go:12-20,51,177-183`).
- `Period` is a feed range (7d/30d/90d/mtd/ytd/yYYYY/custom/all); `Bounds` gives [since, until) and `Store.Years` lists years with activity (`internal/activity/period.go:14-20,83`).
- Sync pass: Syncer pulls each source (4 at a time), upserts, links transfers over the last 90 days, then back-values legs; it is owned by the Syncer (`internal/activity/syncer.go:19-26,189-262`). The backfill horizon is `syncSince`, pushed back to the open tax year's start (`main.go:251-258`). Steps live in the sync-activity process card.

## Connected to

- **owns:** per-source sync state (`activity_state`), transfer links.
- **owned-by:** [database](../storage/database.md) (the tables ride its migrations).
- **joins:** [tax-lot](tax-lot.md) (engine input, all entries, oldest first), [csv-export](csv-export.md) (activity.csv), [koinly-import](koinly-import.md) (rolls current holdings back over entries), [manual-cost-basis](manual-cost-basis.md) (its CSV parsers emit entries under `coinbase-csv`/`kraken-csv`), [source-contract](../sources/source-contract.md) (who implements `ActivitySource`), [price](../valuation/price.md) (the `Valuer`).
- **looks-like-but-is-not:** [value-history](../valuation/value-history.md) ("history" there is portfolio value, not transactions); [snapshot](../valuation/snapshot.md) holds balances, not transactions.

## If you change this

- **Hits:** every source that builds `core.Tx`, and the exchange CSV parsers ([manual-cost-basis](manual-cost-basis.md)); `internal/activity` tests; the two roll-backs over legs and kinds, `History.Backfill` ([value-history](../valuation/value-history.md)) and `tax.RollBack` ([koinly-import](koinly-import.md)); `large_activity` alerts (kind, `Leg.USD`, `internal/alerts/engine.go:583-646`); the Koinly-format `transactions.csv` (`internal/tax/report/koinlycsv.go:161-219`); `tax.Compute` (kind and PeerID semantics, `internal/tax/engine.go:275-310`); server `taxActivity` CSV dedupe and re-wallet (`internal/server/tax.go:231`); activity feed handlers (`internal/server/activity_handlers.go`); [csv-export](csv-export.md) columns; `activity-v1` migration if you add a column.
- **Does not hit:** [portfolio-view](../views/portfolio-view.md) or [snapshot](../valuation/snapshot.md) (balances are fetched by `Source.Fetch`, not rebuilt from entries); [manual-cost-basis](manual-cost-basis.md) YAML.

## Surfaces

| Surface | Role |
|---|---|
| Syncer (`main.go:495`) | writes entries, state, transfer links, leg USD |
| `/reports/import` csvImport | writes parsed CSV entries via `Upsert` (`internal/server/reports_handlers.go:312`) |
| `/activity`, `/activity/list` | read via `List`, `Years`, `Statuses` (`internal/server/activity_handlers.go:196-199`) |
| tax pages, closes, Koinly wizard | read all via `All` (`internal/server/tax.go:55-60`) |
| `/export/activity.csv` | reads via `All` (`internal/server/reports_handlers.go:394`) |
| chart backfill | reads via `taxActivity` (`internal/server/history_handlers.go:80`) |
| source removal | `DeleteSource` (`internal/server/settings_handlers.go:413`) |
| alerts engine | `OnNew` callback with fresh entries (`main.go:505`) |
| user | none; no YAML for activity |

## See

- Source: `internal/core/activity.go`, `internal/activity/`
- Doc: `docs/superpowers/specs/2026-10-16-koinly-tax-design.md`
