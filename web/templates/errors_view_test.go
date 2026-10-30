package templates

import (
	"strings"
	"testing"
	"time"
)

func TestExplainError(t *testing.T) {
	const every = 15 * time.Minute
	tests := []struct {
		name, raw, typ string
		public         bool
		text, fix      string // substrings; fix "" means no fix
		raw2           bool   // the raw error is kept for Details
	}{
		{"solana timeout on the public RPC", "context deadline exceeded", "solana", true,
			"Timed out: the Solana RPC was too slow to answer. Anything fetched before that is kept. CryptKeeper tries again every 15 min.",
			"A free Helius RPC URL in config.yaml (solana.rpcUrl)", true},
		{"solana timeout on a private RPC", `Post "https://rpc.example/": context deadline exceeded`, "solana", false,
			"Timed out", "", true},
		{"http client timeout", "Get \"https://mempool.space/api/address/bc1q\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)", "bitcoin", true,
			"Timed out: the Esplora server", "bitcoin.esploraUrl", true},
		{"429", "https://api.mainnet-beta.solana.com: HTTP 429: Too many requests", "solana", true,
			"Rate limited: the Solana RPC asked CryptKeeper to slow down.", "Helius", true},
		{"kraken rate limit", "kraken: rate limited: EAPI:Rate limit exceeded", "kraken", false,
			"Rate limited: Kraken asked CryptKeeper to slow down.", "", true},
		{"kraken bad key", "kraken: EAPI:Invalid key", "kraken", false,
			"Kraken rejected the API key.", "Query Funds and Query Ledger Entries", true},
		{"coinbase 401", "https://api.coinbase.com/v2/accounts: HTTP 401: Unauthorized", "coinbase", false,
			"Coinbase rejected the API key.", "View permission", true},
		{"public RPC blocks the IP", `getBalance: https://api.mainnet-beta.solana.com: HTTP 403: {"jsonrpc":"2.0","error":{"code": 403,"message":"Your IP or provider is blocked from this endpoint"}, "id": 1 }`, "solana", true,
			"The Solana RPC is blocking this machine's IP address. CryptKeeper tries again every 15 min.", "A free Helius RPC URL in config.yaml (solana.rpcUrl)", true},
		{"403 from a keyless public server", "https://mempool.space/api/address/bc1qexample: HTTP 403: Forbidden", "bitcoin", true,
			"The Esplora server is blocking this machine's IP address.", "bitcoin.esploraUrl", true},
		{"private RPC blocks the IP", "https://rpc.example/: HTTP 403: blocked", "solana", false,
			"The Solana RPC is blocking this machine's IP address.", "VPN", true},
		{"private RPC bad key", "https://rpc.example/: HTTP 401: Unauthorized", "solana", false,
			"The Solana RPC rejected the API key.", "config.yaml (solana.rpcUrl)", true},
		{"dns", `Get "https://mempool.space/api": dial tcp: lookup mempool.space: no such host`, "bitcoin", true,
			"Couldn't reach the Esplora server.", "internet connection", true},
		{"server error", "https://api.coinbase.com/v2/accounts: HTTP 503: unavailable", "coinbase", false,
			"Coinbase had a problem on its side (HTTP 503).", "", true},
		{"panic", "panic: assignment to entry in nil map", "solana", false,
			"CryptKeeper hit an internal error", "", true},
		{"missing secret", "needs its API key and secret: edit this source to add them", "kraken", false,
			"Needs its API key and secret.", "Edit this source to add them.", false},
		{"unknown", "decode ledger: unexpected EOF", "kraken", false,
			"The last attempt failed. CryptKeeper tries again every 15 min.", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ExplainError(tt.raw, tt.typ, tt.public, every)
			if !strings.Contains(v.Text, tt.text) {
				t.Errorf("text = %q, want it to contain %q", v.Text, tt.text)
			}
			if (tt.fix == "") != (v.Fix == "") || !strings.Contains(v.Fix, tt.fix) {
				t.Errorf("fix = %q, want %q", v.Fix, tt.fix)
			}
			if tt.raw2 != (v.Raw == tt.raw) {
				t.Errorf("raw = %q, want kept: %v", v.Raw, tt.raw2)
			}
			if strings.Contains(v.Text, "deadline") || strings.Contains(v.Text, "EAPI") {
				t.Errorf("jargon in the text: %q", v.Text)
			}
		})
	}
	if v := ExplainError("  ", "solana", true, every); v != (ErrorView{}) {
		t.Errorf("no error should explain to nothing, got %+v", v)
	}
	for _, c := range []struct {
		every time.Duration
		want  string
	}{{0, "on the next sync"}, {5 * time.Minute, "every 5 min"}, {time.Hour, "every hour"}, {3 * time.Hour, "every 3 hours"}, {90 * time.Minute, "every 90 min"}} {
		if got := retryText(c.every); !strings.Contains(got, c.want) {
			t.Errorf("retryText(%v) = %q, want %q", c.every, got, c.want)
		}
	}
}
