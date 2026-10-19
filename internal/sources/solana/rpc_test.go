package solana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/httpx"
)

func fastClient(srv *httptest.Server) *httpx.Client {
	return &httpx.Client{HTTP: srv.Client(), MaxRetries: 2, BaseDelay: time.Millisecond}
}

func TestRPCCallDecodesResult(t *testing.T) {
	var gotBody struct {
		JSONRPC string            `json:"jsonrpc"`
		Method  string            `json:"method"`
		Params  []json.RawMessage `json:"params"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method=%s content-type=%s", r.Method, r.Header.Get("Content-Type"))
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"context":{"slot":9},"value":42},"id":1}`)
	}))
	defer srv.Close()

	rc := newRPCClient(srv.URL, fastClient(srv))
	var out struct {
		Value uint64 `json:"value"`
	}
	if err := rc.call(context.Background(), "getBalance", []any{"abc"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Value != 42 {
		t.Errorf("value = %d", out.Value)
	}
	if gotBody.JSONRPC != "2.0" || gotBody.Method != "getBalance" || len(gotBody.Params) != 1 {
		t.Errorf("request body = %+v", gotBody)
	}
}

func TestRPCCallErrorObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32010,"message":"Program excluded from secondary indexes"},"id":1}`)
	}))
	defer srv.Close()

	err := newRPCClient(srv.URL, fastClient(srv)).call(context.Background(), "getProgramAccounts", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var re *rpcError
	if !errors.As(err, &re) || re.Code != -32010 {
		t.Fatalf("err = %v (%T), want *rpcError code -32010", err, err)
	}
	if !strings.Contains(err.Error(), "Program excluded from secondary indexes") {
		t.Errorf("message missing from %q", err)
	}
}

func TestRPCRetriesOn429(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":429,"message":"Too many requests"},"id":1}`)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","result":7,"id":1}`)
	}))
	defer srv.Close()

	var out int
	if err := newRPCClient(srv.URL, fastClient(srv)).call(context.Background(), "getSlot", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out != 7 || n.Load() != 2 {
		t.Errorf("out=%d attempts=%d", out, n.Load())
	}
}

func TestRPCHTTPErrorIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Indexed requests require a personal token"},"id":1}`)
	}))
	defer srv.Close()

	url := srv.URL + "/v2/PATHSECRET?api-key=QUERYSECRET"
	err := newRPCClient(url, fastClient(srv)).call(context.Background(), "getProgramAccounts", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, secret := range []string{"QUERYSECRET", "PATHSECRET", "api-key"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error leaks %q: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "403") {
		t.Errorf("error should keep the status: %s", msg)
	}
}

func TestRPCHostOnly(t *testing.T) {
	cases := map[string]string{
		"https://mainnet.helius-rpc.com/?api-key=SECRET": "mainnet.helius-rpc.com",
		"https://api.mainnet-beta.solana.com":            "api.mainnet-beta.solana.com",
		"http://localhost:8899/":                         "localhost:8899",
		"https://user:pw@rpc.example.com/v2/KEY":         "rpc.example.com",
		"::bad::":                                        "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
