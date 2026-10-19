package sources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/yamlfile"
)

// Factory builds a live source from a definition whose secrets have been opened.
type Factory func(def Def) (core.Source, error)

// Entry is a definition with its live source (nil if building it failed).
type Entry struct {
	Def      Def
	Source   core.Source
	BuildErr error
}

// Registry owns data/sources.yaml. It is safe for concurrent use.
type Registry struct {
	path      string
	backupDir string
	box       *secrets.Box
	factory   Factory
	onChange  func()

	mu    sync.Mutex
	file  File
	mtime time.Time
	built map[string]builtSource // by id; rebuilt only when the def changes
}

type builtSource struct {
	hash string
	src  core.Source
	err  error
}

// NewRegistry loads path (a missing file means no sources yet).
func NewRegistry(path, backupDir string, box *secrets.Box, factory Factory) (*Registry, error) {
	r := &Registry{path: path, backupDir: backupDir, box: box, factory: factory, built: map[string]builtSource{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadLocked(true); err != nil {
		return nil, err
	}
	return r, nil
}

// OnChange registers a callback run (in a new goroutine) after every save or reload.
func (r *Registry) OnChange(f func()) { r.mu.Lock(); r.onChange = f; r.mu.Unlock() }

// reloadLocked re-reads the file when its mtime changed (hand edits) or force is set.
func (r *Registry) reloadLocked(force bool) error {
	st, err := os.Stat(r.path)
	if os.IsNotExist(err) {
		r.file = File{Version: 1}
		r.mtime = time.Time{}
		return nil
	}
	if err != nil {
		return err
	}
	if !force && st.ModTime().Equal(r.mtime) {
		return nil
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var f File
	if err := yamlfile.Decode(raw, &f); err != nil {
		return fmt.Errorf("%s: %w", r.path, err)
	}
	if f.Version == 0 {
		f.Version = 1
	}
	seen := map[string]bool{}
	for i := range f.Sources {
		d := &f.Sources[i]
		if seen[d.ID] {
			return fmt.Errorf("%s: duplicate source id %q", r.path, d.ID)
		}
		seen[d.ID] = true
		if d.Kind == "" {
			d.Kind = DefaultKind(d.Type)
		}
	}
	changed := !r.mtime.IsZero()
	r.file, r.mtime = f, st.ModTime()
	if changed && r.onChange != nil {
		go r.onChange()
	}
	return nil
}

// Entries returns every source in file order, building (or reusing) live instances.
func (r *Registry) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.reloadLocked(false) // on a bad hand edit, keep serving the last good file
	out := make([]Entry, 0, len(r.file.Sources))
	live := map[string]bool{}
	for _, d := range r.file.Sources {
		live[d.ID] = true
		h := defHash(d)
		b, ok := r.built[d.ID]
		if !ok || b.hash != h {
			opened, err := r.openLocked(d)
			if err == nil {
				b.src, b.err = r.factory(opened)
			} else {
				b.src, b.err = nil, err
			}
			b.hash = h
			r.built[d.ID] = b
		}
		out = append(out, Entry{Def: d, Source: b.src, BuildErr: b.err})
	}
	for id := range r.built {
		if !live[id] {
			delete(r.built, id)
		}
	}
	return out
}

// Get returns the definition (secrets still sealed) for id.
func (r *Registry) Get(id string) (Def, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.reloadLocked(false)
	for _, d := range r.file.Sources {
		if d.ID == id {
			return d, true
		}
	}
	return Def{}, false
}

// Defs returns all definitions (secrets sealed).
func (r *Registry) Defs() []Def {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.reloadLocked(false)
	return slices.Clone(r.file.Sources)
}

// Open returns a copy of d with secret fields decrypted.
func (r *Registry) Open(d Def) (Def, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openLocked(d)
}

func (r *Registry) openLocked(d Def) (Def, error) {
	for _, name := range SecretFields {
		p := d.Secret(name)
		if *p == "" {
			continue
		}
		plain, err := r.box.Open(*p)
		if err != nil {
			return d, fmt.Errorf("%s: %w (was data/secret.key or CRYPTKEEP_SECRET_KEY changed?)", d.ID, err)
		}
		*p = plain
	}
	return d, nil
}

// Build constructs a live source from a definition with plaintext secrets (for "Test
// connection" before saving).
func (r *Registry) Build(plain Def) (core.Source, error) { return r.factory(plain) }

// Upsert validates and saves a definition given with PLAINTEXT secrets. For an existing
// id, empty secret fields keep the stored value. Returns the stored (sealed) definition.
func (r *Registry) Upsert(plain Def) (Def, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadLocked(false); err != nil {
		return Def{}, err
	}
	idx := slices.IndexFunc(r.file.Sources, func(d Def) bool { return d.ID == plain.ID })
	sealed := plain
	for _, name := range SecretFields {
		p := sealed.Secret(name)
		if *p == "" && idx >= 0 {
			*p = *r.file.Sources[idx].Secret(name)
			continue
		}
		if *p != "" && !secrets.IsSealed(*p) {
			s, err := r.box.Seal(*p)
			if err != nil {
				return Def{}, err
			}
			*p = s
		}
	}
	if sealed.Kind == "" {
		sealed.Kind = DefaultKind(sealed.Type)
	}
	if idx >= 0 {
		sealed.CreatedAt = r.file.Sources[idx].CreatedAt
	} else if sealed.CreatedAt.IsZero() {
		sealed.CreatedAt = time.Now().UTC().Truncate(time.Second)
	}
	if err := sealed.Validate(); err != nil {
		return Def{}, err
	}
	next := r.file
	next.Sources = slices.Clone(r.file.Sources)
	if idx >= 0 {
		next.Sources[idx] = sealed
	} else {
		next.Sources = append(next.Sources, sealed)
	}
	if err := r.writeLocked(next); err != nil {
		return Def{}, err
	}
	return sealed, nil
}

// ImportResult counts what an Import changed; Removed lists the ids it deleted so callers
// can clean up portfolios and activity.
type ImportResult struct {
	Added, Replaced, Unchanged int
	Removed                    []string
}

// Import merges (or, with replace, substitutes) the sources in f in one validated write.
// Secrets may be plaintext (sealed here), sealed with this install's key (kept), or blank
// (an existing source keeps its stored secret; a new one is saved without it).
func (r *Registry) Import(f File, replace bool) (ImportResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadLocked(false); err != nil {
		return ImportResult{}, err
	}
	var res ImportResult
	next := File{Version: 1}
	if !replace {
		next.Sources = slices.Clone(r.file.Sources)
	}
	seen := map[string]bool{}
	for _, d := range f.Sources {
		if seen[d.ID] {
			return ImportResult{}, fmt.Errorf("duplicate source id %q in the import", d.ID)
		}
		seen[d.ID] = true
		cur := slices.IndexFunc(r.file.Sources, func(x Def) bool { return x.ID == d.ID })
		for _, name := range SecretFields {
			p := d.Secret(name)
			switch {
			case *p == "" && cur >= 0:
				*p = *r.file.Sources[cur].Secret(name)
			case *p != "" && !secrets.IsSealed(*p):
				sealed, err := r.box.Seal(*p)
				if err != nil {
					return ImportResult{}, err
				}
				*p = sealed
			}
		}
		if d.Kind == "" {
			d.Kind = DefaultKind(d.Type)
		}
		if err := d.Validate(); err != nil {
			return ImportResult{}, err
		}
		switch {
		case cur < 0:
			res.Added++
		case defHash(r.file.Sources[cur]) == defHash(d):
			res.Unchanged++
		default:
			res.Replaced++
		}
		if i := slices.IndexFunc(next.Sources, func(x Def) bool { return x.ID == d.ID }); i >= 0 {
			next.Sources[i] = d
		} else {
			next.Sources = append(next.Sources, d)
		}
	}
	if replace {
		for _, d := range r.file.Sources {
			if !seen[d.ID] {
				res.Removed = append(res.Removed, d.ID)
			}
		}
	}
	if res.Added == 0 && res.Replaced == 0 && len(res.Removed) == 0 {
		return res, nil
	}
	if err := r.writeLocked(next); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// ParseFile strictly decodes a sources document (line-numbered errors).
func ParseFile(raw []byte) (File, error) {
	var f File
	if err := yamlfile.Decode(raw, &f); err != nil {
		return File{}, err
	}
	if f.Version == 0 {
		f.Version = 1
	}
	return f, nil
}

// Delete removes a source.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadLocked(false); err != nil {
		return err
	}
	next := r.file
	next.Sources = slices.DeleteFunc(slices.Clone(r.file.Sources), func(d Def) bool { return d.ID == id })
	if len(next.Sources) == len(r.file.Sources) {
		return fmt.Errorf("unknown source %q", id)
	}
	return r.writeLocked(next)
}

// UniqueID returns an unused id derived from label.
func (r *Registry) UniqueID(label string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	base := Slug(label)
	id := base
	for n := 2; slices.ContainsFunc(r.file.Sources, func(d Def) bool { return d.ID == id }); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// Export returns sources.yaml content. Without includeSecrets, secret fields are blanked.
func (r *Registry) Export(includeSecrets bool) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.file
	f.Sources = slices.Clone(f.Sources)
	if !includeSecrets {
		for i := range f.Sources {
			for _, name := range SecretFields {
				*f.Sources[i].Secret(name) = ""
			}
		}
	}
	return yamlfile.Encode(f)
}

func (r *Registry) writeLocked(f File) error {
	raw, err := yamlfile.Encode(f)
	if err != nil {
		return err
	}
	if err := yamlfile.WriteAtomic(r.path, raw, r.backupDir, 30); err != nil {
		return err
	}
	r.file = f
	if st, err := os.Stat(r.path); err == nil {
		r.mtime = st.ModTime()
	}
	if r.onChange != nil {
		go r.onChange()
	}
	return nil
}

// Path returns the file path (for messages).
func (r *Registry) Path() string { return filepath.Clean(r.path) }

// defHash identifies what a live source is built from; the description isn't part of it,
// so editing one doesn't restart the wallet's sync.
func defHash(d Def) string {
	d.Description = ""
	raw, _ := yamlfile.Encode(d)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
