---
type: process
universe: live
status: verified
verified: 2026-10-19 @ d4899f7
consumes: [source-contract, source-def, price, config, tax-lot]
produces: [activity-entry, database, value-history, alert-rule]
---

# sync-activity

On a timer or a kick, pull each activity-capable source's transactions (resuming from saved state), value them, store them, pair up transfers between the user's own sources, and alert on what is new.

## Input → Movement → Output

Input: the live sources that implement `core.ActivitySource`, the per-source `activity_state` row, a backfill horizon (`Since`), and a 15 minute tick or `Kick()`. Movement: `SyncOnce` runs up to four sources at once; each source makes one `Activity` call (three while still backfilling), the entries are valued from historical closes and upserted, and the new state is saved; then `MatchTransfers` links sends to receives and `OnNew` hands the fresh entries to the alert engine. Output: rows in `activity`, an updated `activity_state` per source (state blob, `backfill_done`, `last_sync`, `last_error`), and SSE patches of the sync card.

## Why this shape

- Opaque per-source `State` plus a `BackfillDone` flag: history is fetched in budgeted slices over several ticks, so a wallet with years of transactions neither times out nor hammers a public API. Later ticks fetch only what is new.
- A source may return an error with a non-nil `State`: partial progress is saved, so a slow page does not restart the backfill from zero.
- Failures are written to that source's `last_error` and never abort the pass; one broken exchange key must not stop Bitcoin syncing. Panics are recovered the same way.
- Public hosts are paced process-wide (`httpx.SetPace`), and a 429 holds every waiting request to that host. Without it, parallel sources would burn through the public RPC limit and get the IP blocked.
- Upsert keeps an existing `PeerID` and `transfer` kind: a source re-reporting a pending entry as confirmed must not undo transfer matching.
- The `Valuer` is asked once per symbol and day per pass; unpriceable legs stay unvalued and `revalue` retries them on later passes.

## Steps

1. `serve` builds the `Syncer` with `Since` = backfill days, or back to the open tax year's start if earlier (`syncSince`), plus interval, `Valuer` = the prices service, `SkipMints`, and `OnNew` calling `engine.OnActivity`: `main.go:495-508`, `main.go:251-258`.
2. `go syncer.Run(ctx)` starts it; registry changes also kick it: `main.go:509-511`. Other kicks: backup import, source save (delete kicks only through the registry hook), and accepting a Koinly opening-year import (`internal/server/settings_handlers.go:323-325`, `internal/server/source_handlers.go:351-353`, `internal/server/koinly_handlers.go:613`).
3. `Run` syncs at once, then on `Interval` (default 15 min) or `kick`: `internal/activity/syncer.go:164-183`. `SyncOnce` takes a one-slot semaphore so passes run one after another: `internal/activity/syncer.go:189-196`.
4. `a.activitySources` supplies the entries: registry sources whose `Source` implements `core.ActivitySource`: `main.go:367-375`.
5. Each source runs in an errgroup limited to 4, marked running (signals SSE subscribers) until its outcome is saved: `internal/activity/syncer.go:213-229`, `:135-152`.
6. `syncSource` loads `state` and `backfill_done`; it makes 1 call, or up to 3 while backfilling (stopping once the source reports done), all sharing one 60s timeout, budget 30 per call: `internal/activity/syncer.go:379-396`, `:418-421`, `:19-25`, `internal/activity/store.go:324-331`.
7. The `Activity` call hits the source's own paced HTTP: `httpx.Wait` before each request, `hold` on 429: `internal/httpx/httpx.go:72`, `:92-98`. Pace is set per host in `internal/sources/solana/solana.go:107`, `internal/sources/solana/rpc.go:57`, `internal/sources/bitcoin/bitcoin.go:178`, `internal/prices/prices.go:141`.
8. `prepare` fills `SourceID`/`ID`, drops id-less entries and values legs and fees (skipping hidden mints): pegged stablecoins at face value, others at the UTC day's close, SPL tokens by mint. The closes come from `prices.Service.Historical`/`HistoricalMint`, which read and fill the shared `price_history` cache: `internal/activity/syncer.go:403-405`, `:428-445`, `:500-524`, `internal/prices/history.go:43-131`.
9. `Store.upsert` writes in one SQL transaction, preserves transfer links, and returns only entries whose IDs were new: `internal/activity/store.go:83-145`.
10. `saveProgress` stores the new state, backfill flag and `last_sync`, clears `last_error`; an error is then written by `saveError` (after that saved progress when the source still returned a `State`): `internal/activity/syncer.go:411-417`, `:370-376`, `internal/activity/store.go:334-358`.
11. After all sources: `matchTransfers` reads the last 90 days, runs `MatchTransfers` (same tx hash, or amount within tolerance in a time window, different sources) and upserts the changed ones: `internal/activity/syncer.go:235-238`, `:345-358`, `internal/activity/transfers.go:51`.
12. Fresh entries get any link made this pass, are sorted oldest first and passed to `OnNew` (panic-guarded), synchronously: the pass waits for the alert engine, including any ntfy send: `internal/activity/syncer.go:239-256`, `:335-342`. `OnActivity` evaluates only enabled `large_activity` rules: `internal/alerts/engine.go:127-144`.
13. `revalue` fills legs left without USD (earlier price outages) and upserts only the ones that gained a value: `internal/activity/syncer.go:257`, `:282-320`.
14. The Activity page and Settings open `GET /activity/sync`: it subscribes to the syncer, patches `ActivitySyncCard` on every start/finish signal or 30s tick, and on the Activity page also offers new entries when the count grows: `web/templates/js.go:98-104`, `internal/server/activity_handlers.go:149-182`.

## If you change this

- **Hits:** [activity-entry](../objects/ledger/activity-entry.md) shape and the `activity`/`activity_state` schema (`internal/activity/store.go:22-41`); every source's `Activity` and its `State` format ([source-contract](../objects/sources/source-contract.md)); transfer matching, which tax lots depend on ([compute-taxes](compute-taxes.md); a sync does not call `invalidateTax`, so new entries reach tax figures when the one-minute cache expires); `large_activity` alerts ([evaluate-alerts](evaluate-alerts.md)); the sync card; the backfill horizon, which the open tax year also moves; leg valuation, which shares `prices.Historical` and the `price_history` cache with [backfill-history](backfill-history.md) and tax reports.
- **Does not hit:** balances and the snapshot ([refresh-balances](refresh-balances.md) is a separate loop with its own interval); the portfolio `history` rows. Hand-entered sources (manual, brokerage) have no activity.

## Surfaces

| Surface | Role |
|---|---|
| `GET /activity/sync` | SSE live sync card |
| Activity page | list, filters, "new entries" offer |
| `/sources` save/delete, backup import | kick the syncer |
| Koinly accept | kick, because the horizon reaches back to the open year |
| Alert engine | `OnNew` receiver |

## See

- Objects: [activity-entry](../objects/ledger/activity-entry.md), [source-contract](../objects/sources/source-contract.md), [price](../objects/valuation/price.md), [alert-rule](../objects/views/alert-rule.md), [database](../objects/storage/database.md), [config](../objects/storage/config.md), [value-history](../objects/valuation/value-history.md), [tax-lot](../objects/ledger/tax-lot.md)
- Source: `internal/activity/syncer.go`, `internal/activity/store.go`, `internal/activity/transfers.go`
