package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"crypto-portfolio/internal/httpx"
)

// rpcClient speaks JSON-RPC 2.0 over HTTP POST. HTTP 429/5xx are retried by httpx.
//
// RPC URLs frequently embed an API key (Helius uses ?api-key=..., others put it in the path), and
// httpx error strings include the request URL, so every error leaving this type is scrubbed.
type rpcClient struct {
	url      string
	hc       *httpx.Client
	scrubber *strings.Replacer
	// methodPace spaces the calls of each method (the public RPC limits every method
	// separately); 0 for none.
	methodPace time.Duration
	host       string
}

func newRPCClient(rawURL string, hc *httpx.Client) *rpcClient {
	u, _ := url.Parse(rawURL)
	host := ""
	if u != nil {
		host = u.Host
	}
	return &rpcClient{url: rawURL, hc: hc, scrubber: urlScrubber(rawURL), host: host}
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call invokes method and decodes the "result" member into out (which may be nil).
func (r *rpcClient) call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	if r.methodPace > 0 {
		key := r.host + " " + method
		httpx.SetPace(key, r.methodPace)
		if err := httpx.Wait(ctx, key); err != nil {
			return err
		}
	}
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	var resp rpcResponse
	if err := r.hc.PostJSON(ctx, r.url, nil, req, &resp); err != nil {
		return r.scrub(err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// scrubbedError carries a message with secrets removed while still unwrapping to the original.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

func (r *rpcClient) scrub(err error) error {
	msg := err.Error()
	clean := r.scrubber.Replace(msg)
	if clean == msg {
		return err
	}
	return &scrubbedError{msg: clean, err: err}
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

// hostOnly returns just the host[:port] of an RPC URL, for display. Never the path or query,
// which may contain an API key.
func hostOnly(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
