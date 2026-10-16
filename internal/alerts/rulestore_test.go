package alerts

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct{ dir, path, backups string }

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	return fixture{dir: dir, path: filepath.Join(dir, "alerts.yaml"), backups: filepath.Join(dir, "backups")}
}

func (fx fixture) store(t *testing.T) *RuleStore {
	t.Helper()
	s, err := NewRuleStore(fx.path, fx.backups)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (fx fixture) write(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(fx.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var touchMu sync.Mutex
var touchN int

// touch pushes the mtime forward so a hand edit is always seen, even on coarse filesystems.
func (fx fixture) touch(t *testing.T) {
	t.Helper()
	touchMu.Lock()
	touchN++
	n := touchN
	touchMu.Unlock()
	future := time.Now().Add(time.Duration(n) * time.Minute)
	if err := os.Chtimes(fx.path, future, future); err != nil {
		t.Fatal(err)
	}
}

func ruleIDs(rs []Rule) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func (fx fixture) backupCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(fx.backups)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "alerts-") && strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	return n
}

func TestRuleStoreMissingFileIsEmpty(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("unexpected rule")
	}
	if err := s.LoadError(); err != nil {
		t.Fatal(err)
	}
}

func TestRuleStoreInvalidFileIsError(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "rules:\n  - {id: a, type: nope}\n")
	if _, err := NewRuleStore(fx.path, fx.backups); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestRuleStoreUpsertAssignsIDs(t *testing.T) {
	s := newFixture(t).store(t)
	a, err := s.Upsert(Rule{Type: TypePrice, Asset: "btc", Above: 100000})
	if err != nil || a.ID != "price-btc" || a.Asset != "BTC" {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := s.Upsert(Rule{Type: TypePrice, Asset: "BTC", Below: 50000})
	if err != nil || b.ID != "price-btc-2" {
		t.Fatalf("%+v %v", b, err)
	}
	c, err := s.Upsert(Rule{Type: TypePrice, Asset: "spl:So1ana", Below: 1})
	if err != nil || c.ID != "price-spl-so1ana" {
		t.Fatalf("%+v %v", c, err)
	}
	d, err := s.Upsert(Rule{Type: TypeBalanceDrop})
	if err != nil || d.ID != "balance-drop" {
		t.Fatalf("%+v %v", d, err)
	}
	e, err := s.Upsert(Rule{Type: TypeLargeActivity, MinUsd: 500})
	if err != nil || e.ID != "large-activity" {
		t.Fatalf("%+v %v", e, err)
	}
	if got := ruleIDs(s.List()); !reflect.DeepEqual(got, []string{"price-btc", "price-btc-2", "price-spl-so1ana", "balance-drop", "large-activity"}) {
		t.Fatalf("order %v", got)
	}
}

func TestRuleStoreUpsertReplacesInPlaceAndSkipsNoop(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	for _, r := range []Rule{
		{ID: "a", Type: TypeSourceDown, Minutes: 5},
		{ID: "b", Type: TypeSourceDown, Minutes: 10},
		{ID: "c", Type: TypeSourceDown, Minutes: 15},
	} {
		if _, err := s.Upsert(r); err != nil {
			t.Fatal(err)
		}
	}
	before := fx.backupCount(t)
	got, err := s.Upsert(Rule{ID: "b", Type: TypeSourceDown, Minutes: 99})
	if err != nil || got.Minutes != 99 {
		t.Fatalf("%+v %v", got, err)
	}
	if ids := ruleIDs(s.List()); !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("order changed: %v", ids)
	}
	if fx.backupCount(t) != before+1 {
		t.Fatalf("an overwrite should leave one more backup (%d -> %d)", before, fx.backupCount(t))
	}
	// Same content again: no rewrite, no new backup.
	if _, err := s.Upsert(Rule{ID: "b", Type: TypeSourceDown, Minutes: 99}); err != nil {
		t.Fatal(err)
	}
	if fx.backupCount(t) != before+1 {
		t.Fatalf("a no-op upsert wrote a backup")
	}
	// Survives a restart, with the backup base named after the file.
	again := fx.store(t)
	if r, ok := again.Get("b"); !ok || r.Minutes != 99 {
		t.Fatalf("not persisted: %+v %v", r, ok)
	}
}

func TestRuleStoreUpsertInvalidLeavesFileAlone(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if _, err := s.Upsert(Rule{ID: "a", Type: TypeSourceDown, Minutes: 5}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(fx.path)
	if _, err := s.Upsert(Rule{ID: "x", Type: TypePrice, Asset: "BTC"}); err == nil {
		t.Fatal("want validation error")
	}
	if _, err := s.Upsert(Rule{ID: "BAD ID", Type: TypeSourceDown, Minutes: 5}); err == nil {
		t.Fatal("want id error")
	}
	if now, _ := os.ReadFile(fx.path); string(now) != string(raw) {
		t.Fatalf("file changed by failed upsert:\n%s", now)
	}
	if got := ruleIDs(s.List()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("cache changed by failed upsert: %v", got)
	}
}

func TestRuleStoreDelete(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	_, _ = s.Upsert(Rule{ID: "a", Type: TypeSourceDown, Minutes: 5})
	_, _ = s.Upsert(Rule{ID: "b", Type: TypeSourceDown, Minutes: 5})
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if got := ruleIDs(s.List()); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("%v", got)
	}
	if err := s.Delete("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRuleStoreReloadsHandEdits(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	_, _ = s.Upsert(Rule{ID: "a", Type: TypeSourceDown, Minutes: 5})

	fx.write(t, "version: 1\nrules:\n  - {id: edited, type: large_activity, minUsd: 1}\n")
	fx.touch(t)
	if got := ruleIDs(s.List()); !reflect.DeepEqual(got, []string{"edited"}) {
		t.Fatalf("hand edit not picked up: %v", got)
	}

	// A broken edit keeps the last good copy and reports the error.
	fx.write(t, "rules:\n  - {id: edited, type: large_activity, minUsdd: 1}\n")
	fx.touch(t)
	if got := ruleIDs(s.List()); !reflect.DeepEqual(got, []string{"edited"}) {
		t.Fatalf("broken edit replaced good copy: %v", got)
	}
	if err := s.LoadError(); err == nil || !strings.Contains(err.Error(), "minUsdd") {
		t.Fatalf("want load error naming the typo, got %v", err)
	}

	// Fixing it clears the error.
	fx.write(t, "rules:\n  - {id: fixed, type: large_activity, minUsd: 1}\n")
	fx.touch(t)
	if got := ruleIDs(s.List()); !reflect.DeepEqual(got, []string{"fixed"}) {
		t.Fatalf("%v", got)
	}
	if err := s.LoadError(); err != nil {
		t.Fatal(err)
	}

	// Deleting the file empties the store.
	if err := os.Remove(fx.path); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("removed file: %+v", got)
	}
}

func TestRuleStoreReturnsCopies(t *testing.T) {
	s := newFixture(t).store(t)
	_, _ = s.Upsert(Rule{ID: "a", Type: TypeBalanceDrop, Wallets: []string{"cold"}})
	got := s.List()
	got[0].Wallets[0] = "mutated"
	got[0].Type = "x"
	r, _ := s.Get("a")
	if r.Type != TypeBalanceDrop || r.Wallets[0] != "cold" {
		t.Fatalf("store state leaked through a returned copy: %+v", r)
	}
}

func TestRuleStoreConcurrent(t *testing.T) {
	s := newFixture(t).store(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, err := s.Upsert(Rule{Type: TypeLargeActivity, MinUsd: float64(j)}); err != nil {
					t.Error(err)
				}
				_ = s.List()
			}
		}()
	}
	wg.Wait()
	if n := len(s.List()); n != 40 {
		t.Fatalf("want 40 rules with distinct auto ids, got %d", n)
	}
}

func TestRuleStoreImport(t *testing.T) {
	dir := t.TempDir()
	s, err := NewRuleStore(filepath.Join(dir, "alerts.yaml"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(Rule{ID: "keep", Type: TypePrice, Asset: "BTC", Above: 100}); err != nil {
		t.Fatal(err)
	}
	in := []Rule{
		{ID: "keep", Type: TypePrice, Asset: "BTC", Above: 200}, // replaced
		{ID: "new", Type: TypeSourceDown, Minutes: 30},          // added
		{Type: TypeLargeActivity, MinUsd: 1000},                 // added with a generated id
	}
	res, err := s.Import(in, false)
	if err != nil || res.Added != 2 || res.Replaced != 1 {
		t.Fatalf("merge: %+v %v", res, err)
	}
	if r, _ := s.Get("keep"); r.Above != 200 || len(s.List()) != 3 {
		t.Fatalf("after merge: %+v (%d rules)", r, len(s.List()))
	}
	res, err = s.Import(in[:1], true)
	if err != nil || res.Removed != 2 || len(s.List()) != 1 {
		t.Fatalf("replace: %+v %v (%d rules)", res, err, len(s.List()))
	}
	if _, err := s.Import([]Rule{{ID: "bad", Type: TypePrice}}, false); err == nil {
		t.Fatal("invalid rules must be rejected before writing")
	}
	if len(s.List()) != 1 {
		t.Fatal("a rejected import must not change anything")
	}
}
