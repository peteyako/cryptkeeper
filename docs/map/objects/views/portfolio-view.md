---
type: object
cluster: views
universe: live
status: verified
verified: 2026-10-20 @ 73e5bf4
entity: internal/portfolios/portfolios.go
---

# Portfolio view

A user's named portfolio (`portfolios.Portfolio`, stored in `data/portfolios.yaml`): a filter over wallets and assets, plus a watch list, that turns the full snapshot into one portfolio's snapshot.

## Why this shape

A portfolio is a filter, not a copy of data: a holding belongs to it iff its wallet and its asset are both selected (`internal/portfolios/portfolios.go:107-108`). The filtered snapshot is rebuilt with the same `portfolio.FromWallets` as the full one, so totals, shares and stale flags cannot drift (`internal/portfolios/filter.go:30`). The YAML is hand-edited, so parsing is strict, tells a missing key (= all) from an explicit `[]` (= none) from a null, and a bad edit never replaces the last good copy (`portfolios.go:58-60`, `:117-148`, `internal/portfolios/store.go:31-35`).

## Shape

- `Portfolio{ID, Name, Wallets Selector, Assets Selector, Watch []string}` (`portfolios.go:109-115`); `File{Version, Portfolios, Alerts}` with `Version = 1` (`:24-26`, `:187-194`).
- `Selector{All bool, IDs []string}`: YAML `all` or a list; zero value = none; `[]` is written for none; omitted key defaults to all (`:41-56`, `:58-105`, `:134-145`).
- Strict decode: unknown fields are errors with line numbers (`yamlfile.Decode` uses `KnownFields(true)`, `internal/yamlfile/yamlfile.go:23`; `Parse` at `portfolios.go:223-240`). IDs match `^[a-z0-9][a-z0-9-]{0,39}$`, are unique, never `all` (reserved for "All holdings"); names 1-60 chars (`:24-34`, `:249-285`).
- Normalization upper-cases asset symbols via `manual.NormalizeAsset` (keeps `spl:<mint>`), dedupes, nils empty lists (`:157-184`).
- `Apply(p, snap)`: keep wallets p selects; filter `Rows` and `HiddenRows` by `Include`; drop a wallet left with no rows unless p lists it explicitly or p is unfiltered; recompute via `FromWallets` (`filter.go:9-31`). `Include(p)` is also the `History.Series` filter (`filter.go:44-50`).
- Watch list: extra assets shown with a live price only, never valued in the total (`portfolios.go:107-108`; priced in `internal/server/portfolio_handlers.go:80-110`).
- `Store`: mtime check on every read picks up hand edits; an unparseable edit keeps the cached copy and sets `LoadError`; writes validate the whole file, back up the old one (30 kept) and replace atomically, caching only after success (`internal/portfolios/store.go:17-18`, `:31-36`, `:101-141`, `:162-186`). `OnChange` listeners fire on writes and detected edits (`:76-100`).
- `Upsert` (empty ID derives a slug, `-2`, `-3`), `Delete`, `RemoveWallet` (when a source is deleted), `Export(id|"")`, `Import(merge|replace)` (parse first; empty input rejected), `Backups`, `Restore(name)` (a restore is itself backed up) (`store.go:189-380`, `portfolios.go:296-339`).
- `WithAlerts` appends the alert rules to an exported document; `data/portfolios.yaml` itself never stores them (`portfolios.go:190-194`, `:341-352`).
- Resolution in the server: `""`/`all` = whole snapshot; unknown id falls back to All holdings with `known=false` (`internal/server/portfolio_handlers.go:37-60`). Routes `/portfolios...` (`internal/server/server.go:138-147`). Tax view reuses `Apply` with a wallet-only selector (`internal/server/tax.go:197`).
- Wired at `main.go:182`; the alert engine uses it for `portfolio_move` (`main.go:404-425`).

Citations: `internal/portfolios/portfolios.go:109`, `internal/portfolios/filter.go:15`, `internal/portfolios/store.go:36`

## Connected to

- **owns:** `data/portfolios.yaml` and its backups copies ([yaml-store](../storage/yaml-store.md)).
- **owned-by:** `main.go` builds the `Store`; the server holds it (`internal/server/server.go:46`).
- **joins:** [snapshot](../valuation/snapshot.md) (input and output of `Apply`); [value-history](../valuation/value-history.md) (`Include`); [alert-rule](alert-rule.md) (`portfolio_move` targets an id; `WithAlerts` export); [source-def](../sources/source-def.md) (wallet ids; `RemoveWallet` on delete); [backup-file](../storage/backup-file.md) (user export/import); [tax-lot](../ledger/tax-lot.md) (`tax.yaml` `scope` names a portfolio whose wallets the tax covers).
- **looks-like-but-is-not:** the `portfolio` package (aggregation, [snapshot](../valuation/snapshot.md)); the `All holdings` view (reserved id `all`, not stored); a wallet/source; the automatic timestamped copies in `data/backups/`.

## If you change this

- **Hits:** `Apply`/`Include` callers (server page resolution, tax view, alerts valuer in `main.go`); the tax scope: the wallets of the portfolio `tax.yaml` `scope` names decide what the open year computes (`internal/server/tax.go:161-182`), so editing that portfolio changes tax results; portfolio manager/edit pages ([page-model](../web/page-model.md)); source delete flow (`RemoveWallet`, `main.go:593`, `:759`, `internal/server/portfolio_handlers.go:484`); ids referenced elsewhere: deleting or renaming a portfolio leaves `portfolio_move` rules and the tax `scope` pointing at nothing, silently (the rule is skipped, `main.go:406-409`; tax falls back to every wallet), as nothing cross-validates at save time; alert export/import ([alert-rule](alert-rule.md)); testdata under `internal/portfolios/testdata`.
- **Does not hit:** wallet/source definitions in `sources.yaml`; the Snapshot types; the contents of `alerts.yaml` or `tax.yaml`.

## Surfaces

| Surface | Role |
|---|---|
| User editing `data/portfolios.yaml` | writes; picked up by mtime, rejected edits shown via `LoadError` |
| Server `/portfolios*` handlers | list, upsert, delete, import, export, restore |
| Page handlers | `resolvePortfolio` per request |
| Alerts engine closures | `Apply`, `Include` |
| Tests | `internal/portfolios/*_test.go` |

## See

- Source: `internal/portfolios/portfolios.go`
