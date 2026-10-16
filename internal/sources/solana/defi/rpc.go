package defi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"crypto-portfolio/internal/httpx"
)

// NewHTTPRPC returns a minimal JSON-RPC client implementing RPC, for tests and standalone use.
// The production solana source adapts its own client instead. Errors never contain the URL's
// path, query or credentials (RPC URLs often embed an API key).
func NewHTTPRPC(rawURL string, c *httpx.Client) RPC {
	if c == nil {
		c = httpx.New()
	}
	return &httpRPC{url: rawURL, hc: c, scrub: urlScrubber(rawURL)}
}

type httpRPC struct {
	url   string
	hc    *httpx.Client
	scrub *strings.Replacer
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcErr) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcErr         `json:"error"`
}

func (r *httpRPC) call(ctx context.Context, method string, params []any, out any) error {
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	var resp rpcResp
	if err := r.hc.PostJSON(ctx, r.url, nil, req, &resp); err != nil {
		return fmt.Errorf("%s: %s", method, r.scrub.Replace(err.Error()))
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %w", method, resp.Error)
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// rpcAccount is an account in base64 encoding: {"data": ["<base64>", "base64"], …}.
type rpcAccount struct {
	Data []string `json:"data"`
}

func (a *rpcAccount) decode() ([]byte, error) {
	if len(a.Data) < 1 || (len(a.Data) > 1 && a.Data[1] != "base64") {
		return nil, fmt.Errorf("unexpected account data encoding")
	}
	return base64.StdEncoding.DecodeString(a.Data[0])
}

func (r *httpRPC) GetMultipleAccounts(ctx context.Context, pubkeys []string) ([][]byte, error) {
	if len(pubkeys) == 0 {
		return nil, nil
	}
	var res struct {
		Value []*rpcAccount `json:"value"`
	}
	params := []any{pubkeys, map[string]any{"encoding": "base64", "commitment": "confirmed"}}
	if err := r.call(ctx, "getMultipleAccounts", params, &res); err != nil {
		return nil, err
	}
	if len(res.Value) != len(pubkeys) {
		return nil, fmt.Errorf("getMultipleAccounts: %d results for %d keys", len(res.Value), len(pubkeys))
	}
	out := make([][]byte, len(pubkeys))
	for i, a := range res.Value {
		if a == nil {
			continue
		}
		d, err := a.decode()
		if err != nil {
			return nil, fmt.Errorf("getMultipleAccounts %s: %w", pubkeys[i], err)
		}
		if d == nil {
			d = []byte{} // existing account with no data stays distinguishable from missing
		}
		out[i] = d
	}
	return out, nil
}

func (r *httpRPC) GetProgramAccounts(ctx context.Context, program string, filters []Filter) ([]KeyedAccount, error) {
	fs := make([]any, 0, len(filters))
	for _, f := range filters {
		if f.DataSize > 0 {
			fs = append(fs, map[string]any{"dataSize": f.DataSize})
		} else {
			fs = append(fs, map[string]any{"memcmp": map[string]any{"offset": f.Offset, "bytes": f.Bytes}})
		}
	}
	cfg := map[string]any{"encoding": "base64", "commitment": "confirmed"}
	if len(fs) > 0 {
		cfg["filters"] = fs
	}
	var res []struct {
		Pubkey  string     `json:"pubkey"`
		Account rpcAccount `json:"account"`
	}
	if err := r.call(ctx, "getProgramAccounts", []any{program, cfg}, &res); err != nil {
		return nil, err
	}
	out := make([]KeyedAccount, 0, len(res))
	for _, a := range res {
		d, err := a.Account.decode()
		if err != nil {
			return nil, fmt.Errorf("getProgramAccounts %s: %w", a.Pubkey, err)
		}
		out = append(out, KeyedAccount{Pubkey: a.Pubkey, Data: d})
	}
	return out, nil
}

// urlScrubber replaces the URL, and the parts of it that may carry credentials, with the bare
// scheme://host.
func urlScrubber(raw string) *strings.Replacer {
	if raw == "" {
		return strings.NewReplacer()
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.NewReplacer(raw, "<rpc url>")
	}
	base := u.Scheme + "://" + u.Host
	pairs := []string{raw, base, u.String(), base, u.Redacted(), base}
	if u.RawQuery != "" {
		pairs = append(pairs, u.RawQuery, "…")
	}
	if len(u.Path) > 1 {
		pairs = append(pairs, u.Path, "/…")
	}
	if u.User != nil {
		if n := u.User.Username(); n != "" {
			pairs = append(pairs, n, "…")
		}
		if p, ok := u.User.Password(); ok && p != "" {
			pairs = append(pairs, p, "…")
		}
	}
	return strings.NewReplacer(pairs...)
}
