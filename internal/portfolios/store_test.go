package portfolios

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

type fixture struct {
	dir, path, backups string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	return fixture{dir: dir, path: filepath.Join(dir, "portfolios.yaml"), backups: filepath.Join(dir, "backups")}
}

func (fx fixture) store(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(fx.path, fx.backups)
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

// touch pushes the file's mtime forward so a hand edit is always seen as a change, even on
// filesystems with coarse timestamps.
func (fx fixture) touch(t *testing.T) {
	t.Helper()
	future := time.Now().Add(time.Duration(touchCounter.next()) * time.Minute)
	if err := os.Chtimes(fx.path, future, future); err != nil {
		t.Fatal(err)
	}
}

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) next() int { c.mu.Lock(); defer c.mu.Unlock(); c.n++; return c.n }

var touchCounter counter

func ids(ps []Portfolio) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func simple(name string) Portfolio {
	return Portfolio{Name: name, Wallets: AllSelector(), Assets: AllSelector()}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnChange")
	}
}

func expectNone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected OnChange")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestNewStoreMissingFileIsEmpty(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
	if s.LoadError() != nil {
		t.Fatal(s.LoadError())
	}
	if _, err := os.Stat(fx.path); !os.IsNotExist(err) {
		t.Fatal("NewStore must not create the file")
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get on empty store")
	}
}

func TestNewStoreInvalidFileIsError(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "version: 1\nportfolios:\n  - {id: a, name: A, walets: x}\n")
	if _, err := NewStore(fx.path, fx.backups); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want a line-numbered error, got %v", err)
	}
}

func TestNewStoreLoadsExistingFile(t *testing.T) {
	fx := newFixture(t)
	raw, _ := os.ReadFile("testdata/example.yaml")
	fx.write(t, string(raw))
	s := fx.store(t)
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"long-term", "kids", "memes", "watch"}) {
		t.Fatalf("got %v", got)
	}
	p, ok := s.Get("kids")
	if !ok || p.Name != "Kids' wallets" {
		t.Fatalf("%v %v", p, ok)
	}
}

func TestListReturnsCopies(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if _, err := s.Upsert(Portfolio{Name: "A", Wallets: Selector{IDs: []string{"w"}}, Assets: AllSelector(), Watch: []string{"ETH"}}); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	got[0].Name = "mutated"
	got[0].Wallets.IDs[0] = "mutated"
	got[0].Watch[0] = "mutated"
	again, _ := s.Get("a")
	if again.Name != "A" || again.Wallets.IDs[0] != "w" || again.Watch[0] != "ETH" {
		t.Fatalf("cache was mutated through a returned copy: %#v", again)
	}
}

func TestUpsertAssignsUniqueSlugIDs(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	var got []string
	for _, name := range []string{"Kids' wallets", "Kids' wallets", "kids wallets", "All", "All"} {
		p, err := s.Upsert(simple(name))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p.ID)
	}
	want := []string{"kids-wallets", "kids-wallets-2", "kids-wallets-3", "all-1", "all-1-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Persisted: a fresh store sees the same list.
	if again := ids(fx.store(t).List()); !reflect.DeepEqual(again, want) {
		t.Fatalf("reloaded %v", again)
	}
}

func TestUpsertSlugSuffixKeepsIDWithinLimit(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	long := strings.Repeat("a", 60)
	for i := range 12 {
		p, err := s.Upsert(simple(long))
		if err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
		if len(p.ID) > 40 {
			t.Fatalf("id %q is %d long", p.ID, len(p.ID))
		}
	}
	if n := len(s.List()); n != 12 {
		t.Fatalf("want 12, got %d", n)
	}
}

func TestUpsertReplacesInPlaceKeepingOrder(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	for _, n := range []string{"One", "Two", "Three"} {
		if _, err := s.Upsert(simple(n)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Upsert(Portfolio{ID: "two", Name: "Two renamed", Wallets: Selector{IDs: []string{"w1"}}, Assets: Selector{IDs: []string{"btc", "BTC"}}, Watch: []string{"eth"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two", "three"}; !reflect.DeepEqual(ids(s.List()), want) {
		t.Fatalf("order %v", ids(s.List()))
	}
	if got.Name != "Two renamed" || !reflect.DeepEqual(got.Assets.IDs, []string{"BTC"}) || !reflect.DeepEqual(got.Watch, []string{"ETH"}) {
		t.Fatalf("returned portfolio not normalized: %#v", got)
	}
	stored, _ := s.Get("two")
	if !reflect.DeepEqual(stored, got) {
		t.Fatalf("stored %#v != returned %#v", stored, got)
	}
}

func TestUpsertExplicitNewID(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	p, err := s.Upsert(Portfolio{ID: "custom", Name: "Custom"})
	if err != nil || p.ID != "custom" {
		t.Fatalf("%v %v", p, err)
	}
}

func TestUpsertNoOpDoesNotRewrite(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	p, _ := s.Upsert(simple("Same"))
	if _, err := s.Upsert(simple("Other")); err != nil { // creates a first backup source
		t.Fatal(err)
	}
	before, _ := s.Backups()
	if _, err := s.Upsert(p); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Backups()
	if len(after) != len(before) {
		t.Fatalf("no-op upsert wrote a backup: %d -> %d", len(before), len(after))
	}
}

func TestUpsertInvalidLeavesEverythingAlone(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if _, err := s.Upsert(simple("Keep")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(fx.path)
	for name, p := range map[string]Portfolio{
		"no name":   {Name: "  "},
		"long name": {Name: strings.Repeat("x", 61)},
		"bad id":    {ID: "Bad ID", Name: "X"},
		"reserved":  {ID: "all", Name: "X"},
	} {
		if _, err := s.Upsert(p); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	after, _ := os.ReadFile(fx.path)
	if string(before) != string(after) {
		t.Fatal("file changed on a failed upsert")
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"keep"}) {
		t.Fatalf("in-memory list changed: %v", got)
	}
}

func TestUpsertDropsBlankSelectorEntries(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	p, err := s.Upsert(Portfolio{Name: "X", Wallets: Selector{IDs: []string{" ", "w", ""}}, Assets: AllSelector()})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Wallets.IDs, []string{"w"}) {
		t.Fatalf("%#v", p.Wallets)
	}
}

func TestDelete(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	for _, n := range []string{"One", "Two", "Three"} {
		s.Upsert(simple(n))
	}
	if err := s.Delete("two"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "three"}; !reflect.DeepEqual(ids(s.List()), want) {
		t.Fatalf("got %v", ids(s.List()))
	}
	if again := ids(fx.store(t).List()); !reflect.DeepEqual(again, []string{"one", "three"}) {
		t.Fatalf("not persisted: %v", again)
	}
	err := s.Delete("two")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestHandEditIsPickedUpByMtime(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if _, err := s.Upsert(simple("First")); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatal(got)
	}
	fx.write(t, "version: 1\nportfolios:\n  - {id: hand, name: Hand}\n  - {id: edited, name: Edited, assets: [btc]}\n")
	fx.touch(t)
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"hand", "edited"}) {
		t.Fatalf("hand edit not seen: %v", got)
	}
	p, _ := s.Get("edited")
	if !reflect.DeepEqual(p.Assets.IDs, []string{"BTC"}) || !p.Wallets.All {
		t.Fatalf("hand edit not normalized: %#v", p)
	}
	if s.LoadError() != nil {
		t.Fatal(s.LoadError())
	}
	// Deleting the file by hand empties the store.
	if err := os.Remove(fx.path); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("want empty after file removal, got %v", got)
	}
}

func TestWritesApplyOnTopOfHandEdits(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(simple("First"))
	fx.write(t, "version: 1\nportfolios:\n  - {id: hand, name: Hand}\n")
	fx.touch(t)
	// No List() in between: the write must still start from the on-disk state.
	if _, err := s.Upsert(simple("Second")); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"hand", "second"}) {
		t.Fatalf("got %v", got)
	}
}

func TestInvalidHandEditKeepsLastGoodAndReportsError(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(simple("Good"))
	fx.write(t, "version: 1\nportfolios:\n  - {id: good, name: Good, walets: [x]}\n")
	fx.touch(t)
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"good"}) {
		t.Fatalf("should keep serving the last good copy, got %v", got)
	}
	err := s.LoadError()
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want a line-numbered LoadError, got %v", err)
	}
	// The error persists across calls until the file changes again.
	s.List()
	if s.LoadError() == nil {
		t.Fatal("error should stick")
	}
	// Fixing the file clears it.
	fx.write(t, "version: 1\nportfolios:\n  - {id: fixed, name: Fixed}\n")
	fx.touch(t)
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"fixed"}) {
		t.Fatalf("got %v", got)
	}
	if s.LoadError() != nil {
		t.Fatal(s.LoadError())
	}
}

func TestSuccessfulWriteClearsLoadError(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(simple("Good"))
	fx.write(t, "this: is: not: valid")
	fx.touch(t)
	s.List()
	if s.LoadError() == nil {
		t.Fatal("want an error")
	}
	if _, err := s.Upsert(simple("Another")); err != nil {
		t.Fatal(err)
	}
	if s.LoadError() != nil {
		t.Fatal("a successful write replaces the broken file")
	}
	// The broken file was backed up first, so nothing is lost.
	backups, _ := s.Backups()
	found := false
	for _, b := range backups {
		if raw, err := os.ReadFile(filepath.Join(fx.backups, b.Name)); err == nil && strings.Contains(string(raw), "not: valid") {
			found = true
		}
	}
	if !found {
		t.Fatal("the broken hand edit should have been backed up")
	}
}

func TestOnChangeFiresOnWriteAndHandEdit(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	ch := make(chan struct{}, 8)
	s.OnChange(func() { ch <- struct{}{} })

	if _, err := s.Upsert(simple("One")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch)

	if err := s.Delete("one"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch)

	fx.write(t, "version: 1\nportfolios:\n  - {id: hand, name: Hand}\n")
	fx.touch(t)
	s.List()
	waitFor(t, ch)

	// Nothing changed: no event.
	s.List()
	expectNone(t, ch)

	// A failed write is not a change.
	if _, err := s.Upsert(Portfolio{Name: ""}); err == nil {
		t.Fatal("want error")
	}
	expectNone(t, ch)
}

func TestOnChangeMultipleListeners(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	a, b := make(chan struct{}, 2), make(chan struct{}, 2)
	s.OnChange(func() { a <- struct{}{} })
	s.OnChange(func() { b <- struct{}{} })
	s.Upsert(simple("One"))
	waitFor(t, a)
	waitFor(t, b)
}

func TestOnChangeRunsOutsideTheLock(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	done := make(chan struct{})
	s.OnChange(func() {
		s.List() // would deadlock if called while the store lock is held
		close(done)
	})
	s.Upsert(simple("One"))
	waitFor(t, done)
}

func importFile(ps ...Portfolio) []byte {
	raw, err := File{Version: 1, Portfolios: ps}.Encode()
	if err != nil {
		panic(err)
	}
	return raw
}

func pf(id, name string, assets ...string) Portfolio {
	p := Portfolio{ID: id, Name: name, Wallets: AllSelector(), Assets: AllSelector()}
	if len(assets) > 0 {
		p.Assets = Selector{IDs: assets}
	}
	return p
}

func TestImportMerge(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A", "BTC"))
	s.Upsert(pf("b", "B"))
	s.Upsert(pf("c", "C"))

	res, err := s.Import(importFile(
		pf("b", "B"),         // identical → unchanged
		pf("c", "C renamed"), // changed → replaced
		pf("d", "D", "ETH"),  // new → added
		pf("e", "E"),         // new → added
	), ImportMerge)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImportResult{Added: 2, Replaced: 1, Unchanged: 1}); res != want {
		t.Fatalf("got %+v want %+v", res, want)
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("order %v", got)
	}
	c, _ := s.Get("c")
	if c.Name != "C renamed" {
		t.Fatal(c)
	}
	if backups, _ := s.Backups(); len(backups) == 0 {
		t.Fatal("import must back up the previous file")
	}
}

func TestImportReplace(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A"))
	s.Upsert(pf("b", "B"))
	s.Upsert(pf("c", "C"))

	res, err := s.Import(importFile(pf("c", "C"), pf("x", "X"), pf("b", "B changed")), ImportReplace)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImportResult{Added: 1, Replaced: 1, Unchanged: 1, Removed: 1}); res != want {
		t.Fatalf("got %+v want %+v", res, want)
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"c", "x", "b"}) {
		t.Fatalf("replace should make the file exactly the import, got %v", got)
	}
	if backups, _ := s.Backups(); len(backups) == 0 {
		t.Fatal("import must back up the previous file")
	}
	if again := ids(fx.store(t).List()); !reflect.DeepEqual(again, []string{"c", "x", "b"}) {
		t.Fatalf("not persisted: %v", again)
	}
}

func TestImportNormalizesBeforeComparing(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A", "BTC"))
	res, err := s.Import([]byte("portfolios:\n  - {id: a, name: A, assets: [btc]}\n"), ImportMerge)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unchanged != 1 || res.Replaced != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestImportNoOpSkipsWrite(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A"))
	s.Upsert(pf("b", "B"))
	before, _ := s.Backups()
	res, err := s.Import(importFile(pf("a", "A")), ImportMerge)
	if err != nil || res != (ImportResult{Unchanged: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	after, _ := s.Backups()
	if len(before) != len(after) {
		t.Fatal("a no-op import should not write")
	}
}

func TestInvalidImportWritesNothing(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A"))
	before, _ := os.ReadFile(fx.path)
	nBackups, _ := s.Backups()

	for name, raw := range map[string]string{
		"typo":      "portfolios:\n  - {id: z, name: Z, walets: [x]}\n",
		"duplicate": "portfolios:\n  - {id: z, name: Z}\n  - {id: z, name: Z2}\n",
		"reserved":  "portfolios:\n  - {id: all, name: Z}\n",
		"empty":     "",
		"blank":     "  \n\n",
		"garbage":   "}{",
	} {
		for _, mode := range []ImportMode{ImportMerge, ImportReplace} {
			if _, err := s.Import([]byte(raw), mode); err == nil {
				t.Errorf("%s/%s: want error", name, mode)
			}
		}
	}
	if _, err := s.Import(importFile(pf("z", "Z")), ImportMode("bogus")); err == nil {
		t.Error("unknown mode must be rejected")
	}
	after, _ := os.ReadFile(fx.path)
	if string(before) != string(after) {
		t.Fatal("file changed by an invalid import")
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatal(got)
	}
	if now, _ := s.Backups(); len(now) != len(nBackups) {
		t.Fatal("invalid import created a backup")
	}
}

func TestImportIntoEmptyStore(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	res, err := s.Import(importFile(pf("a", "A")), ImportReplace)
	if err != nil || res != (ImportResult{Added: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(fx.path); err != nil {
		t.Fatal(err)
	}
}

func TestExport(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A", "BTC"))
	s.Upsert(Portfolio{ID: "b", Name: "B", Wallets: Selector{}, Assets: AllSelector(), Watch: []string{"ETH"}})

	all, err := s.Export("")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(all)
	if err != nil || !reflect.DeepEqual(ids(f.Portfolios), []string{"a", "b"}) {
		t.Fatalf("%v %v", err, f)
	}
	one, err := s.Export("b")
	if err != nil {
		t.Fatal(err)
	}
	f, err = Parse(one)
	if err != nil || len(f.Portfolios) != 1 || f.Portfolios[0].ID != "b" || !f.Portfolios[0].Wallets.IsNone() {
		t.Fatalf("%v %#v", err, f)
	}
	if _, err := s.Export("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Export → import into a fresh store reproduces the list.
	fx2 := newFixture(t)
	s2 := fx2.store(t)
	if _, err := s2.Import(all, ImportReplace); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.List(), s2.List()) {
		t.Fatalf("%#v\n%#v", s.List(), s2.List())
	}
}

func TestBackupsAndRestoreRoundTrip(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	if b, err := s.Backups(); err != nil || len(b) != 0 {
		t.Fatalf("%v %v", b, err)
	}
	s.Upsert(pf("a", "A"))           // first write: nothing to back up yet
	time.Sleep(3 * time.Millisecond) // distinct backup timestamps
	s.Upsert(pf("b", "B"))           // backs up {a}
	time.Sleep(3 * time.Millisecond)
	s.Delete("a") // backs up {a, b}; now {b}
	backups, err := s.Backups()
	if err != nil || len(backups) != 2 {
		t.Fatalf("want 2 backups, got %v %v", backups, err)
	}
	// Newest first: backups[0] holds {a, b}.
	time.Sleep(3 * time.Millisecond)
	if err := s.Restore(backups[0].Name); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("restore: %v", got)
	}
	// A restore backs up the current file first, so it is itself undoable.
	after, _ := s.Backups()
	if len(after) != 3 {
		t.Fatalf("want 3 backups after restore, got %d", len(after))
	}
	// Undo: the newest backup is the {b}-only state.
	time.Sleep(3 * time.Millisecond)
	if err := s.Restore(after[0].Name); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("undo: %v", got)
	}
	if again := ids(fx.store(t).List()); !reflect.DeepEqual(again, []string{"b"}) {
		t.Fatalf("not persisted: %v", again)
	}
}

func TestRestoreRejectsUnknownAndTraversalNames(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A"))
	time.Sleep(3 * time.Millisecond)
	s.Upsert(pf("b", "B"))

	// A file outside the backup dir that a traversal would reach.
	if err := os.WriteFile(filepath.Join(fx.dir, "evil.yaml"), []byte("portfolios:\n  - {id: evil, name: Evil}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "nope.yaml", "../evil.yaml", "../portfolios.yaml", "/etc/passwd", "portfolios-20200101-000000.000.yaml", "portfolios-../../evil.yaml"} {
		if err := s.Restore(name); err == nil {
			t.Errorf("Restore(%q) should fail", name)
		}
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("failed restores must not change anything: %v", got)
	}
}

func TestRestoreRejectsInvalidBackup(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(pf("a", "A"))
	time.Sleep(3 * time.Millisecond)
	s.Upsert(pf("b", "B"))
	backups, _ := s.Backups()
	// Corrupt the backup on disk.
	if err := os.WriteFile(filepath.Join(fx.backups, backups[0].Name), []byte("portfolios:\n  - {id: Bad ID}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(backups[0].Name); err == nil {
		t.Fatal("want validation error")
	}
	if got := ids(s.List()); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
}

func TestUpsertErrorOnReadOnlyDirLeavesMemoryUnchanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	fx := newFixture(t)
	s := fx.store(t)
	if _, err := s.Upsert(pf("a", "A")); err != nil {
		t.Fatal(err)
	}
	before := s.List()
	beforeRaw, _ := os.ReadFile(fx.path)

	if err := os.Chmod(fx.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(fx.dir, 0o700) })
	// Probe: if we can still write here, permissions are not enforced (some sandboxes).
	if f, err := os.CreateTemp(fx.dir, "probe"); err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Skip("directory permissions are not enforced here")
	}

	fired := make(chan struct{}, 1)
	s.OnChange(func() { fired <- struct{}{} })

	if _, err := s.Upsert(pf("b", "B")); err == nil {
		t.Fatal("want an error from a read-only directory")
	}
	if err := s.Delete("a"); err == nil {
		t.Fatal("delete should fail too")
	}
	if _, err := s.Import(importFile(pf("z", "Z")), ImportReplace); err == nil {
		t.Fatal("import should fail too")
	}
	if got := s.List(); !reflect.DeepEqual(got, before) {
		t.Fatalf("in-memory list changed after a failed write: %v", got)
	}
	afterRaw, _ := os.ReadFile(fx.path)
	if string(beforeRaw) != string(afterRaw) {
		t.Fatal("file changed")
	}
	expectNone(t, fired)
}

func TestRemoveWallet(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.Upsert(Portfolio{ID: "kids", Name: "Kids", Wallets: Selector{IDs: []string{"kids-phantom", "kids-cash"}}, Assets: AllSelector()})
	s.Upsert(Portfolio{ID: "solo", Name: "Solo", Wallets: Selector{IDs: []string{"kids-cash"}}, Assets: AllSelector()})
	s.Upsert(Portfolio{ID: "everything", Name: "Everything", Wallets: AllSelector(), Assets: AllSelector()})
	s.Upsert(Portfolio{ID: "other", Name: "Other", Wallets: Selector{IDs: []string{"cold"}}, Assets: AllSelector()})
	s.Upsert(Portfolio{ID: "watch", Name: "Watch", Wallets: Selector{}, Assets: AllSelector()})

	changed, err := s.RemoveWallet("kids-cash")
	if err != nil || changed != 2 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	kids, _ := s.Get("kids")
	if !reflect.DeepEqual(kids.Wallets.IDs, []string{"kids-phantom"}) {
		t.Fatalf("%#v", kids.Wallets)
	}
	solo, _ := s.Get("solo")
	if !solo.Wallets.IsNone() {
		t.Fatalf("a portfolio that only held the removed wallet is now empty: %#v", solo.Wallets)
	}
	if e, _ := s.Get("everything"); !e.Wallets.All {
		t.Fatal("'all' must be untouched")
	}
	if o, _ := s.Get("other"); !reflect.DeepEqual(o.Wallets.IDs, []string{"cold"}) {
		t.Fatal("unrelated portfolio changed")
	}

	// Nothing to remove: no write, no backup.
	before, _ := s.Backups()
	changed, err = s.RemoveWallet("ghost")
	if err != nil || changed != 0 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	after, _ := s.Backups()
	if len(before) != len(after) {
		t.Fatal("RemoveWallet wrote although nothing changed")
	}
	// Persisted.
	again, _ := fx.store(t).Get("kids")
	if !reflect.DeepEqual(again.Wallets.IDs, []string{"kids-phantom"}) {
		t.Fatalf("%#v", again)
	}
}

func TestConcurrentUse(t *testing.T) {
	fx := newFixture(t)
	s := fx.store(t)
	s.OnChange(func() { s.List() })
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := s.Upsert(simple("Concurrent")); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				s.List()
				s.LoadError()
				s.Get("concurrent")
				if i%2 == 0 {
					s.Export("")
				}
			}
		}()
	}
	wg.Wait()
	if n := len(s.List()); n != 30 {
		t.Fatalf("want 30, got %d", n)
	}
}
