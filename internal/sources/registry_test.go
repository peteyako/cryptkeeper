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

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/secrets"
	"cryptkeeper/internal/sources/manual"
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

func TestImportMergeReplaceAndSecrets(t *testing.T) {
	var builds atomic.Int32
	r, path := newReg(t, &builds)
	r.Upsert(Def{ID: "kraken", Type: TypeKraken, Label: "Kraken", APIKey: "k-1111", APISecret: "stored-secret"})
	r.Upsert(Def{ID: "gone", Type: TypeSolana, Label: "Old", Address: "abc"})

	// An export without secrets, plus a new exchange source with no secret at all.
	f := File{Sources: []Def{
		{ID: "kraken", Type: TypeKraken, Label: "Kraken renamed"},        // blank secret keeps the stored one
		{ID: "cb", Type: TypeCoinbase, Label: "Coinbase", KeyName: "kn"}, // new, no private key: allowed
		{ID: "cold", Type: TypeBitcoin, Label: "Cold", Key: "zpub-x", PrivateKey: ""},
	}}
	res, err := r.Import(f, false)
	if err != nil || res.Added != 2 || res.Replaced != 1 {
		t.Fatalf("merge: %+v %v", res, err)
	}
	d, _ := r.Get("kraken")
	opened, _ := r.Open(d)
	if opened.APISecret != "stored-secret" || d.Label != "Kraken renamed" {
		t.Fatalf("blank secret must keep the stored one: %+v", opened)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "stored-secret") {
		t.Fatal("plaintext secret on disk")
	}
	// Replace removes what the file doesn't list and reports it.
	res, err = r.Import(File{Sources: []Def{{ID: "cold", Type: TypeBitcoin, Label: "Cold", Key: "zpub-x"}}}, true)
	if err != nil || len(res.Removed) != 3 || len(r.Defs()) != 1 {
		t.Fatalf("replace: %+v %v (%d left)", res, err, len(r.Defs()))
	}
	// Invalid entries reject the whole import.
	if _, err := r.Import(File{Sources: []Def{{ID: "Bad ID", Type: TypeSolana, Label: "x", Address: "a"}}}, false); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := ParseFile([]byte("version: 1\nsources:\n  - {id: a, typo: b}\n")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("strict parse: %v", err)
	}
}

func TestValidateRefusesSeedInVault(t *testing.T) {
	d := Def{ID: "sol", Type: TypeSolana, Label: "Phantom", Kind: core.KindHot, Address: "7xKXabc",
		JupiterVault: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "seed phrase") {
		t.Fatalf("err = %v", err)
	}
	d.JupiterVault = "4N1FkXvBrAoyTwzM3R97RbDLpTZV9Jo17UYRnFnvNeDk"
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestDescriptionIsDisplayOnly: a wallet's description is saved with it, capped in length,
// and changing it doesn't rebuild the live source.
func TestDescriptionIsDisplayOnly(t *testing.T) {
	var builds atomic.Int32
	r, _ := newReg(t, &builds)
	mom := Def{ID: "mom", Type: TypeManual, Label: "Mom", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}}
	if _, err := r.Upsert(mom); err != nil {
		t.Fatal(err)
	}
	r.Entries()
	mom.Description = "Paper wallet in the safe, for the kids"
	if _, err := r.Upsert(mom); err != nil {
		t.Fatal(err)
	}
	es := r.Entries()
	if es[0].Def.Description != mom.Description {
		t.Fatalf("description %q", es[0].Def.Description)
	}
	if builds.Load() != 1 {
		t.Fatalf("a new description rebuilt the source (%d builds)", builds.Load())
	}
	mom.Description = strings.Repeat("x", MaxDescription+1)
	if _, err := r.Upsert(mom); err == nil {
		t.Fatal("an over-long description was saved")
	}
}
