package sources

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources/manual"
)

type fakeSource struct{ id, secret string }

func (f fakeSource) ID() string { return f.id }
func (f fakeSource) Fetch(context.Context) (core.WalletResult, error) {
	return core.WalletResult{WalletID: f.id}, nil
}

func newReg(t *testing.T, builds *atomic.Int32) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	box, _ := secrets.NewBox(bytes.Repeat([]byte{9}, 32))
	path := filepath.Join(dir, "sources.yaml")
	r, err := NewRegistry(path, filepath.Join(dir, "backups"), box, func(d Def) (core.Source, error) {
		builds.Add(1)
		return fakeSource{id: d.ID, secret: d.APISecret}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, path
}

func TestUpsertSealsSecretsAndKeepsThemOnEdit(t *testing.T) {
	var builds atomic.Int32
	r, path := newReg(t, &builds)
	_, err := r.Upsert(Def{ID: "kraken", Type: TypeKraken, Label: "Kraken", APIKey: "key-1234", APISecret: "s3cr3t-plain"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "s3cr3t-plain") || strings.Contains(string(raw), "key-1234") {
		t.Fatalf("plaintext secret on disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), "enc:v1:") {
		t.Fatalf("expected sealed values:\n%s", raw)
	}
	// Edit the label with blank secrets: secrets are kept.
	if _, err := r.Upsert(Def{ID: "kraken", Type: TypeKraken, Label: "Kraken main"}); err != nil {
		t.Fatal(err)
	}
	es := r.Entries()
	if len(es) != 1 || es[0].Source.(fakeSource).secret != "s3cr3t-plain" || es[0].Def.Label != "Kraken main" {
		t.Fatalf("%+v", es)
	}
	// Default kind applied.
	if es[0].Def.Kind != core.KindExchange {
		t.Fatalf("kind %q", es[0].Def.Kind)
	}
	exp, _ := r.Export(false)
	if strings.Contains(string(exp), "enc:v1:") {
		t.Fatalf("export without secrets leaked sealed values:\n%s", exp)
	}
}

func TestEntriesReuseBuiltSources(t *testing.T) {
	var builds atomic.Int32
	r, _ := newReg(t, &builds)
	r.Upsert(Def{ID: "mom", Type: TypeManual, Label: "Mom", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}})
	r.Entries()
	r.Entries()
	if builds.Load() != 1 {
		t.Fatalf("built %d times, want 1", builds.Load())
	}
	r.Upsert(Def{ID: "mom", Type: TypeManual, Label: "Mom", Holdings: []manual.Holding{{Asset: "BTC", Amount: 2}}})
	r.Entries()
	if builds.Load() != 2 {
		t.Fatalf("changed def should rebuild, built %d", builds.Load())
	}
}

func TestHandEditReloadAndValidation(t *testing.T) {
	var builds atomic.Int32
	r, path := newReg(t, &builds)
	changed := make(chan struct{}, 4)
	r.OnChange(func() { changed <- struct{}{} })
	r.Upsert(Def{ID: "sol", Type: TypeSolana, Label: "Phantom", Address: "abc"})
	<-changed
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(path, []byte("version: 1\nsources:\n  - {id: sol, type: solana, label: Renamed, kind: hot, address: abc}\n"), 0o600)
	future := time.Now().Add(time.Second)
	os.Chtimes(path, future, future)
	if d, _ := r.Get("sol"); d.Label != "Renamed" {
		t.Fatalf("hand edit not picked up: %q", d.Label)
	}
	if _, err := r.Upsert(Def{ID: "Bad ID", Type: TypeSolana, Label: "x", Address: "a"}); err == nil {
		t.Fatal("expected id validation error")
	}
	if err := r.Delete("nope"); err == nil {
		t.Fatal("expected unknown id error")
	}
	if got := r.UniqueID("Phantom!"); got != "phantom" {
		t.Fatalf("unique id %q", got)
	}
	if got := r.UniqueID("Sol"); got != "sol-2" {
		t.Fatalf("unique id %q", got)
	}
}
