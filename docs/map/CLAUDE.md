# CryptKeeper system map

A walkable graph of the code: what the nouns are, how they move, and what else moves when you
touch one. The code is the source of truth; every card cites `path:line`.

## Where things live

| Folder | What it holds |
|---|---|
| `objects/` | one card per noun, in six clusters; the list is [objects/_index.md](objects/_index.md) |
| `processes/` | one card per movement that runs; the list is [processes/_index.md](processes/_index.md) |
| `effects/` | the change-impact index: [effects/CONTEXT.md](effects/CONTEXT.md) |
| `_meta/` | [schema.md](_meta/schema.md) (card rules) and `build.sh` (`task map`) |
| `_templates/` | blank object and process cards |

## Clusters

| Cluster | Ask it about |
|---|---|
| `sources` | a connected wallet, exchange or account: definitions, the source contract, each type |
| `valuation` | prices, the priced snapshot, value history |
| `views` | portfolios (named views) and alert rules |
| `ledger` | activity, cost basis, tax lots, tax reports, Koinly import, CSV export |
| `storage` | the database, YAML stores, config, the secret key, backup files |
| `web` | server and routes, page models, templ components, static assets, demo fixtures |

## Route by question

| If | Go to | Then stop at |
|---|---|---|
| "what is X?" | [objects/_index.md](objects/_index.md) → one card | the lines it cites |
| "what breaks if I change X?" | [effects/CONTEXT.md](effects/CONTEXT.md) | the cards it names |
| "how does a refresh, sync, tax run or page load work?" | [processes/_index.md](processes/_index.md) → one card | its steps |
| a word seems to mean two things | [CONTEXT.md](CONTEXT.md), *Name collisions* | that table |
| adding or renaming a card | [_meta/schema.md](_meta/schema.md), copy a template | `task map` |

## The one rule

Open one card, then the code it cites. Never load a whole cluster.
