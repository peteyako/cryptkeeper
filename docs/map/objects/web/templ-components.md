---
type: object
cluster: web
universe: live
status: verified
verified: 2026-10-19 @ 5d02073
entity: web/templates/layout.templ
---

# Templ components

The `.templ` sources in `web/templates/`: the app shell (`layout.templ`), shared components (`components.templ`, `icons.templ`) and one file per page, plus the Datastar expression builders in `js.go`.

## Why this shape

Interactivity is declared in attributes (`data-signals`, `data-on:click`, `@get`/`@post`) and answered by SSE patches, so there is no hand-written JS beyond the vendored Datastar and a small chart hover script. A handler re-renders a templ component and Datastar morphs the element with the same `id`, so every patchable region is a component whose root carries a stable id. Server values placed in expressions must pass through `JSString`, because an unescaped quote is a silent client-side syntax error (`web/templates/js.go:8-12`); all expressions are built in `js.go` precisely so they can be tested.

## Shape

- Generated code: `*_templ.go` is gitignored, produced by `task generate` (`Taskfile.yml:9-14`, `go tool templ generate`), `.air.toml:13` and the Docker build (`Dockerfile:7`). Never cite or edit it; it may be missing or stale in a checkout.
- Shell (`web/templates/layout.templ`): `Layout` (`:30`) wraps a page body; `layoutFocus` (`:36`) is the chrome-free sheet used by the add-source flow (`sources.templ:29,229`). `page` (`:40`) picks by `IsNav(ctx)`: `navRegions` (`:51`) sends only `<title id="page-title">`, `#sidebar`, `#main`, `#bottomnav`; `fullPage` (`:64`) is the whole document (head links `asset("tokens.css")`, `app.css`, `datastar.js`, `chart.js`). A bare (focus) page cannot be patched into the shell, so it answers `location.reload()` (`:53`).
- Nav: `sideNav`/`bottomNav` lists (`layout.templ:5-19`), `portfolioLink` (`:164`) switches portfolio in place on the overview but navigates elsewhere. `<body>` carries `data-signals={bodySignals(s)}` and is never patched (`:87-93`; signals `hide, theme, range, pf, q, showHidden, showPnl, cashOpen`, `js.go:74-78`). `LiveStream` (`:210`) is the hidden `#live` element that opens `/stream`.
- Shared components: `SyncChip` (`#sync-status`, `components.templ:11`), `Fig*` privacy-masked figures (`:21-37`), `Disc/AssetMark`, `Notice`, `StatusChip`, `SourceChip`, `AccountCard`, `RangeControl`, `HideButton`, `RefreshButton` (`:39-160`); `Icon(name, size)` in `icons.templ:8`.
- Page files, each exposing an `XxxFull` plus fragments: overview (`OverviewFull:11`, `Dashboard:22` root `#dashboard`, `ValueCard:69` root `#value-card`), wallets (`WalletsFull`, `WalletFull:45`, `WalletTab:253` root `#wallet-tab`), sources (`SourcePickFull`, `SourceFormFull`, `#source-form`, `#form-errors`, `#test-result`), portfolios, activity, reports (`tax.templ` and `review.templ` are Reports fragments with no `Full`), koinly, alerts, settings (incl. `ErrorFull:150`), coin. `taxreport.templ`, `form8949.templ` and `print.templ` are standalone print documents (`PrintLayout`, no app shell) rendered directly by `internal/server/taxdownloads.go:112,199`.
- SSE patch targets (root id of the fragment a handler sends): `#dashboard`, `#value-card`, `#sync-status`, `#wallet-tab`, `#activity-list`/`#activity-more`/`#activity-sync`/`#activity-new` (`activity.templ:89-300`; `ActivityRows` has no root and is appended into `#activity-rows` by selector, `internal/server/activity_handlers.go:226`), `#alert-rules`, `#alert-recent`, `#alert-test`, `#form-errors`, `#test-result`, `#portfolio-list`, `#portfolio-io` (wraps `#import-result`), `#backup-import-result`, `#csv-result`, `#pnl`, `#tax-section`, `#review-panel`, `#koinly-wizard`, `#close-card` (`review.templ:218`), `#settings-display`, `#settings-prices`, `#settings-bitcoin`, `#coin-chart`. The set the server really sends is the `PatchElementTempl(templates.X)` calls in `internal/server/*.go`; renaming a root id breaks that handler silently.
- `js.go` constants and builders: `liveFilter`/`liveInit` (`@get('/stream', ...)`, `:54,93`), `syncInit`, `refreshClick`, `backfillClick`, `testClick`, `navClick`/`navPop` (push URL then `@get`, `:404-421`), range/theme/hide/pf click+class builders, `actSignals`/`actFilter` (`:283-290`), `bodyInit`/`bodyEffect` (persist `cryptkeeper.*` localStorage keys with `cryptkeep.*` fallback, `:82-88`). Signal filters (`filterSignals: {include: ...}`) keep requests small and are what handlers read via `datastar.ReadSignals`.
- Tests: `js_test.go` (`TestJSString:64`, `TestExpressionsAreValidJavaScript:155` runs every expression through Node, `TestBodyEffectBehaviour:251`, `TestExpressionBuilders:306`); `templates_test.go:50` `allPages` renders every page and fragment from `internal/demo` and is reused by `static_test.go`.

## Connected to

- **owns:** the `.templ` markup, ids, `js.go` expressions, `navRegions`.
- **owned-by:** package `web/templates`.
- **joins:** [page-model](page-model.md) (props), [server](server.md) (renders and patches), [static-assets](static-assets.md) (every class used must have a rule in `app.css`), [demo-fixtures](demo-fixtures.md).
- **looks-like-but-is-not:** `*_templ.go` (generated); `web/static/datastar.js` (vendored runtime, not edited).

## If you change this

- **Hits:** renaming a root id or fragment → the `PatchElementTempl` caller in `internal/server`, `uipreview` stubs in `cmd/uipreview`, and `templates_test.go`; adding a class → `web/static/app.css` (test fails otherwise); adding a signal or expression → `js.go` + `js_test.go` and the handler's `ReadSignals` struct; adding a page → `XxxFull`, a `*Page` model, a route, a nav entry if top-level, and a demo builder.
- **Does not hit:** `tokens.css`, except the token names `format.go` puts in inline styles (allocation colors, `Monogram`; see [page-model](page-model.md)); the domain packages.

## Surfaces

| Surface | Role |
|---|---|
| `internal/server` handlers | render full pages and fragments |
| Browser Datastar runtime | reads `data-*` attributes, issues `@get/@post`, morphs by id |
| `cmd/uipreview` | renders the same components from fixtures |
| `task generate`, air, Docker build | regenerate `*_templ.go` |

## See

- Source: `web/templates/layout.templ`
