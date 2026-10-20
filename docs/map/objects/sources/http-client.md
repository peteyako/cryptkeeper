---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-20 @ 936c434
entity: internal/httpx/httpx.go
---

# HTTP client (`httpx.Client`)

The one HTTP client every networked source (not manual or brokerage), the price service and the ntfy notifier share: 20 s timeout, JSON helpers, a fixed user agent, retry with backoff, and a process-wide per-host request pacer.

## Why this shape

Public APIs (Esplora, Yahoo, the public Solana RPC) ban an IP that keeps going over their limit, and one `Client` is not the only thing talking to a host: many wallets and several `Client` copies hit the same server. So pacing lives in a package-level map keyed by host, not on the client, and holds for the whole process (`internal/httpx/httpx.go:205-207`). The pacer lets one request go at a time in arrival order and sets `next` to an interval past the moment the request actually went, not a slot handed out in advance. That keeps requests apart after the process was frozen (a sleeping machine) and lets a 429 move `next`, which stops requests already waiting (`httpx.go:209-219,304-311`). One 429 therefore holds every wallet and caller on that host, so they do not meet the same refusal one by one. Callers that run their own server fallback want a 429 back at once instead of sitting in retry: that is `PaceOwn429`.

## Shape

- Client: `HTTP`, `MaxRetries`, `BaseDelay`, `PaceOwn429`; `New()` gives 20 s timeout, 3 retries, 500 ms base (`httpx.go:21-38`). Tests and callers may pass `nil` and get `httpx.New()` (each constructor does).
- `Do` builds the request again per attempt, calls `Wait` for the request host, sets `User-Agent: cryptkeeper/1.0` only if the caller set none, reads at most 32 MiB (`httpx.go:18,59-118`).
- Retry: 429, 5xx and transport errors, backoff `BaseDelay << (attempt-1)`, or `Retry-After` when it is at most a minute (a longer one falls back to the backoff, though the hold below still delays a paced host); other non-2xx return the body and a `StatusError` at once (`httpx.go:61-72,101-111,131-145`). Context cancel ends it.
- 429 hold: every 429 calls `hold(host, Retry-After)`, which pushes `next` out on a paced host (unpaced hosts ignore it). With `PaceOwn429` the 429 is returned immediately with `RetryAfter` set, other failures still retry (`httpx.go:97-103,237-250`).
- `StatusError{Status, Body, URL, RetryAfter}`: the URL drops query and user info because providers put API keys in query strings and error text reaches the UI (`httpx.go:41-55,120-124`). Callers read it with `errors.As`.
- Helpers: `GetJSON`, `PostJSON`, `PostRaw` set `Accept: application/json`, merge caller headers, decode into `out` (nil `out` skips decode) (`httpx.go:147-203`).
- Pacer API: `SetPace(host, interval)` (longest interval wins, 0 removes), `Pace(host)`, `Wait(ctx, key)`. Keys are free-form: Solana paces `host + " " + method` itself (`httpx.go:221-235,252-321`, `internal/sources/solana/rpc.go:55-58`). Cancelled waiters leave the line and pass the head on (`httpx.go:323-341`).
- Derived clients: Bitcoin makes `failFast` (one attempt with an 8 s timeout, no retries, `PaceOwn429`) and `ownPacing` (copy with `PaceOwn429`) from the shared one (`internal/sources/bitcoin/esplora.go:124-137`, `internal/sources/bitcoin/bitcoin.go:146`).
- Construction: `main.go` builds one `httpx.New()` into `app.client` and hands it on (`main.go:90,172`).

Citations: `internal/httpx/httpx.go`, `main.go:172-174,268-285,428,801`

## Connected to

- **owns:** the per-host pacer map and the 429 hold; `StatusError`.
- **owned-by:** `app` in `main.go`.
- **joins:** [bitcoin-source](bitcoin-source.md) (1 req/s on public Esplora hosts, 429 pause per server); [solana-source](solana-source.md) (public RPC host pace plus per-method pace); [solana-defi](solana-defi.md) (host pace only: its own `httpRPC` skips the per-method keys); [coinbase-source](coinbase-source.md) and [kraken-source](kraken-source.md) (request rebuilt and re-signed per attempt); [price](../valuation/price.md) (Yahoo hosts paced 500 ms; CoinGecko 429 handling); [config](../storage/config.md) (URLs decide which hosts are public and so paced).
- **looks-like-but-is-not:** the Bitcoin `serverPauses` (a per-server pause in `esplora.go` that moves a failing server to the back), which sits above this and is separate from the pacer hold.

## If you change this

- **Hits:** every package in Surfaces. Retry or `PaceOwn429` semantics: Bitcoin `esplora.go` fallback and pause logic. `StatusError` shape: every `errors.As(&se)` caller. Pacer ordering: `internal/httpx/httpx_test.go`, `pace_unix_test.go`, and the pace tests in `internal/sources/bitcoin` and `internal/sources/solana`.
- **Does not hit:** `config.yaml` pace values (the intervals are constants in the callers, not config); data files; auth signing (Coinbase JWT, Kraken HMAC, Blockstream token) which callers add as headers.

## Surfaces

| Surface | Role |
|---|---|
| `main.go` | constructs the one client; passes to prices, coinbase, kraken, bitcoin, solana, `alerts.Ntfy`, Blockstream auth |
| `internal/prices` (`prices.go`, `coingecko.go`, `sources.go`, `stocks.go`, `history.go`) | uses `Client`; `SetPace` on Yahoo hosts (`prices.go:141`); reads `StatusError` |
| `internal/sources/bitcoin` (`bitcoin.go`, `esplora.go`, `blockstream.go`, `activity.go`) | `SetPace` on public servers (`bitcoin.go:178`); `failFast`/`ownPacing`; `StatusError` |
| `internal/sources/solana` (`solana.go`, `rpc.go`, `tokens.go`, `rewards.go`) | `SetPace` on host (`solana.go:107`); `Wait` per method (`rpc.go:57-58`) |
| `internal/sources/solana/defi` (`defi.go`, `rpc.go`, `jupiter.go`, `raydium.go`) | `Client` only |
| `internal/sources/coinbase`, `internal/sources/kraken` | `Client`; `StatusError` for auth and rate errors |
| `internal/alerts/ntfy.go` | `Client`; `StatusError` |
| Tests | `internal/httpx/*_test.go` plus fakes in each caller's tests |

## See

- Source: `internal/httpx/httpx.go`
