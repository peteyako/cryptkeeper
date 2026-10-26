---
type: object
cluster: views
universe: live
status: verified
verified: 2026-10-26 @ be0017a
entity: internal/alerts/rules.go
---

# Alert rule

A user-defined alert (`alerts.Rule`, stored in `data/alerts.yaml`) that the `Engine` evaluates after every refresh or new activity, delivering through ntfy and logging to `alert_log`.

## Why this shape

Rules are hand-editable YAML, so a field that belongs to another rule type is a validation error: a typo must not silently do nothing (`internal/alerts/rules.go:45-46`, `:184-203`). Engine state (baselines, armed flags, cooldown clocks) is not in the file but in SQLite, so editing rules never resets history and deleting a rule garbage-collects its state (`internal/alerts/state.go:15-33`, `:138-158`). Delivery is at-most-once: an event is consumed whether or not ntfy succeeds, so an outage cannot become a retry storm (`internal/alerts/engine.go:34-36`).

## Shape

- Types: `balance_drop` (wallets, minPct), `price` (asset + exactly one of above/below), `portfolio_move` (portfolio id or `all`, pct, window), `source_down` (minutes), `large_activity` (minUsd) (`internal/alerts/rules.go:22-28`, `:48-52`, `:207-243`).
- Common fields `id` (`^[a-z0-9][a-z0-9-]{0,39}$`), `name` (notification title), `enabled` (nil = on), `cooldown` (Go duration, default 6h; balance_drop and source_down ignore it) (`rules.go:30-43`, `:56-79`, `:84-94`). `portfolio_move` fills defaults `portfolio: all`, `window: 24h` (`:138-145`).
- `File{Version: 1, Rules}`; `Parse` is strict (unknown fields error with line numbers) (`rules.go:266-269`, `:327-329`). `price` asset keys keep `spl:` and `stock:` forms (`:148-159`).
- `RuleStore`: same hand-edit-reload / validate-whole-file / atomic-write-with-30-backups pattern as the portfolios store; `Upsert` derives ids like `price-btc`; `Import(rules, replace)` (`internal/alerts/rulestore.go:16`, `:36-47`, `:81-185`, `:206-211`).
- `Engine` inputs: `Rules`, `DB`, `Notifier`, `Redact`, `BaseURL`, plus closures `Portfolio` (value of a portfolio in a snapshot; ok=false skips) and `History` (value at a past time), `Labels`, `PortfolioLabel`, `MaxTxAge` (default 72h) (`engine.go:21-58`, `main.go:386-431`).
- Evaluation: `OnSnapshot` runs the four snapshot types for enabled rules, each under `safely`; `OnActivity` runs `large_activity` on newly inserted txs (`engine.go:92-145`; hooks at `main.go:491-493` and `:505-507`). One mutex serializes read-modify-write state (`engine.go:60`).
- Per-type rules in brief: price re-arms only after a 2% retreat and ignores stale prices (`engine.go:65-67`, `:396-436`); balance_drop compares synced wallets only against the last baseline (`:335-366`); source_down fires once per outage after N minutes (`:525-557`); portfolio_move needs history reaching back the window and a non-stale total (`:465-493`, `main.go:412`).
- Tables (migration `alerts-v1`): `alert_state (rule_id, key, json)` PK (rule_id, key); `alert_log (id, ts, rule_id, title, body, err)`, pruned to 500 rows (`state.go:19-33`, `:178-197`).
- Delivery: `Notifier` interface; `Ntfy{URL, Token, Client}` plain-text POST with Title/Priority/Tags/Click headers; errors never include the topic URL or token (`internal/alerts/ntfy.go:18-60`). Built only if `config ntfy.url` is set; token from `NTFY_TOKEN` (`main.go:427-429`, `internal/config/config.go:36`, `:120-123`). `Redact` keeps amounts out of text.
- Wired: `alerts.NewRuleStore` at `main.go:193`; routes `/alerts*` including `POST /alerts/test` (`internal/server/server.go:180-186`, `internal/server/alerts_handlers.go`).

Citations: `internal/alerts/rules.go:56`, `internal/alerts/engine.go:37`, `internal/alerts/state.go:38`

## Connected to

- **owns:** `data/alerts.yaml` ([yaml-store](../storage/yaml-store.md)); `alert_state` and `alert_log` rows ([database](../storage/database.md)).
- **owned-by:** `main.go` builds the `RuleStore` and `Engine`; the server reads both (`internal/server/server.go:50-51`).
- **joins:** [snapshot](../valuation/snapshot.md) (`Refresher.OnSnapshot`, reads `Status`, `Prices[].Stale`, rows); [portfolio-view](portfolio-view.md) (`portfolio_move` target; rules ride along in "Export all"); [value-history](../valuation/value-history.md) (`ValueAt`); [activity-entry](../ledger/activity-entry.md) (`large_activity`); [price](../valuation/price.md) (price rules); [backup-file](../storage/backup-file.md) (a backup carries the rules).
- **looks-like-but-is-not:** the `Refresher`/`Broker` (SSE to browsers, not push notifications); `config.yaml` `ntfy` settings (transport config, not rules).

## If you change this

- **Hits:** rule add/edit forms and alerts page ([page-model](../web/page-model.md), `internal/server/alerts_handlers.go`); `portfolios.Parse` (embeds `alerts.NormalizeRules` for the alerts section, `internal/portfolios/portfolios.go:232-238`); `internal/backup` (its `alerts` section, normalized and imported, `internal/backup/backup.go:39,214,316-321`); engine state keys for that type; `testdata` under `internal/alerts`.
- **Does not hit:** [price](../valuation/price.md) fetching (rules only read `snap.Prices`); the SSE broker; `portfolios.yaml` content (only the export wrapper).

## Surfaces

| Surface | Role |
|---|---|
| Refresher `OnSnapshot` hook | `go engine.OnSnapshot` after each live refresh, never blocking it (`main.go:491-493`) |
| Activity syncer | `OnActivity` with new txs and wallet kinds |
| Server `/alerts*` | CRUD, test notification, recent log (`Recent`) |
| Portfolios export/import (server, CLI), backups | rules ride along: `WithAlerts` on export, `RuleStore.Import` on import (`internal/server/portfolio_handlers.go:421,491`, `main.go:627,663`, `internal/backup/backup.go:316-321`) |
| User editing `data/alerts.yaml` | writes; mtime reload, bad edit keeps last good |
| ntfy | outbound only |
| Tests | `internal/alerts/engine_test.go`, `rules_test.go`, `rulestore_test.go`, `ntfy_test.go` |

## See

- Source: `internal/alerts/rules.go`
- Movement: [evaluate-alerts](../../processes/evaluate-alerts.md)
