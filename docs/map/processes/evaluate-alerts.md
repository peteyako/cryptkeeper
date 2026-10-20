---
type: process
universe: live
status: verified
verified: 2026-10-20 @ ea0d7ef
consumes: [alert-rule, snapshot, activity-entry, value-history, portfolio-view, config, database]
produces: [database]
---

# evaluate-alerts

After every finished refresh and price-only reprice, and after every pass that stored new activity, test each enabled alert rule against its saved state, send a push notification to ntfy when one trips, and log the attempt.

## Input → Movement → Output

Input: a fresh `Snapshot` (from the refresher: a finished refresh or a reprice, which keeps the last wallets and moves only prices; progress snapshots are not evaluated) or the entries newly inserted by a sync pass, plus the rules in `data/alerts.yaml` and the per-rule rows in `alert_state`. Movement: the engine takes its lock, loads each rule's state, compares (balance baseline, price threshold with arming, portfolio move over a history window, outage clock, large sends), and for a trip calls `fire`, which posts to ntfy and records the result. Output: an ntfy push, a row in `alert_log` (with the error text if delivery failed), and updated `alert_state`.

## Why this shape

- Delivery is at-most-once: a fired event is consumed (cooldown clock, drop baseline, price disarm) whether or not ntfy answered. Otherwise an ntfy outage would turn into a notification storm the moment it recovered. The failure is visible in the log instead.
- ntfy requests do retry (429, 5xx, transport) inside the HTTP client with a 20s overall send timeout, which protects against blips; the engine does not re-send afterward.
- State is written to the database, not memory, so a restart does not re-announce, and `flushState` ignores request cancellation because an alert may already be out.
- Price rules arm and disarm with 2% hysteresis, and the first sight of a rule (or an edited threshold) only arms: a price already past the line does not alert, and a price jittering on the line does not spam.
- Snapshot evaluation runs in a goroutine off the refresher, serialised by a mutex, so slow notifications never delay snapshots and state read-modify-write never races. Activity evaluation is not detached: it runs inside the sync pass.
- `large_activity` ignores transactions older than 72h by default, so the first backfill of years of history does not look like a burst of sends; only hot and hardware wallets are watched.
- A wallet that is not freshly synced is neither compared nor used as a baseline, so a stale cached result cannot fake a balance drop.

## Steps

1. `alertsEngine` builds the engine: rules, DB, label lookups, portfolio and history valuers, and an `Ntfy` notifier only if `ntfy.url` is configured: `main.go:386-431`.
2. Snapshot trigger: the refresher's `OnSnapshot` runs `engine.OnSnapshot` in a goroutine; activity trigger: the syncer's `OnNew` calls `engine.OnActivity` with wallet kinds synchronously, before the pass's revalue step: `main.go:490-493`, `main.go:505-507`, `internal/portfolio/refresher.go:311-313` (refresh), `:248-250` (reprice), `internal/activity/syncer.go:239-256`.
3. `OnSnapshot` returns if there is no rule store or database, locks, garbage-collects state of deleted or disabled rules, then evaluates each enabled rule inside a panic guard: `internal/alerts/engine.go:93-123`, `:167-174`, `internal/alerts/state.go:138-158`.
4. `balance_drop`: per watched, freshly synced wallet compare current amounts to the stored baseline, fire on a fall beyond `MinPct`, then store the current amounts as the new baseline: `internal/alerts/engine.go:335-366`.
5. `price`: skip stale or missing prices; compute threshold and re-arm line; first sight arms; an armed crossing disarms and fires if cooled down: `internal/alerts/engine.go:396-436`.
6. `portfolio_move`: if cooled down, compare the portfolio's current value (not valued when the snapshot has stale data) with `History.ValueAt` a window ago; fire when the change meets `Pct`: `internal/alerts/engine.go:465-493`, `main.go:404-425`.
7. `source_down`: a stale or errored wallet starts a down clock; fire once when it passes `Minutes`; recovery clears it: `internal/alerts/engine.go:525-557`.
8. `OnActivity` evaluates only enabled `large_activity` rules: sends from hot or hardware wallets, not failed, not already announced (the last 200 ids are remembered), within max age, at least `MinUsd`; the batch becomes one notification: `internal/alerts/engine.go:127-144`, `:593-643`.
9. `fire` stamps the time, `send` calls the notifier under a 20s timeout and recovers panics (with no notifier configured it returns "no notifier configured", so rules still trip and log), then `record` inserts into `alert_log` and keeps the newest 500 rows: `internal/alerts/engine.go:179-200`, `internal/alerts/state.go:178-195`.
10. `Ntfy.Send` validates the URL, posts plain text with Title, Priority, Tags, Click and Bearer headers through `httpx.Client.Do` (3 retries), and scrubs the topic URL and token from errors: `internal/alerts/ntfy.go:43-100`, `internal/httpx/httpx.go:57-118`.
11. State changes go out in one transaction per rule: `internal/alerts/state.go:105-134`.
12. Test path: `POST /alerts/test` refuses if no ntfy URL is set, otherwise `Engine.Test` sends a "CryptKeeper test" notification as rule `test` and logs it; the page patches the result and the recent log: `internal/server/alerts_handlers.go:261-275`, `internal/alerts/engine.go:147-163`.
13. The Alerts page lists rules and the last 50 log rows: `internal/server/alerts_handlers.go:88-113`.

## If you change this

- **Hits:** [alert-rule](../objects/views/alert-rule.md) types and fields (`internal/alerts/rules.go`, rule form, backup `Alerts` section), `alert_state` key names (an unreadable key is treated as first sight and re-arms), `alert_log` shape and the Alerts page, the `Snapshot` fields the rules read (`Prices`, `Wallets[].Status`, `Rows`, `HiddenRows`), redaction of amounts, and both triggers in `main.go`. Anything slow in `OnActivity` (a blocked lock, an ntfy send with retries) holds up that sync pass ([sync-activity](sync-activity.md)) until it returns.
- **Does not hit:** snapshot building or the refresh schedule; what activity sync fetches and stores; CSV exports.

## Surfaces

| Surface | Role |
|---|---|
| Refresher `OnSnapshot` | triggers snapshot rules |
| Syncer `OnNew` | triggers `large_activity` |
| `/alerts`, `/alerts/new`, `/alerts/{id}/edit`, `POST /alerts*` | rule CRUD and the recent log |
| `POST /alerts/test` | send a test push |
| `data/alerts.yaml`, `alert_state`, `alert_log` | rules, state, history |
| `config.yaml` `ntfy:` | topic URL, redaction, token via env |

## See

- Objects: [alert-rule](../objects/views/alert-rule.md), [snapshot](../objects/valuation/snapshot.md), [activity-entry](../objects/ledger/activity-entry.md), [value-history](../objects/valuation/value-history.md), [portfolio-view](../objects/views/portfolio-view.md), [database](../objects/storage/database.md), [config](../objects/storage/config.md)
- Source: `internal/alerts/engine.go`, `internal/alerts/state.go`, `internal/alerts/ntfy.go`
