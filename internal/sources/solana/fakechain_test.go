package solana

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/base58"
)

const (
	testOwner  = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	usdcMint   = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	testRPCKey = "SECRETKEY123"
)

// testMint returns a valid, deterministic 32-byte base58 pubkey for a fake mint.
func testMint(name string) string {
	h := sha256.Sum256([]byte("vault-test-mint/" + name))
	return base58.Encode(h[:])
}

func base58Decode32(s string) []byte {
	b := base58.Decode(s)
	if len(b) != 32 {
		panic("not a 32-byte key: " + s)
	}
	return b
}

type fakeAcct struct {
	pubkey, mint, amount string
	decimals             int
}

type fakeStakeAcct struct {
	pubkey   string
	lamports uint64
}

// fakeChain is an in-memory Solana JSON-RPC endpoint plus Jupiter token API. It records what
// it was asked so tests can assert on lookups.
type fakeChain struct {
	t     *testing.T
	owner string
	srv   *httptest.Server

	mu         sync.Mutex
	balance    uint64
	tokenAccts map[string][]fakeAcct     // token program id -> accounts
	mintInfo   map[string]map[string]any // mint -> getAccountInfo(jsonParsed) value
	metaAccts  map[string][]byte         // metaplex PDA -> raw account data
	stake      []fakeStakeAcct
	stakeFail  string // "", "rpc" (JSON-RPC error object) or "http" (HTTP 403)
	jupiter    map[string]map[string]any
	jupStatus  int // non-zero: Jupiter answers with this HTTP status
	// failMethods makes a method answer with a JSON-RPC error carrying this message.
	failMethods map[string]string
	// failInfo makes every getAccountInfo answer with a JSON-RPC error.
	failInfo  bool
	infoDelay time.Duration

	// observations
	calls       map[string]int
	jupQueries  [][]string
	infoReqs    []string // "address|encoding"
	inflight    int
	maxInflight int
}

func newFakeChain(t *testing.T) *fakeChain {
	return &fakeChain{
		t: t, owner: testOwner,
		tokenAccts:  map[string][]fakeAcct{},
		mintInfo:    map[string]map[string]any{},
		metaAccts:   map[string][]byte{},
		jupiter:     map[string]map[string]any{},
		failMethods: map[string]string{},
		calls:       map[string]int{},
	}
}

// start serves the fake and returns a Source configured against it. The RPC URL carries an
// API key in its query string, like Helius, so tests can check it never leaks.
func (f *fakeChain) source(mutate func(*Config)) *Source {
	f.t.Helper()
	f.srv = httptest.NewServer(f)
	f.t.Cleanup(f.srv.Close)
	cfg := Config{
		ID: "phantom", Label: "Phantom", Address: f.owner,
		RPCURL:           f.srv.URL + "/?api-key=" + testRPCKey,
		IncludeStake:     true,
		JupiterTokensURL: f.srv.URL + "/jup",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg, fastClient(f.srv), NewMemoryMetaCache())
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fakeChain) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeChain) jupiterQueries() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.jupQueries...)
}

func (f *fakeChain) infoRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.infoReqs...)
}

func (f *fakeChain) addTokenAccount(program string, a fakeAcct) {
	if a.pubkey == "" {
		a.pubkey = testMint("acct/" + a.mint + "/" + a.amount + fmt.Sprint(len(f.tokenAccts[program])))
	}
	f.tokenAccts[program] = append(f.tokenAccts[program], a)
}

func (f *fakeChain) addMetaplex(mint, name, symbol string) {
	pda, err := metadataPDA(mint)
	if err != nil {
		f.t.Fatal(err)
	}
	f.metaAccts[pda] = buildMetadataAccount(mint, name, symbol, "https://example.com/"+symbol+".json")
}

func (f *fakeChain) addJupiter(mint, symbol, name string, decimals int, verified bool) {
	f.jupiter[mint] = map[string]any{
		"id": mint, "symbol": symbol, "name": name, "decimals": decimals,
		"icon": "https://img.example/" + symbol + ".png", "isVerified": verified,
	}
}

func (f *fakeChain) loadJupiterFixture(mint, file string) {
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		f.t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
		f.t.Fatal(err)
	}
	f.jupiter[mint] = arr[0]
}

func plainMintInfo(decimals int) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"program": "spl-token", "space": 82,
			"parsed": map[string]any{"type": "mint", "info": map[string]any{"decimals": decimals, "isInitialized": true, "supply": "1"}},
		},
		"owner": TokenProgramID, "lamports": 1461600, "executable": false,
	}
}

func token2022MintInfo(decimals int, name, symbol string) map[string]any {
	exts := []any{
		map[string]any{"extension": "metadataPointer", "state": map[string]any{"metadataAddress": "x"}},
	}
	if name != "" || symbol != "" {
		exts = append(exts, map[string]any{"extension": "tokenMetadata", "state": map[string]any{
			"name": name, "symbol": symbol, "uri": "https://example.com/t.json", "additionalMetadata": []any{},
		}})
	}
	return map[string]any{
		"data": map[string]any{
			"program": "spl-token-2022", "space": 400,
			"parsed": map[string]any{"type": "mint", "info": map[string]any{"decimals": decimals, "extensions": exts}},
		},
		"owner": Token2022ProgramID, "lamports": 5000000, "executable": false,
	}
}

func (f *fakeChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/jup" {
		f.serveJupiter(w, r)
		return
	}
	f.serveRPC(w, r)
}

func (f *fakeChain) serveJupiter(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["jupiter"]++
	var mints []string
	if q := r.URL.Query().Get("query"); q != "" {
		mints = strings.Split(q, ",")
	}
	f.jupQueries = append(f.jupQueries, mints)
	if f.jupStatus != 0 {
		w.WriteHeader(f.jupStatus)
		fmt.Fprint(w, `{"code":`+fmt.Sprint(f.jupStatus)+`,"message":"gateway"}`)
		return
	}
	out := []map[string]any{}
	for _, m := range mints {
		if t, ok := f.jupiter[m]; ok {
			out = append(out, t)
		}
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeChain) serveRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("bad rpc request: %v", err)
		http.Error(w, "bad", 400)
		return
	}
	f.mu.Lock()
	f.calls[req.Method]++
	if msg, ok := f.failMethods[req.Method]; ok {
		f.mu.Unlock()
		rpcErrorReply(w, -32000, msg)
		return
	}
	f.mu.Unlock()

	str := func(i int) string {
		var s string
		if i < len(req.Params) {
			json.Unmarshal(req.Params[i], &s)
		}
		return s
	}
	opts := func(i int) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if i < len(req.Params) {
			json.Unmarshal(req.Params[i], &m)
		}
		return m
	}
	ctx := map[string]any{"slot": 1, "apiVersion": "4.3.0"}

	switch req.Method {
	case "getBalance":
		if str(0) != f.owner {
			f.t.Errorf("getBalance for %s", str(0))
		}
		f.mu.Lock()
		bal := f.balance
		f.mu.Unlock()
		rpcReply(w, map[string]any{"context": ctx, "value": bal})

	case "getTokenAccountsByOwner":
		if str(0) != f.owner {
			f.t.Errorf("getTokenAccountsByOwner for %s", str(0))
		}
		var prog string
		json.Unmarshal(opts(1)["programId"], &prog)
		var enc string
		json.Unmarshal(opts(2)["encoding"], &enc)
		if enc != "jsonParsed" {
			f.t.Errorf("getTokenAccountsByOwner encoding = %q", enc)
		}
		f.mu.Lock()
		accts := append([]fakeAcct(nil), f.tokenAccts[prog]...)
		f.mu.Unlock()
		value := []any{}
		for _, a := range accts {
			value = append(value, map[string]any{
				"pubkey": a.pubkey,
				"account": map[string]any{
					"lamports": 2039280, "executable": false, "owner": prog, "space": 165,
					"data": map[string]any{
						"program": "spl-token", "space": 165,
						"parsed": map[string]any{"type": "account", "info": map[string]any{
							"mint": a.mint, "owner": f.owner, "state": "initialized", "isNative": false,
							// uiAmountString is deliberately useless: code must use the integer "amount".
							"tokenAmount": map[string]any{"amount": a.amount, "decimals": a.decimals, "uiAmount": 0.0, "uiAmountString": "ignored"},
						}},
					},
				},
			})
		}
		rpcReply(w, map[string]any{"context": ctx, "value": value})

	case "getAccountInfo":
		addr := str(0)
		var enc string
		json.Unmarshal(opts(1)["encoding"], &enc)
		f.mu.Lock()
		f.infoReqs = append(f.infoReqs, addr+"|"+enc)
		f.inflight++
		if f.inflight > f.maxInflight {
			f.maxInflight = f.inflight
		}
		delay, fail := f.infoDelay, f.failInfo
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		f.mu.Lock()
		f.inflight--
		var value any
		switch enc {
		case "jsonParsed":
			if v, ok := f.mintInfo[addr]; ok {
				value = v
			}
		case "base64":
			if raw, ok := f.metaAccts[addr]; ok {
				value = map[string]any{
					"data":  []string{base64.StdEncoding.EncodeToString(raw), "base64"},
					"owner": MetadataProgramID, "lamports": 5616720, "executable": false, "space": len(raw),
				}
			}
		default:
			f.t.Errorf("getAccountInfo encoding = %q", enc)
		}
		f.mu.Unlock()
		if fail {
			rpcErrorReply(w, -32005, "node is unhealthy")
			return
		}
		rpcReply(w, map[string]any{"context": ctx, "value": value})

	case "getProgramAccounts":
		if str(0) != StakeProgramID {
			f.t.Errorf("getProgramAccounts program = %s", str(0))
		}
		o := opts(1)
		var enc string
		json.Unmarshal(o["encoding"], &enc)
		if enc != "jsonParsed" {
			f.t.Errorf("getProgramAccounts encoding = %q", enc)
		}
		var filters []struct {
			Memcmp struct {
				Offset int    `json:"offset"`
				Bytes  string `json:"bytes"`
			} `json:"memcmp"`
		}
		json.Unmarshal(o["filters"], &filters)
		if len(filters) != 1 || filters[0].Memcmp.Offset != 44 || filters[0].Memcmp.Bytes != f.owner {
			f.t.Errorf("getProgramAccounts filters = %s", o["filters"])
		}
		f.mu.Lock()
		fail, stake := f.stakeFail, append([]fakeStakeAcct(nil), f.stake...)
		f.mu.Unlock()
		switch fail {
		case "rpc":
			rpcErrorReply(w, -32010, "Program excluded from account secondary indexes")
			return
		case "http":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Indexed requests require a personal token"},"id":1}`)
			return
		}
		res := []any{}
		for _, s := range stake {
			res = append(res, map[string]any{
				"pubkey": s.pubkey,
				"account": map[string]any{
					"lamports": s.lamports, "owner": StakeProgramID, "executable": false, "space": 200,
					"data": map[string]any{"program": "stake", "space": 200, "parsed": map[string]any{
						"type": "delegated",
						"info": map[string]any{"meta": map[string]any{"authorized": map[string]any{"staker": f.owner, "withdrawer": f.owner}}},
					}},
				},
			})
		}
		rpcReply(w, res)

	default:
		rpcErrorReply(w, -32601, "Method not found")
	}
}

func rpcReply(w http.ResponseWriter, result any) {
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func rpcErrorReply(w http.ResponseWriter, code int, msg string) {
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": code, "message": msg}})
}
