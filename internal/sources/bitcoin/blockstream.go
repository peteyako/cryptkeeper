package bitcoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"crypto-portfolio/internal/httpx"
)

// A Blockstream Explorer API key is a client ID and secret from dashboard.blockstream.info.
// It is exchanged for access tokens that last 5 minutes (OAuth client credentials), which
// unlock enterprise.blockstream.info: the same Esplora API with far higher rate limits than
// the shared public servers.
const (
	blockstreamTokenURL = "https://login.blockstream.com/realms/blockstream-public/protocol/openid-connect/token"
	blockstreamAPI      = "https://enterprise.blockstream.info/api"
	// tokenMargin: a token this close to expiring is replaced before it is used.
	tokenMargin = 30 * time.Second
)

// publicEsploraHosts are the shared public servers a Blockstream key stands in for. A
// private node is never swapped for it: that would send its addresses to Blockstream.
var publicEsploraHosts = map[string]bool{"mempool.space": true, "blockstream.info": true}

// BlockstreamAuth holds the Blockstream API key and its current access token. One is shared
// by every Bitcoin wallet, and Settings can change the key at any time.
type BlockstreamAuth struct {
	client            *httpx.Client
	tokenURL, baseURL string
	now               func() time.Time

	fetchMu sync.Mutex // one token request at a time

	mu         sync.Mutex
	id, secret string
	tok        string
	expires    time.Time
}

// NewBlockstreamAuth returns an auth with no key set. A nil client means httpx.New().
func NewBlockstreamAuth(c *httpx.Client) *BlockstreamAuth {
	if c == nil {
		c = httpx.New()
	}
	return &BlockstreamAuth{client: c, tokenURL: blockstreamTokenURL, baseURL: blockstreamAPI, now: time.Now}
}

// SetCredentials replaces the key ("" for none) and drops the current token.
func (a *BlockstreamAuth) SetCredentials(id, secret string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.id, a.secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	a.tok, a.expires = "", time.Time{}
}

// Configured reports whether a key is set.
func (a *BlockstreamAuth) Configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id != "" && a.secret != ""
}

// Check asks Blockstream for a token with id and secret, keeping neither, and returns
// Blockstream's reason when it refuses them.
func (a *BlockstreamAuth) Check(ctx context.Context, id, secret string) error {
	_, _, err := a.requestToken(ctx, strings.TrimSpace(id), strings.TrimSpace(secret))
	return err
}

// token returns an access token for the current key, fetching a new one when there is none
// or it is about to expire.
func (a *BlockstreamAuth) token(ctx context.Context) (string, error) {
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()
	a.mu.Lock()
	id, secret, tok, expires := a.id, a.secret, a.tok, a.expires
	a.mu.Unlock()
	if tok != "" && a.now().Add(tokenMargin).Before(expires) {
		return tok, nil
	}
	tok, life, err := a.requestToken(ctx, id, secret)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.id == id && a.secret == secret { // the key didn't change meanwhile
		a.tok, a.expires = tok, a.now().Add(life)
	}
	a.mu.Unlock()
	return tok, nil
}

// drop forgets tok if it is still the current token: the server refused it.
func (a *BlockstreamAuth) drop(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == tok {
		a.tok = ""
	}
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	ErrorDescription string `json:"error_description"`
}

// requestToken exchanges id and secret for an access token and its lifetime. Errors never
// contain the secret.
func (a *BlockstreamAuth) requestToken(ctx context.Context, id, secret string) (string, time.Duration, error) {
	if id == "" || secret == "" {
		return "", 0, errors.New("no Blockstream API key set")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}, "scope": {"openid"}}
	var r tokenResponse
	if err := a.client.PostRaw(ctx, a.tokenURL, "application/x-www-form-urlencoded", nil, []byte(form.Encode()), &r); err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && json.Unmarshal([]byte(se.Body), &r) == nil && r.ErrorDescription != "" {
			return "", 0, fmt.Errorf("Blockstream refused the key: %s", r.ErrorDescription)
		}
		return "", 0, fmt.Errorf("Blockstream login: %w", err)
	}
	if r.AccessToken == "" {
		return "", 0, errors.New("Blockstream login returned no access token")
	}
	life := time.Duration(r.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	return r.AccessToken, life, nil
}
