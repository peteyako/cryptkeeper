---
type: process
universe: live
status: verified
verified: 2026-10-20 @ 936c434
consumes: [server, page-model, templ-components, snapshot, config, static-assets]
produces: [page-model]
---

# serve-page

Take one HTTP request through the router and its guards, build a view model from current state, and answer with a full document, a Datastar SSE patch of the page regions, or an SSE patch for a form post.

## Input → Movement → Output

Input: a request (browser load, Datastar `@get` navigation, or Datastar `@post`) plus Datastar signals (`pf`, `range`). Movement: chi routes it through `securityHeaders`, then (for app routes) `basicAuth` and `guard`; the handler reads the latest snapshot and stores, builds a `templates.*Page` model, and renders a templ component that picks its shape from the request. Output: a full HTML document for a browser load; one SSE event morphing `#page-title`, `#sidebar`, `#main`, `#bottomnav` for in-app navigation; or SSE patches of specific elements for form posts.

## Why this shape

- Pages never wait on APIs: handlers read `Refresher.Latest()` and the local stores, so a page loads while sources are slow (see [refresh-balances](refresh-balances.md)).
- Same URL, two shapes: a Datastar GET gets regions, a browser GET gets the document, so deep links, reload and open-in-new-tab all work while ordinary clicks keep the CSS, fonts and the `/stream` connection.
- The guard demands `Datastar-Request: true` on every POST: with no login on a LAN, a hostile web page cannot post to the server because that custom header forces a CORS preflight the server never approves.
- A navigation always answers 200: Datastar retries other statuses as failures, so 404 pages are full-load only.
- `<body>` holds the signals and is never patched, so client state (portfolio, range, hide-amounts) survives navigation.

## Steps

1. `serve` builds the router with `server.New(Deps)` and listens: `main.go:513-527`.
2. `New` installs `Recoverer` and `securityHeaders` globally; `/healthz`, the manifest and `/static/icons/*` sit outside auth: `internal/server/server.go:100-106`, `:224-232`.
3. All other routes sit in a group using `basicAuth` (active only when a password is configured; constant-time compare) then `guard`: `internal/server/server.go:108-110`, `internal/server/guard.go:45-62`.
4. `guard` lets GET/HEAD through; any other method needs `Datastar-Request: true` and, when the browser sends them, a same-host `Origin` and a `Sec-Fetch-Site` of `same-origin` or `none`: `internal/server/guard.go:19-42`.
5. The route table maps each path to a handler: `internal/server/server.go:112-194`.
6. The overview handler takes `pf` from the URL (`/p/{pf}`) and `range` from the query, or from the `range` signal on a navigation, with defaults `all` and `1W`, and renders a 404 page for an unknown portfolio; the patch endpoints (`/view`, `/chart`, `/stream`) read both as signals: `internal/server/handlers.go:21-41`, `:227-238`.
7. It builds the page model: `shell` gives title, active nav item, portfolios list and auth user; `overviewPage` adds the filtered snapshot, sync chip, P&L cells and chart series: `internal/server/handlers.go:43-60`, `:143-174`. Snapshot access is `latest()`, empty before the first refresh: `internal/server/handlers.go:70-76`.
8. `render` -> `renderStatus` decides the shape from `isNav`: a Datastar GET: `internal/server/handlers.go:194-225`.
9. Navigation: the context is marked with `templates.WithNav` and `PatchElementTempl` writes one SSE event: `internal/server/handlers.go:209-217`, `web/templates/view.go:596-601`. `Layout` then emits only `navRegions`: title, sidebar, `<main id="main">`, bottom bar. A page in the chrome-free add-source shell instead patches an empty `#main` that runs `location.reload()`: `web/templates/layout.templ:40-62`.
10. Full load: headers `Cache-Control: no-store`, status code, and `fullPage` renders the document with head, assets, `<body>` signals and the app shell: `internal/server/handlers.go:219-224`, `web/templates/layout.templ:64-110`.
11. In the browser, an in-app link's `navClick` pushes the URL and `@get`s it (modified clicks fall through to the browser); back/forward goes through `navPop`: `web/templates/js.go:423-440`.
12. Form posts answer with SSE patches of a region, a redirect or an error block, never a document, e.g. `alertSave`: `internal/server/alerts_handlers.go:241-253`. Multipart and urlencoded bodies share `parseBody`: `internal/server/source_handlers.go:219-224`.
13. Live regions: the overview page includes a hidden `LiveStream` that opens `/stream` (see refresh-balances step 15): `web/templates/overview.templ:15-18`.

## If you change this

- **Hits:** [server](../objects/web/server.md) (route table, guards), [page-model](../objects/web/page-model.md) structs the handler fills, [templ-components](../objects/web/templ-components.md) (element ids that patches target, such as `#main`, `#dashboard`), the Datastar signal names in `web/templates/js.go`. Any new POST route is covered by the guard automatically; any new full-page route must render through `s.render` or navigation breaks.
- **Does not hit:** the data stores or sources; the refresher (pages only read it). Generated `*_templ.go` files are rebuilt by `task generate`, never edited.

## Surfaces

| Surface | Role |
|---|---|
| Browser load | full HTML document |
| Datastar `@get` of a page URL | SSE patch of title, sidebar, main, bottom nav |
| Datastar `@post` | guarded mutation; SSE patch, redirect or error |
| `/stream`, `/activity/sync` | long-lived SSE |
| `/healthz`, manifest, icons | unauthenticated |

## See

- Objects: [server](../objects/web/server.md), [page-model](../objects/web/page-model.md), [templ-components](../objects/web/templ-components.md), [static-assets](../objects/web/static-assets.md), [snapshot](../objects/valuation/snapshot.md), [config](../objects/storage/config.md)
- Source: `internal/server/server.go`, `internal/server/handlers.go`, `internal/server/guard.go`, `web/templates/layout.templ`
