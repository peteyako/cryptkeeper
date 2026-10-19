package portfolios

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"cryptkeeper/internal/yamlfile"
)

// backupsKept is how many backups yamlfile.WriteAtomic retains.
const backupsKept = 30

// fileStamp identifies a version of the file on disk.
type fileStamp struct {
	exists bool
	mtime  time.Time
	size   int64
}

func (a fileStamp) same(b fileStamp) bool {
	return a.exists == b.exists && a.size == b.size && a.mtime.Equal(b.mtime)
}

// Store keeps the portfolios in data/portfolios.yaml. It is safe for concurrent use. Every
// read first checks the file's mtime so hand edits are picked up; a hand edit that no longer
// parses leaves the last good copy in place and is reported by LoadError. Every write
// validates the whole file, backs up the previous one and replaces it atomically; the cached
// copy only changes after the write succeeded.
type Store struct {
	path      string
	backupDir string
	base      string // backup base name, e.g. "portfolios"

	mu       sync.Mutex
	file     File
	stamp    fileStamp
	loadErr  error
	onChange []func()
}

// NewStore opens the store. A missing file is an empty store; an invalid one is an error.
func NewStore(path, backupDir string) (*Store, error) {
	s := &Store{
		path:      path,
		backupDir: backupDir,
		base:      strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		file:      File{Version: Version},
	}
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

// OnChange registers f, which is called in its own goroutine after every write and every
// detected hand edit. It may be called more than once to register several listeners.
func (s *Store) OnChange(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, f)
}

func (s *Store) stat() (fileStamp, error) {
	info, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{exists: true, mtime: info.ModTime(), size: info.Size()}, nil
}

func (s *Store) notifyLocked() {
	for _, f := range s.onChange {
		go f()
	}
}

// syncLocked reloads the file if it changed on disk since we last looked.
func (s *Store) syncLocked() {
	cur, err := s.stat()
	if err != nil {
		s.loadErr = fmt.Errorf("%s: %w", s.path, err)
		s.stamp = fileStamp{size: -1} // never matches, so the next successful stat reloads
		return
	}
	if cur.same(s.stamp) {
		return
	}
	s.stamp = cur // taken before reading, so an edit racing the read is caught next time
	oldFile, oldErr := s.file, s.loadErr

	switch {
	case !cur.exists:
		s.file, s.loadErr = File{Version: Version}, nil
	default:
		raw, err := os.ReadFile(s.path)
		if err == nil {
			var f File
			if f, err = Parse(raw); err == nil {
				s.file, s.loadErr = f, nil
			}
		}
		if err != nil {
			s.loadErr = fmt.Errorf("%s: %w", s.path, err)
		}
	}
	if !reflect.DeepEqual(oldFile, s.file) || (oldErr == nil) != (s.loadErr == nil) {
		s.notifyLocked()
	}
}

// LoadError is the parse error of the last hand edit that was rejected, or nil.
func (s *Store) LoadError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	return s.loadErr
}

// List returns a copy of the portfolios in file order.
func (s *Store) List() []Portfolio {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	return s.file.clone().Portfolios
}

// Get returns a copy of one portfolio.
func (s *Store) Get(id string) (Portfolio, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	if i := s.file.index(id); i >= 0 {
		return s.file.Portfolios[i].clone(), true
	}
	return Portfolio{}, false
}

// commitLocked writes data (which must be the encoding of f) and, only if that succeeded,
// makes f the cached copy.
func (s *Store) commitLocked(f File, data []byte) error {
	if err := yamlfile.WriteAtomic(s.path, data, s.backupDir, backupsKept); err != nil {
		return err
	}
	s.file, s.loadErr = f, nil
	if stamp, err := s.stat(); err == nil {
		s.stamp = stamp
	}
	s.notifyLocked()
	return nil
}

// writeLocked normalizes and validates f, encodes it and commits it.
func (s *Store) writeLocked(f File) error {
	f.normalize()
	if err := f.Validate(); err != nil {
		return err
	}
	data, err := f.Encode()
	if err != nil {
		return err
	}
	return s.commitLocked(f, data)
}

// Upsert adds or replaces a portfolio. With an empty ID a new id is derived from the name
// (Slug, made unique with -2, -3, …). An existing ID is replaced in place, keeping the order;
// an unknown non-empty ID is appended. The stored (normalized) portfolio is returned. The
// whole file is validated before anything is written.
func (s *Store) Upsert(p Portfolio) (Portfolio, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	f := s.file.clone()
	p = p.clone()
	p.normalize()
	if p.ID == "" {
		p.ID = uniqueID(Slug(p.Name), func(id string) bool { return f.index(id) >= 0 })
	}
	i := f.index(p.ID)
	if i >= 0 {
		f.Portfolios[i] = p
	} else {
		f.Portfolios = append(f.Portfolios, p)
	}
	f.normalize()
	if err := f.Validate(); err != nil {
		return Portfolio{}, err
	}
	if i >= 0 && s.loadErr == nil && reflect.DeepEqual(f, s.file) {
		return p.clone(), nil // nothing changed: no rewrite, no backup
	}
	data, err := f.Encode()
	if err != nil {
		return Portfolio{}, err
	}
	if err := s.commitLocked(f, data); err != nil {
		return Portfolio{}, err
	}
	return p.clone(), nil
}

// Delete removes a portfolio.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	f := s.file.clone()
	i := f.index(id)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	f.Portfolios = slices.Delete(f.Portfolios, i, i+1)
	return s.writeLocked(f)
}

// RemoveWallet drops walletID from every portfolio's explicit wallet list, for when a source
// is deleted. A portfolio that listed only that wallet is left selecting no wallets. It
// writes only if something changed and returns how many portfolios changed.
func (s *Store) RemoveWallet(walletID string) (changed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	f := s.file.clone()
	for i := range f.Portfolios {
		w := &f.Portfolios[i].Wallets
		if w.explicit(walletID) {
			w.IDs = slices.DeleteFunc(w.IDs, func(id string) bool { return id == walletID })
			changed++
		}
	}
	if changed == 0 {
		return 0, nil
	}
	if err := s.writeLocked(f); err != nil {
		return 0, err
	}
	return changed, nil
}

// Export renders one portfolio (as a complete file), or the whole file when id is "".
func (s *Store) Export(id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	f := s.file.clone()
	if id != "" {
		i := f.index(id)
		if i < 0 {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		f.Portfolios = []Portfolio{f.Portfolios[i]}
	}
	return f.Encode()
}

// ImportMode says how Import combines the imported file with the current one.
type ImportMode string

const (
	// ImportMerge replaces portfolios with the same id and appends new ones.
	ImportMerge ImportMode = "merge"
	// ImportReplace makes the file exactly the import.
	ImportReplace ImportMode = "replace"
)

// ImportResult counts what an import did, by portfolio id. Unchanged means the imported
// portfolio was identical to the existing one; Removed (replace only) counts ids dropped.
type ImportResult struct{ Added, Replaced, Unchanged, Removed int }

// Import parses and validates raw first (nothing is written on error), then merges or
// replaces. The previous file is backed up by the write. An import that would change nothing
// is not written. Empty input is rejected, so a blank upload cannot wipe a replace import.
func (s *Store) Import(raw []byte, mode ImportMode) (ImportResult, error) {
	if mode != ImportMerge && mode != ImportReplace {
		return ImportResult{}, fmt.Errorf("unknown import mode %q (use %q or %q)", mode, ImportMerge, ImportReplace)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return ImportResult{}, fmt.Errorf("the import file is empty")
	}
	in, err := Parse(raw)
	if err != nil {
		return ImportResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()

	var res ImportResult
	cur := s.file.clone()
	for _, p := range in.Portfolios {
		switch i := cur.index(p.ID); {
		case i < 0:
			res.Added++
		case reflect.DeepEqual(cur.Portfolios[i], p):
			res.Unchanged++
		default:
			res.Replaced++
		}
	}

	next := File{Version: Version}
	switch mode {
	case ImportReplace:
		for _, p := range cur.Portfolios {
			if in.index(p.ID) < 0 {
				res.Removed++
			}
		}
		next.Portfolios = in.clone().Portfolios
	default:
		next = cur
		for _, p := range in.clone().Portfolios {
			if i := next.index(p.ID); i >= 0 {
				next.Portfolios[i] = p
			} else {
				next.Portfolios = append(next.Portfolios, p)
			}
		}
	}

	if res.Added == 0 && res.Replaced == 0 && res.Removed == 0 && s.loadErr == nil {
		return res, nil
	}
	if err := s.writeLocked(next); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// Backups lists the saved copies of the file, newest first.
func (s *Store) Backups() ([]yamlfile.BackupInfo, error) {
	return yamlfile.ListBackups(s.backupDir, s.base)
}

// Restore makes the named backup (one of Backups()) the current file. The backup is
// validated first. The write backs up the current file, so a restore can be undone.
func (s *Store) Restore(name string) error {
	raw, err := yamlfile.ReadBackup(s.backupDir, s.base, name)
	if err != nil {
		return err
	}
	f, err := Parse(raw)
	if err != nil {
		return fmt.Errorf("backup %s: %w", name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked()
	return s.commitLocked(f, raw)
}
