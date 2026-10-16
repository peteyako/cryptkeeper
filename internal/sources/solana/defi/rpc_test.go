package defi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// rpcServer answers getMultipleAccounts from captured accounts and getProgramAccounts with a
// canned result, recording request bodies.
func rpcServer(t *testing.T, accts map[string][]byte, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("bad request body %s", raw)
		}
		*bodies = append(*bodies, req)
		params, _ := req["params"].([]any)
		switch req["method"] {
		case "getMultipleAccounts":
			keys, _ := params[0].([]any)
			vals := make([]any, len(keys))
			for i, k := range keys {
				if d, ok := accts[k.(string)]; ok {
					vals[i] = map[string]any{"data": []string{base64.StdEncoding.EncodeToString(d), "base64"}, "owner": "x", "lamports": 1}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": 1}, "value": vals}})
		case "getProgramAccounts":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": []any{
				map[string]any{"pubkey": "K1", "account": map[string]any{"data": []string{base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "base64"}}},
			}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPRPCEndToEndOrca(t *testing.T) {
	accts, _ := loadAccounts(t, "orca_accounts.json")
	var bodies []map[string]any
	srv := rpcServer(t, accts, &bodies)
	rpc := NewHTTPRPC(srv.URL, fastClient())
	ps, err := newOrca().Positions(context.Background(), rpc, orcaInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 || findPosition(t, ps, orcaPosIn).Legs[0].AmountStr != orcaWant[orcaPosIn].a {
		t.Fatalf("positions %+v", ps)
	}
	cfg := bodies[0]["params"].([]any)[1].(map[string]any)
	if cfg["encoding"] != "base64" || cfg["commitment"] != "confirmed" {
		t.Errorf("config %v", cfg)
	}
}

func TestHTTPRPCMissingAndEmpty(t *testing.T) {
	var bodies []map[string]any
	srv := rpcServer(t, map[string][]byte{"A": {9}, "E": {}}, &bodies)
	rpc := NewHTTPRPC(srv.URL, fastClient())
	got, err := rpc.GetMultipleAccounts(context.Background(), []string{"A", "missing", "E"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !slices.Equal(got[0], []byte{9}) || got[1] != nil || got[2] == nil || len(got[2]) != 0 {
		t.Errorf("got %v", got)
	}
	if got, err := rpc.GetMultipleAccounts(context.Background(), nil); err != nil || got != nil || len(bodies) != 1 {
		t.Errorf("empty request: %v, %v, %d bodies", got, err, len(bodies))
	}
}

func TestHTTPRPCProgramAccountsFilters(t *testing.T) {
	var bodies []map[string]any
	srv := rpcServer(t, nil, &bodies)
	rpc := NewHTTPRPC(srv.URL, fastClient())
	got, err := rpc.GetProgramAccounts(context.Background(), JupPerpsProgram, []Filter{
		{DataSize: 216},
		{Offset: 8, Bytes: perpsOwner},
	})
	if err != nil || len(got) != 1 || got[0].Pubkey != "K1" || !slices.Equal(got[0].Data, []byte{1, 2, 3}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	params := bodies[0]["params"].([]any)
	if params[0] != JupPerpsProgram {
		t.Errorf("program %v", params[0])
	}
	filters, _ := json.Marshal(params[1].(map[string]any)["filters"])
	if string(filters) != `[{"dataSize":216},{"memcmp":{"bytes":"`+perpsOwner+`","offset":8}}]` {
		t.Errorf("filters %s", filters)
	}
}

func TestHTTPRPCErrorsAreScrubbed(t *testing.T) {
	const secret = "s3cr3t-api-key"
	// JSON-RPC error object.
	var bodies []map[string]any
	srv := rpcServer(t, nil, &bodies)
	rpc := &httpRPC{url: srv.URL, hc: fastClient(), scrub: urlScrubber(srv.URL)}
	var out any
	if err := rpc.call(context.Background(), "nope", []any{}, &out); err == nil || !strings.Contains(err.Error(), "Method not found") {
		t.Errorf("err = %v", err)
	}
	// HTTP failure: the URL (with its API key) must not leak into the error.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer fail.Close()
	withKey := fail.URL + "/v1/" + secret + "?api-key=" + secret
	_, err := NewHTTPRPC(withKey, fastClient()).GetMultipleAccounts(context.Background(), []string{"A"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
	// Malformed account encoding.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[{"data":["!!!","base64"]}]}}`))
	}))
	defer bad.Close()
	if _, err := NewHTTPRPC(bad.URL, fastClient()).GetMultipleAccounts(context.Background(), []string{"A"}); err == nil {
		t.Error("bad base64 accepted")
	}
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":[]}}`))
	}))
	defer short.Close()
	if _, err := NewHTTPRPC(short.URL, fastClient()).GetMultipleAccounts(context.Background(), []string{"A"}); err == nil {
		t.Error("short result accepted")
	}
}
