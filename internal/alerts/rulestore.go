package alerts

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"cryptkeeper/internal/yamlfile"
)

// backupsKept is how many backups yamlfile.WriteAtomic retains.
const backupsKept = 30

// ErrNotFound is returned (wrapped) when a rule id does not exist.
var ErrNotFound = errors.New("alert rule not found")

type fileStamp struct {
	exists bool
	mtime  time.Time
	size   int64
}

func (a fileStamp) same(b fileStamp) bool {
	return a.exists == b.exists && a.size == b.size && a.mtime.Equal(b.mtime)
}

// RuleStore keeps the alert rules in data/alerts.yaml. It is safe for concurrent use. Every
// read first checks the file's mtime so hand edits are picked up; a hand edit that no longer
// parses leaves the last good copy in place and is reported by LoadError. Every write
// validates the whole file, backs up the previous one (base name "alerts") and replaces it
// atomically; the cached copy only changes after the write succeeded.
type RuleStore struct {
	path      string
	backupDir string

	mu      sync.Mutex
	file    File
	stamp   fileStamp
	loadErr error
}

// NewRuleStore opens the store. A missing file is an empty store; an invalid one is an error.
func NewRuleStore(path, backupDir string) (*RuleStore, error) {
	s := &RuleStore{path: path, backupDir: backupDir, file: File{Version: Version}}
	stamp, err := s.stat()
	if err != nil {
		return nil, err
	}
	s.stamp = stamp
	if !stamp.exists {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.file = f
	return s, nil
}

func (s *RuleStore) stat() (fileStamp, error) {
	info, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{exists: true, mtime: info.ModTime(), size: info.Size()}, nil
}

// syncLocked reloads the file if it changed on disk since we last looked.
func (s *RuleStore) syncLocked() {
	cur, err := s.stat()
	if err != nil {
		s.loadErr = fmt.Errorf("%s: %w", s.path, err)
		return
	}
	if cur.same(s.stamp) {
		return
	}
	s.stamp = cur // taken before reading, so an edit racing the read is caught next time
	if !cur.exists {
		s.file, s.loadErr = File{Version: Version}, nil
		return
	}
	raw, err := os.ReadFile(s.path)
	if err == nil {
		var f File
		if f, err = Parse(raw); err == nil {
			s.file, s.loadErr = f, nil
			return
		}
	}
	s.loadErr = fmt.Errorf("%s: %w", s.path, err)
}

// LoadError is the parse error of the last hand edit that was rejected, or nil.
func (s *RuleStore) LoadError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	return s.loadErr
}

// List returns a copy of the rules in file order.
func (s *RuleStore) List() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	return s.file.clone().Rules
}

// Get returns a copy of one rule.
func (s *RuleStore) Get(id string) (Rule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	if i := s.file.index(id); i >= 0 {
		return s.file.Rules[i].clone(), true
	}
	return Rule{}, false
}

// commitLocked writes data (which must be the encoding of f) and, only if that succeeded,
// makes f the cached copy.
func (s *RuleStore) commitLocked(f File, data []byte) error {
	if err := yamlfile.WriteAtomic(s.path, data, s.backupDir, backupsKept); err != nil {
		return err
	}
	s.file, s.loadErr = f, nil
	if stamp, err := s.stat(); err == nil {
		s.stamp = stamp
	}
	return nil
}

// Upsert adds or replaces a rule and returns the stored (normalized) rule. With an empty ID
// one is derived from the type, plus the asset for price rules ("price-btc"), made unique
// with -2, -3, …. An existing ID is replaced in place, keeping the order; an unknown
// non-empty ID is appended. The whole file is validated before anything is written, and an
// upsert that changes nothing is not written.
func (s *RuleStore) Upsert(r Rule) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	f := s.file.clone()
	r = r.clone()
	r.normalize()
	if r.ID == "" {
		r.ID = uniqueID(autoID(r), func(id string) bool { return f.index(id) >= 0 })
	}
	i := f.index(r.ID)
	if i >= 0 {
		f.Rules[i] = r
	} else {
		f.Rules = append(f.Rules, r)
	}
	f.normalize()
	if err := f.Validate(); err != nil {
		return Rule{}, err
	}
	if i >= 0 && s.loadErr == nil && reflect.DeepEqual(f, s.file) {
		return r.clone(), nil
	}
	data, err := f.Encode()
	if err != nil {
		return Rule{}, err
	}
	if err := s.commitLocked(f, data); err != nil {
		return Rule{}, err
	}
	return r.clone(), nil
}

// Delete removes a rule.
func (s *RuleStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	f := s.file.clone()
	i := f.index(id)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	f.Rules = slices.Delete(f.Rules, i, i+1)
	f.normalize()
	data, err := f.Encode()
	if err != nil {
		return err
	}
	return s.commitLocked(f, data)
}

// ImportResult counts what an Import changed.
type ImportResult struct{ Added, Replaced, Unchanged, Removed int }

// Import adds rules in bulk (from a portfolios export). Rules with an id that already
// exists replace it; replace=true makes the file exactly the imported rules. The whole
// result is validated before anything is written, and the previous file is backed up.
func (s *RuleStore) Import(rules []Rule, replace bool) (ImportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	cur := s.file.clone()
	var res ImportResult
	next := File{Version: Version}
	if !replace {
		next = cur.clone()
	}
	for _, r := range rules {
		r = r.clone()
		r.normalize()
		if r.ID == "" {
			r.ID = uniqueID(autoID(r), func(id string) bool { return next.index(id) >= 0 })
		}
		switch i := cur.index(r.ID); {
		case i < 0:
			res.Added++
		case reflect.DeepEqual(cur.Rules[i], r):
			res.Unchanged++
		default:
			res.Replaced++
		}
		if i := next.index(r.ID); i >= 0 {
			next.Rules[i] = r
		} else {
			next.Rules = append(next.Rules, r)
		}
	}
	if replace {
		for _, r := range cur.Rules {
			if next.index(r.ID) < 0 {
				res.Removed++
			}
		}
	}
	next.normalize()
	if err := next.Validate(); err != nil {
		return ImportResult{}, err
	}
	if res.Added == 0 && res.Replaced == 0 && res.Removed == 0 && s.loadErr == nil {
		return res, nil
	}
	data, err := next.Encode()
	if err != nil {
		return ImportResult{}, err
	}
	if err := s.commitLocked(next, data); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}
