---
type: object
cluster: sources
universe: live
status: verified
verified: 2026-10-20 @ 0a7dfb9
entity: internal/sources/bitcoin/bitcoin.go
---

# Bitcoin source (`bitcoin.Source`)

A read-only Bitcoin wallet (source type `bitcoin`) built from an xpub/ypub/zpub, address, output descriptor or wallet file, scanned through an Esplora HTTP API.

## Why this shape

Bitcoin has no account balance call, so the source derives addresses locally and asks a public Esplora server about each, which is slow and rate limited. Nearly everything here is about asking less and surviving refusal: a gap-limit scan, light rescans between full ones, a scan saved across restarts, request pacing, fallback servers and an optional Blockstream key. Only public keys are ever accepted, and extended keys are never logged (`internal/sources/bitcoin/bitcoin.go:5`).

## Shape

- Input: `Config` takes exactly one of `Key`, `Descriptor`, `WalletFile` (`New` errors otherwise) (`internal/sources/bitcoin/bitcoin.go:25-41,114-129`). Leftover wiring: the factory never sets `WalletFile`; a wallet file is converted to a descriptor at upload time by `DescriptorFromWalletFile`, stored in `Def.Descriptor`, with `Def.ImportedVia = "wallet file"` (`main.go:274-279`, `internal/server/source_handlers.go:151-163`, `internal/sources/bitcoin/walletfile.go:19`).
- Key kinds: `Detect` classifies input for the form; private keys (WIF, `xprv`), testnet and multisig cosigner keys are refused (`bitcoin.go:262-290`, `internal/sources/bitcoin/derive.go:113-117`). Ledger exports are parsed by `ParseLedgerExport` (`internal/sources/bitcoin/ledger.go:33`).
- Script type: optional `ScriptType` (`p2pkh|p2sh-p2wpkh|p2wpkh|p2tr`) overrides an extended key (`derive.go:32-45`); otherwise ypub/zpub imply it and a plain `xpub` is read as Legacy, with `autoKey` set so an empty scan emits a hint naming the script type that has history (`bitcoin.go:92-94,227-234`, `internal/sources/bitcoin/esplora.go:627`).
- Descriptors: single-sig and multisig (`wsh/sh/sh(wsh)` sorted and plain multi), multipath `<0;1>`, or a receive+change pair; bare multi is rejected (`internal/sources/bitcoin/descriptor.go:33-43`, `bitcoin.go:303-342`). Chains: `[0]` receive, `[1]` change (`bitcoin.go:65-69`).
- Scan: gap limit `GapLimit` (default 20, from `config.yaml` `bitcoin.gapLimit`) per chain, cap 10,000 addresses (`esplora.go:426-460`, `bitcoin.go:44-45`). A full scan every 6h; in between only used addresses plus 5 lookahead are rechecked; an unchanged chain tip within 15 min reuses the last scan; one scan at a time via singleflight, run detached up to 10 min (`esplora.go:26-34,334-388`). `recentScan` reuses a scan under 10 min old for Addresses/UTXOs/next receive address/Activity (`esplora.go:24,463`).
- Saved scan: `ScanStore` (impl `kvScans` in `main.go`, kv key `bitcoin.scan.<walletID>` = `server.BitcoinScanKVPrefix`) keeps sats/tx counts per index, never addresses, tagged with a fingerprint of the first addresses plus gap; a changed key or gap discards it (`internal/sources/bitcoin/scanstore.go:15-99`, `internal/server/settings_handlers.go:442-443`).
- Servers: `EsploraURL` (default mempool.space/api) then `FallbackURLs`; unset fallback defaults to the other public server, none for a private node, `"off"` disables (`internal/config/config.go:64-96`). A server that does not answer (error, timeout, 429, 5xx) goes to the back for 10 min; non-last servers get one 8s attempt (`esplora.go:179-262`, constants `:43-46`).
- Pacing/429: public hosts get `httpx.SetPace` at 1 request/s across all wallets; private hosts (loopback, LAN, `.local`, `.onion`) are unpaced. A 429 pauses that server for max(Retry-After, 5 min) for every wallet, via package-level `serverPauses` (`bitcoin.go:176-180`, `esplora.go:37-39,94-112,256-293`).
- Blockstream key: `BlockstreamAuth` (client id+secret to OAuth token) is shared by all wallets; used only when the configured host is `mempool.space` or `blockstream.info`, never for a private node; requests go to `enterprise.blockstream.info` first, public servers stand by (`internal/sources/bitcoin/blockstream.go:20-29`, `bitcoin.go:159-161`, `esplora.go:144-176`). Key source: env `BLOCKSTREAM_CLIENT_ID/SECRET`, else sealed kv `bitcoin.blockstreamKey` saved from Settings (`main.go:799-815`, `internal/server/settings_handlers.go:205-298`).
- Fetch returns one `BTC` holding with exact sats string, plus Meta rows (Imported via, Script, Derivation, Addresses used, Node, Block height) and warnings for fallback, key trouble, truncation (`esplora.go:486-547`).
- Optional interfaces: `AddressLister`, `UTXOLister`, `ReceiveAddresser` (`bitcoin.go:58-63`), `AddressDeriver` (`bitcoin.go:354`, used by Koinly wallet matching, `internal/server/koinly_handlers.go:395`), and `core.ActivitySource` (`internal/sources/bitcoin/activity.go:17`). Activity: per-address cursors, pending-tx recheck, backfill to `Since`, request budget 30 (own default and the syncer's), opaque `State` version 1 (`activity.go:19-107`); entries become `core.Tx` ([activity-entry](../ledger/activity-entry.md)).

## Connected to

- **owns:** the BTC `core.Holding` for one wallet; the saved-scan kv row; the activity cursor `State`.
- **owned-by:** [source-def](source-def.md) (`Def.Key/Descriptor/ScriptType/ImportedVia`); implements [source-contract](source-contract.md).
- **joins:** [config](../storage/config.md) (`bitcoin.esploraUrl`, `esploraFallbackUrls`, `esploraFallbackUrl`, `gapLimit`); [database](../storage/database.md) kv; [secret-key](../storage/secret-key.md) (Blockstream key); [price](../valuation/price.md) (`BTC`).
- **looks-like-but-is-not:** `solana-source` (account RPC, no gap scan); the Blockstream *key* (Settings, shared) vs the Esplora *URL* (config).

## If you change this

- **Hits:** Esplora URL/fallback rules: `internal/config/config.go` and `esplora.go`. Scan state layout: `scanstore.go` (bump or discard via fingerprint); activity `State`: bump `activityStateVersion`. Form fields: `internal/server/source_handlers.go` and Settings Bitcoin handler.
- **Does not hit:** `internal/httpx` retry logic generally; the Solana RPC pacing; `data/sources.yaml` secret sealing (bitcoin has no secret fields).

## Surfaces

| Surface | Role |
|---|---|
| Refresher | `Fetch` per cycle (light scan) |
| Activity syncer | `Activity` |
| Server wallet detail | Addresses, UTXOs, next receive address, Connection panel |
| Settings handlers | Blockstream key check/save; saved scan deleted when the wallet is deleted or dropped by a replace-import (`cleanupSource`, `internal/server/settings_handlers.go:445-450`) |
| Koinly import | derived addresses for wallet matching (`AddressDeriver`) |
| User via YAML | `key`, `descriptor`, `scriptType` in `data/sources.yaml`; gap/URLs in `config.yaml` |
| Tests | `internal/sources/bitcoin/*_test.go` (pace, scanbudget, fallback, descriptor) |

## See

- Source: `internal/sources/bitcoin/bitcoin.go`
