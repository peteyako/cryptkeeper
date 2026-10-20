---
type: object
cluster: web
universe: live
status: verified
verified: 2026-10-20 @ ea0d7ef
entity: internal/server/server.go
---

# Server

The HTTP layer of CryptKeeper: package `internal/server`, a chi router (`server.New`) over a `Deps` struct that `main.go` fills in `serve`.

## Why this shape

One `Server` struct embeds `Deps`, so every handler reaches every store through `s.<Dep>` and tests build a server from fakes (`internal/server/server.go:79`). The app is a single-user LAN/Tailscale tool, so there is no login by default: safety comes from a mutation guard that makes every POST prove it came from the app's own Datastar pages, plus optional basic auth. Pages are server-rendered; the same handler answers a browser load with a whole document and an in-app click with an SSE patch of the page regions, so styles/scripts never reload (`internal/server/handlers.go:194-225`). The server never imports the bitcoin package: Bitcoin helpers arrive as injected funcs (`internal/server/server.go:67-73`).

## Shape

- `Deps` (`internal/server/server.go:37-77`): config, `DataDir`, `HTTPClient` (IRS form downloads), `Version`, `Log`, `Box` (secret key), `Registry`, `Portfolios`, `Activity`/`Syncer`, `CostBasis` (retired manual store, backup only), `AlertRules`/`Alerts`, `DB`, `Refresher`/`Broker`/`History`, `Prices`/`Historical`, `TaxSettings`/`TaxYears`/`TaxLoc`, Bitcoin funcs, `Now` (test clock). `HTTPClient`, `Blockstream`, `Syncer`, `Historical` are optional.
- Wiring: `serve` in `main.go:458-` builds the Aggregator/Refresher/Syncer/alerts engine, then `server.New(server.Deps{...})` at `main.go:513`; `ref.OnSnapshot` feeds the alerts engine, `registry.OnChange` kicks refresh+sync (`main.go:491,509`).
- Middleware: `Recoverer` + `securityHeaders` (nosniff, no-referrer, X-Frame-Options DENY) on everything (`internal/server/server.go:101,224`). Unauthenticated: `/healthz`, `/manifest.webmanifest`, `/static/icons/*` (`:104-106`). Everything else sits behind `basicAuth` (active only when `Cfg.Password` is set) then `guard` (`:108-109`, `internal/server/guard.go:19-62`).
- `guard`: GET/HEAD pass; any other method needs `Datastar-Request: true`, and, when the browser sends them, an `Origin` whose host matches and a `Sec-Fetch-Site` of same-origin/none; else 403 (`internal/server/guard.go:19-42`).
- Routes are registered in one block, `internal/server/server.go:108-196`; owners by area:

| Area | Handler file |
|---|---|
| overview, `/stream`, `/view`, `/chart`, `/refresh`, `/refresh/prices`, `/portfolio.json`, render helpers, wallets pages | `handlers.go` |
| chart backfill; coin page `/a/{asset}` | `history_handlers.go`; `coin_handlers.go` |
| sources add/edit/test/delete, `/sources/find-vault` (the Solana vault Find button) | `source_handlers.go` |
| portfolios manager, import/export/restore | `portfolio_handlers.go` |
| activity feed and sync card | `activity_handlers.go` |
| reports, CSV import, `/export/*.csv` | `reports_handlers.go` |
| Koinly wizard | `koinly_handlers.go` |
| tax view, year close, tags/write-offs, downloads | `taxview.go`, `close_handlers.go`, `review_handlers.go` (+ `review.go` panel builder), `taxdownloads.go` (`registerTaxDownloads`, `:142`), `tax.go` |
| alerts | `alerts_handlers.go` |
| settings (`/settings/logos`, `/settings/intervals`, `/settings/prices`, `/settings/bitcoin`), backup export/import | `settings_handlers.go` |

- Render helpers: `isNav` (GET with `Datastar-Request`) → `renderStatus` wraps ctx with `templates.WithNav` and sends one SSE `PatchElementTempl`; otherwise a full HTML document with the given status. A navigation always answers 200 because Datastar retries other statuses (`internal/server/handlers.go:196-225`). `shell()` builds `templates.Shell` (`:43`).
- SSE streams: `/stream` subscribes to the `Broker`, patches `Dashboard` per snapshot, 25 s keep-alive comment (`handlers.go:256-286`); `/activity/sync` patches `#activity-sync` on syncer changes and every 30 s, and `#activity-new` when `?feed=` is exceeded (`internal/server/activity_handlers.go:140-188`).
- Cost cache: `taxCache` holds the last `tax.Compute` for one minute plus per-year reports; `invalidateTax()` drops it after any change to activity, tags, lots or closed years (`internal/server/tax.go:17-30,65-74,84-102`; callers e.g. `close_handlers.go:188`, `koinly_handlers.go:611`, `settings_handlers.go:377`). Not cached until the first live snapshot (`tax.go:84-86`).
- Serialisation: `closeMu` (year close/reopen) and `draftMu` (Koinly draft) (`server.go:82-85`).
- Refresh intervals: `New` applies the saved Settings intervals (balances and prices, overriding `refreshMinutes` and `priceMinutes`) to the `Refresher` at start (`internal/server/server.go:97-99`); `applyIntervals` does it again after a settings change, a database restore and a reset (`internal/server/settings_handlers.go:51-74`, [refresh-balances](../../processes/refresh-balances.md)). The header chips read them through `Refresher.RefreshEvery` and `PriceEvery` (`internal/server/handlers.go:88`, `:112`), a source's retry note through `RefreshEvery` (`:371`), and `/healthz` judges freshness by 3 `RefreshEvery` intervals (`internal/server/guard.go:81`).
- Static: `staticHandler` sets `immutable` 1-year cache when `?v=` is present, else 1 h (`server.go:200-212`).

## Connected to

- **owns:** the cost cache (`taxCache`), the render helpers, `guard`/`basicAuth`.
- **owned-by:** `main.go` `serve`, which builds `Deps`.
- **joins:** [page-model](page-model.md) (handlers build `templates.*Page`), [templ-components](templ-components.md) (they render), [static-assets](static-assets.md) (`web/static` FS), [refresh-balances](../../processes/refresh-balances.md), [sync-activity](../../processes/sync-activity.md), [serve-page](../../processes/serve-page.md), [compute-taxes](../../processes/compute-taxes.md), [backup-restore](../../processes/backup-restore.md).
- **looks-like-but-is-not:** [demo-fixtures](demo-fixtures.md) (`cmd/uipreview` re-implements a few endpoints from fixtures, it does not use this package); `portfolio.Broker` (the pub/sub, not the HTTP stream).

## If you change this

- **Hits:** adding a route → `server.go` block plus the owning `*_handlers.go`, and a link/`@get`/`@post` in the matching `.templ` ([templ-components](templ-components.md)); adding a `Deps` field → `main.go:513` and test builders in `server_test.go`; changing `guard` → every `@post` in the templates (they rely on the Datastar header); changing `render` → the `navRegions` contract in `layout.templ`; touching what feeds tax lots → call `invalidateTax()`.
- **Does not hit:** `web/static` (served, not owned here); `internal/portfolio` Refresher logic; [demo-fixtures](demo-fixtures.md) unless a template model changes.

## Surfaces

| Surface | Role |
|---|---|
| Browser (Datastar pages) | GET full pages and SSE patches; POST mutations with `Datastar-Request` |
| `healthcheck` subcommand (`main.go:765-772`) | reads `/healthz` (200 only within 3 `Refresher.RefreshEvery` intervals, which follow Settings, `guard.go:71-101`) |
| `main.go` serve | builds `Deps`, owns the `http.Server` |
| `internal/server/*_test.go` | build `Server` from fakes via `Deps` |

## See

- Source: `internal/server/server.go`
