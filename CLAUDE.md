# CryptKeeper

Self-hosted, read-only monitor for crypto wallets, exchanges and brokerage accounts: Go +
templ + Datastar, SQLite plus hand-editable YAML in `data/`. The Go module and binary are both
`cryptkeeper`; only the repo folder is still `crypto-portfolio`. User docs: [README.md](README.md).

## Where things live

| Path | Holds |
|---|---|
| `main.go` | setup (config, secret key, database, sources), `serve`, and the CLI commands |
| `internal/` | one package per noun: sources, prices, portfolio, activity, tax, alerts, store… |
| `web/templates/` | templ pages and their Go view models; `web/static/` holds tokens, CSS, datastar.js |
| `cmd/uipreview/` | every screen rendered from the demo fixtures in `internal/demo` |
| `docs/map/` | **system map**: a card per noun and per process, and what a change hits |
| `docs/superpowers/` | feature specs and milestone plans; `docs/plan.md` is the original plan |
| `data/`, `.env`, `config.yaml` | the user's real state and secrets: gitignored, never copied anywhere |

## Route by task

| If you are… | Open |
|---|---|
| changing or explaining code | [docs/map/CLAUDE.md](docs/map/CLAUDE.md), then one card |
| about to edit something other code depends on | [docs/map/effects/CONTEXT.md](docs/map/effects/CONTEXT.md) |
| designing a new feature | the specs in `docs/superpowers/specs/`; put a new spec there |
| touching the UI | [docs/map/objects/web/templ-components.md](docs/map/objects/web/templ-components.md) |

## Commands (go-task; `task --list` for all)

| Task | Does |
|---|---|
| `task dev` | air: rebuild and restart on change (app :8787, reloading proxy :8788) |
| `task preview` | every screen with demo data on :8790 |
| `task test` / `task check` | race tests / generate + vet + gofmt + tests |
| `task generate` | `*_templ.go` from the `.templ` files |
| `task map` | rebuild the map's indexes and the `AGENTS.md` twins |

## Rules

- Read-only by design: only public keys, addresses and view-only API keys. Keep refusing seed
  phrases and private keys.
- `*_templ.go` is generated and gitignored. Edit `.templ`, never the generated file; builds,
  tests and `task dev` regenerate it.
- Say CryptKeeper in user-facing text. "vault" is only the crypto meaning (Coinbase vault
  accounts, pool vaults). Older names survive only as legacy fallbacks.
- No personal data in the repo: fixtures and examples use invented addresses, amounts and
  timestamps, never values copied from `data/`, Koinly exports or the database.
- Interactivity is Datastar attributes and SSE patches from Go handlers, not hand-written JS.
- When you change code a map card cites, re-verify that card (or set `status: stale`) and run
  `task map`. `AGENTS.md` files are generated twins of `CLAUDE.md`: edit `CLAUDE.md` only.
