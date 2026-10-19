package server

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	"cryptkeeper/internal/secrets"
)

// fakeBlockstream accepts one key and records the credentials it was given.
type fakeBlockstream struct {
	mu         sync.Mutex
	id, secret string
	sets       int
}

func (f *fakeBlockstream) Check(_ context.Context, id, secret string) error {
	if id != "client-a" || secret != "secret-a" {
		return errors.New("Blockstream refused the key: Invalid client or Invalid client credentials")
	}
	return nil
}

func (f *fakeBlockstream) SetCredentials(id, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id, f.secret = id, secret
	f.sets++
}

func bitcoinKeyEnv(t *testing.T) (*env, *fakeBlockstream) {
	t.Helper()
	bs := &fakeBlockstream{}
	return newEnv(t, "", func(d *Deps) { d.Blockstream = bs }), bs
}

func (e *env) storedBlockstreamKey() (BlockstreamKey, bool) {
	e.t.Helper()
	k, ok, err := LoadBlockstreamKey(context.Background(), e.db, e.box)
	if err != nil {
		e.t.Fatal(err)
	}
	return k, ok
}

// TestSettingsBlockstreamKeySaved: a key Blockstream accepts is stored sealed, handed to the
// wallets, and shown masked.
func TestSettingsBlockstreamKeySaved(t *testing.T) {
	e, bs := bitcoinKeyEnv(t)
	body, hdr := form(url.Values{"blockstreamId": {"client-a"}, "blockstreamSecret": {"secret-a"}})
	_, out := e.do("POST", "/settings/bitcoin", body, hdr)
	k, ok := e.storedBlockstreamKey()
	if !ok || k.ID != "client-a" || k.Secret != "secret-a" {
		t.Fatalf("stored key = %+v, %v", k, ok)
	}
	var raw string
	if _, err := e.db.GetJSON(context.Background(), BlockstreamKeyKV, &raw); err != nil || !secrets.IsSealed(raw) || strings.Contains(raw, "secret-a") {
		t.Fatalf("key must be stored sealed: %q %v", raw, err)
	}
	if bs.id != "client-a" || bs.secret != "secret-a" {
		t.Errorf("wallets got %q/%q", bs.id, bs.secret)
	}
	if !strings.Contains(out, `id="settings-bitcoin"`) || !strings.Contains(out, "••••nt-a") || strings.Contains(out, "secret-a") {
		t.Fatalf("patch: %.800s", out)
	}
}

// TestSettingsBlockstreamKeyRefused: a key Blockstream refuses isn't stored, and the form says
// why.
func TestSettingsBlockstreamKeyRefused(t *testing.T) {
	e, bs := bitcoinKeyEnv(t)
	body, hdr := form(url.Values{"blockstreamId": {"client-a"}, "blockstreamSecret": {"wrong"}})
	_, out := e.do("POST", "/settings/bitcoin", body, hdr)
	if _, ok := e.storedBlockstreamKey(); ok {
		t.Fatal("refused key was stored")
	}
	if bs.sets != 0 {
		t.Error("refused key was handed to the wallets")
	}
	if !strings.Contains(out, "Invalid client") {
		t.Fatalf("patch should say why: %.800s", out)
	}
}

// TestSettingsBlockstreamKeyNeedsBoth: one half of the key is refused without asking Blockstream.
func TestSettingsBlockstreamKeyNeedsBoth(t *testing.T) {
	e, _ := bitcoinKeyEnv(t)
	body, hdr := form(url.Values{"blockstreamId": {"client-a"}})
	_, out := e.do("POST", "/settings/bitcoin", body, hdr)
	if _, ok := e.storedBlockstreamKey(); ok || !strings.Contains(out, "client ID and the client secret") {
		t.Fatalf("stored=%v patch: %.600s", ok, out)
	}
}

func TestSettingsBlockstreamKeyClear(t *testing.T) {
	e, bs := bitcoinKeyEnv(t)
	body, hdr := form(url.Values{"blockstreamId": {"client-a"}, "blockstreamSecret": {"secret-a"}})
	e.do("POST", "/settings/bitcoin", body, hdr)
	body, hdr = form(url.Values{"clear": {"1"}})
	_, out := e.do("POST", "/settings/bitcoin", body, hdr)
	if _, ok := e.storedBlockstreamKey(); ok {
		t.Fatal("key must be deleted")
	}
	if bs.id != "" || bs.secret != "" {
		t.Errorf("wallets still have %q", bs.id)
	}
	if !strings.Contains(out, "not set") {
		t.Fatalf("patch: %.600s", out)
	}
}

// TestSettingsBlockstreamKeyFromEnv: with BLOCKSTREAM_CLIENT_ID set, the form is disabled and
// a post changes nothing.
func TestSettingsBlockstreamKeyFromEnv(t *testing.T) {
	bs := &fakeBlockstream{}
	e := newEnv(t, "", func(d *Deps) { d.Blockstream = bs; d.Cfg.BlockstreamClientID = "env-client" })
	_, page := e.do("GET", "/settings", nil, nil)
	if !strings.Contains(page, `id="settings-bitcoin"`) || !strings.Contains(page, "BLOCKSTREAM_CLIENT_ID") {
		t.Fatalf("settings should say the key comes from the environment")
	}
	body, hdr := form(url.Values{"blockstreamId": {"client-a"}, "blockstreamSecret": {"secret-a"}})
	e.do("POST", "/settings/bitcoin", body, hdr)
	if _, ok := e.storedBlockstreamKey(); ok || bs.sets != 0 {
		t.Fatal("the environment's key was overridden from Settings")
	}
}
