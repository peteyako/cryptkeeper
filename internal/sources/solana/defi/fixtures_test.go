package defi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/httpx"
)

// Fixtures in testdata/*_accounts.json are real mainnet accounts captured with one
// getMultipleAccounts call each (so pool and positions come from the same slot):
//
//	{"slot": N, "capturedAt": "...", "accounts": {"<pubkey>": {"owner": "<program>", "data": "<base64>"} | null}}
//
// API fixtures (perps_api_positions.json, raydium_lps_api.json, jupiter_price_perps.json) are
// raw responses captured with curl at the same time. See docs/defi-sources.md.

type accountFixture struct {
	Slot       uint64 `json:"slot"`
	CapturedAt string `json:"capturedAt"`
	Accounts   map[string]*struct {
		Owner string `json:"owner"`
		Data  string `json:"data"`
	} `json:"accounts"`
}

func loadAccounts(t *testing.T, names ...string) (map[string][]byte, time.Time) {
	t.Helper()
	out := map[string][]byte{}
	var at time.Time
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		var f accountFixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		at, _ = time.Parse(time.RFC3339, f.CapturedAt)
		for k, v := range f.Accounts {
			if v == nil {
				continue
			}
			d, err := base64.StdEncoding.DecodeString(v.Data)
			if err != nil {
				t.Fatalf("%s %s: %v", name, k, err)
			}
			out[k] = d
		}
	}
	return out, at
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeRPC serves accounts from a map and records calls.
type fakeRPC struct {
	mu       sync.Mutex
	accounts map[string][]byte
	gmaErr   error
	calls    [][]string
}

func newFakeRPC(accounts map[string][]byte) *fakeRPC { return &fakeRPC{accounts: accounts} }

func (f *fakeRPC) GetMultipleAccounts(ctx context.Context, keys []string) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), keys...))
	if f.gmaErr != nil {
		return nil, f.gmaErr
	}
	if len(keys) > maxAccountsPerCall {
		return nil, fmt.Errorf("too many keys: %d", len(keys))
	}
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = f.accounts[k]
	}
	return out, nil
}

func (f *fakeRPC) GetProgramAccounts(context.Context, string, []Filter) ([]KeyedAccount, error) {
	return nil, errors.New("getProgramAccounts not expected")
}

func (f *fakeRPC) requested(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		for _, k := range c {
			if k == key {
				return true
			}
		}
	}
	return false
}

// fastClient is an httpx client without retry delays.
func fastClient() *httpx.Client {
	c := httpx.New()
	c.MaxRetries = 0
	c.BaseDelay = time.Millisecond
	return c
}

// serveJSON serves body for any request, recording request URLs.
func serveJSON(t *testing.T, status int, body []byte, urls *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if urls != nil {
			*urls = append(*urls, r.URL.String())
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func nft(mint string) TokenBalance {
	return TokenBalance{Mint: mint, Amount: big.NewInt(1), Decimals: 0}
}

func bigStr(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big int %q", s)
	}
	return n
}

func findPosition(t *testing.T, ps []Position, id string) Position {
	t.Helper()
	for _, p := range ps {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("position %s not found in %+v", id, ps)
	return Position{}
}
