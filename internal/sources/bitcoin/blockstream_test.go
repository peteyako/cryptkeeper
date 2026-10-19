package bitcoin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/httpx"
)

// fakeLogin is Blockstream's OpenID token endpoint: it accepts one client and hands out
// numbered tokens that last 300 seconds.
type fakeLogin struct {
	mu       sync.Mutex
	id, sec  string
	issued   int
	lastForm map[string]string
}

func (l *fakeLogin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = r.ParseForm()
	l.lastForm = map[string]string{}
	for k := range r.PostForm {
		l.lastForm[k] = r.PostForm.Get(k)
	}
	if r.PostForm.Get("client_id") != l.id || r.PostForm.Get("client_secret") != l.sec {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized_client", "error_description": "Invalid client or Invalid client credentials"})
		return
	}
	l.issued++
	json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+l.issued)), "expires_in": 300, "token_type": "Bearer"})
}

// enterprise wraps a fake Esplora so it only answers requests carrying a valid token.
type enterprise struct {
	f      *fakeEsplora
	mu     sync.Mutex
	valid  map[string]bool
	tokens []string // Authorization tokens seen, in order
	denied atomic.Int32
}

func (e *enterprise) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	e.mu.Lock()
	e.tokens = append(e.tokens, tok)
	ok := e.valid[tok]
	e.mu.Unlock()
	if !ok {
		e.denied.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	e.f.ServeHTTP(w, r)
}

type blockstreamEnv struct {
	auth       *BlockstreamAuth
	login      *fakeLogin
	ent        *enterprise
	public     *fakeEsplora
	publicURL  string
	clock      *time.Time
	testClient *httpx.Client
}

func newBlockstreamEnv(t *testing.T) *blockstreamEnv {
	t.Helper()
	login := &fakeLogin{id: "client-a", sec: "secret-a"}
	loginSrv := httptest.NewServer(login)
	t.Cleanup(loginSrv.Close)
	ent := &enterprise{f: newFake(), valid: map[string]bool{"tok1": true, "tok2": true, "tok3": true}}
	entSrv := httptest.NewServer(ent)
	t.Cleanup(entSrv.Close)
	public := newFake()
	publicSrv := httptest.NewServer(public)
	t.Cleanup(publicSrv.Close)

	client := &httpx.Client{HTTP: &http.Client{}, MaxRetries: 0, BaseDelay: time.Millisecond}
	clock := fixedNow
	auth := NewBlockstreamAuth(client)
	auth.tokenURL, auth.baseURL = loginSrv.URL, entSrv.URL+"/api"
	auth.now = func() time.Time { return clock }
	return &blockstreamEnv{auth: auth, login: login, ent: ent, public: public, publicURL: publicSrv.URL, clock: &clock, testClient: client}
}

// source builds a wallet whose configured server is the public fake, treated as a public
// Esplora (so the API key applies) unless private is set.
func (e *blockstreamEnv) source(t *testing.T, private bool) *Source {
	t.Helper()
	s, err := New(Config{ID: "w", Key: bip84Zpub, EsploraURL: e.publicURL + "/api", Auth: e.auth}, e.testClient)
	if err != nil {
		t.Fatal(err)
	}
	if !private {
		s.auth = e.auth // the fake's host isn't a known public server
	}
	s.now = func() time.Time { return *e.clock }
	return s
}

// TestBlockstreamKeyUsesEnterprise: with a key, requests go to the enterprise server with a
// bearer token, and the public server isn't asked.
func TestBlockstreamKeyUsesEnterprise(t *testing.T) {
	e := newBlockstreamEnv(t)
	e.auth.SetCredentials("client-a", "secret-a")
	e.ent.f.stats[bip84Receive0] = &fakeStats{chainFunded: 21_000, chainTx: 1}
	res, err := e.source(t, false).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Holdings[0].AmountStr != "0.00021000" {
		t.Fatalf("balance = %s, want the enterprise server's", res.Holdings[0].AmountStr)
	}
	if n := e.public.totalHits(); n != 0 {
		t.Errorf("public server asked %d times", n)
	}
	if e.login.issued != 1 {
		t.Errorf("%d tokens fetched for one scan, want 1", e.login.issued)
	}
	f := e.login.lastForm
	if f["grant_type"] != "client_credentials" || f["scope"] != "openid" || f["client_id"] != "client-a" || f["client_secret"] != "secret-a" {
		t.Errorf("token request form = %v", f)
	}
	if node := metaValue(res.Meta, "Node"); !strings.Contains(node, "API key") {
		t.Errorf("Node = %q, want it to say the API key is used", node)
	}
}

// TestBlockstreamTokenRefreshedBeforeExpiry: a token is reused until close to its 300 s
// lifetime, then a new one is fetched.
func TestBlockstreamTokenRefreshedBeforeExpiry(t *testing.T) {
	e := newBlockstreamEnv(t)
	e.auth.SetCredentials("client-a", "secret-a")
	ctx := context.Background()
	tok, err := e.auth.token(ctx)
	if err != nil || tok != "tok1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	*e.clock = fixedNow.Add(200 * time.Second)
	if tok, _ = e.auth.token(ctx); tok != "tok1" {
		t.Errorf("token at 200 s = %q, want the cached tok1", tok)
	}
	*e.clock = fixedNow.Add(280 * time.Second)
	if tok, _ = e.auth.token(ctx); tok != "tok2" {
		t.Errorf("token at 280 s = %q, want a fresh tok2", tok)
	}
}

// TestBlockstreamBadKeyFallsBackToPublic: a key the login server refuses doesn't stop the
// wallet: it scans through the public server and says the key isn't working.
func TestBlockstreamBadKeyFallsBackToPublic(t *testing.T) {
	e := newBlockstreamEnv(t)
	e.auth.SetCredentials("client-a", "wrong")
	e.public.stats[bip84Receive0] = &fakeStats{chainFunded: 9_000, chainTx: 1}
	res, err := e.source(t, false).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Holdings[0].AmountStr != "0.00009000" {
		t.Fatalf("balance = %s, want the public server's", res.Holdings[0].AmountStr)
	}
	if !hasWarning(res.Warnings, "Blockstream API key") {
		t.Errorf("warnings = %v, want one about the key", res.Warnings)
	}
}

// TestBlockstreamRejectedTokenRenewed: a token the enterprise server refuses is dropped; that
// request falls back to the public server and the next one gets a new token.
func TestBlockstreamRejectedTokenRenewed(t *testing.T) {
	e := newBlockstreamEnv(t)
	e.auth.SetCredentials("client-a", "secret-a")
	e.ent.valid = map[string]bool{"tok2": true} // tok1 was revoked
	s := e.source(t, false)
	var r esploraAddress
	if err := s.get(context.Background(), addressPath(bip84Receive0), &r); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if e.public.totalHits() != 1 {
		t.Errorf("rejected request not answered by the public server")
	}
	if err := s.get(context.Background(), addressPath(bip84Receive0), &r); err != nil {
		t.Fatal(err)
	}
	if got := e.ent.tokens[len(e.ent.tokens)-1]; got != "tok2" {
		t.Errorf("second request used %q, want a new token", got)
	}
}

// TestBlockstreamKeyNotUsedForPrivateNode: addresses never leave a private node because a key
// is set.
func TestBlockstreamKeyNotUsedForPrivateNode(t *testing.T) {
	e := newBlockstreamEnv(t)
	e.auth.SetCredentials("client-a", "secret-a")
	if _, err := e.source(t, true).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.ent.tokens) != 0 || e.login.issued != 0 {
		t.Errorf("enterprise asked %d times, %d tokens fetched; want none for a private node", len(e.ent.tokens), e.login.issued)
	}
}

// TestBlockstreamNoKey: without credentials nothing changes.
func TestBlockstreamNoKey(t *testing.T) {
	e := newBlockstreamEnv(t)
	if _, err := e.source(t, false).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.ent.tokens) != 0 || e.public.totalHits() == 0 {
		t.Errorf("enterprise asked %d times, public %d; want public only", len(e.ent.tokens), e.public.totalHits())
	}
}

func TestBlockstreamCheck(t *testing.T) {
	e := newBlockstreamEnv(t)
	if err := e.auth.Check(context.Background(), "client-a", "secret-a"); err != nil {
		t.Errorf("good key: %v", err)
	}
	err := e.auth.Check(context.Background(), "client-a", "nope")
	if err == nil || !strings.Contains(err.Error(), "Invalid client") {
		t.Errorf("bad key: %v, want Blockstream's reason", err)
	}
	if e.auth.Configured() {
		t.Error("Check stored the credentials")
	}
}

// TestPublicEsploraGetsKey: the key applies to the public servers only.
func TestPublicEsploraGetsKey(t *testing.T) {
	auth := NewBlockstreamAuth(nil)
	for url, want := range map[string]bool{
		"https://mempool.space/api": true, "https://blockstream.info/api": true, "http://node.lan:3002/api": false,
	} {
		s, err := New(Config{Key: bip84Zpub, EsploraURL: url, Auth: auth}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (s.auth != nil) != want {
			t.Errorf("%s: key used = %v, want %v", url, s.auth != nil, want)
		}
	}
}
