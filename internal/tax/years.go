package tax

import (
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto-portfolio/internal/yamlfile"
)

const (
	yearVersion = 1
	yearBackups = 20
)

// Summary is the totals of a closed year, as printed on the report.
type Summary struct {
	ShortProceeds float64 `yaml:"shortProceeds"`
	ShortCost     float64 `yaml:"shortCost"`
	ShortGain     float64 `yaml:"shortGain"`
	LongProceeds  float64 `yaml:"longProceeds"`
	LongCost      float64 `yaml:"longCost"`
	LongGain      float64 `yaml:"longGain"`
	Income        float64 `yaml:"income"`
	Fees          float64 `yaml:"fees"`
}

// Year is a closed tax year: the frozen result plus the lots and parked balances the next
// year opens with.
type Year struct {
	Version   int        `yaml:"version"`
	Year      int        `yaml:"year"`
	Source    string     `yaml:"source"` // "koinly" | "cryptkeep"
	ClosedAt  time.Time  `yaml:"closedAt"`
	Summary   Summary    `yaml:"summary"`
	Disposals []Disposal `yaml:"disposals,omitempty"`
	Income    []Income   `yaml:"income,omitempty"`
	GiftsLost []Disposal `yaml:"giftsLost,omitempty"`
	// ClosingLots are the lots held at year end.
	ClosingLots []Lot `yaml:"closingLots,omitempty"`
	// ClosingParked are balances inside the user's own positions at year end (locked JUP,
	// perps collateral).
	ClosingParked []Parked `yaml:"closingParked,omitempty"`
}

// Summarize totals disposals by term and income. Every row, Note "Fee" rows included, is
// counted in the short/long proceeds, cost and gain totals by its Term, so the totals equal
// the Form 8949 / Schedule D totals built from the same rows. Fees is an informational
// subset: the sum of GainUSD over the Note "Fee" rows.
func Summarize(disposals []Disposal, income []Income) Summary {
	var s Summary
	for _, d := range disposals {
		if d.Note == "Fee" {
			s.Fees += d.GainUSD
		}
		switch {
		case d.Term == LongTerm:
			s.LongProceeds += d.ProceedsUSD
			s.LongCost += d.CostUSD
			s.LongGain += d.GainUSD
		default:
			s.ShortProceeds += d.ProceedsUSD
			s.ShortCost += d.CostUSD
			s.ShortGain += d.GainUSD
		}
	}
	for _, in := range income {
		s.Income += in.ValueUSD
	}
	return s
}

// OpenYearStart is the first instant of the year after last, in loc. It is the zero time
// when no year is closed.
func OpenYearStart(last Year, ok bool, loc *time.Location) time.Time {
	if !ok {
		return time.Time{}
	}
	return time.Date(last.Year+1, 1, 1, 0, 0, 0, 0, loc)
}

// Validate reports whether y can be stored.
func (y Year) Validate() error { return y.validate() }

func (y Year) validate() error {
	if y.Year < 1 || y.Year > 9999 {
		return fmt.Errorf("tax year %d: invalid year", y.Year)
	}
	switch y.Version {
	case 0, yearVersion:
	default:
		return fmt.Errorf("tax year %d: unsupported version %d (want %d)", y.Year, y.Version, yearVersion)
	}
	for i, l := range y.ClosingLots {
		if math.IsNaN(l.Amount) || math.IsInf(l.Amount, 0) || l.Amount <= 0 {
			return fmt.Errorf("tax year %d: closingLots[%d] (%s): amount must be a positive number", y.Year, i, l.Asset)
		}
	}
	for i, p := range y.ClosingParked {
		if !(p.Amount > dust) || math.IsInf(p.Amount, 0) {
			return fmt.Errorf("tax year %d: closingParked[%d] (%s): amount must be above dust", y.Year, i, p.Asset)
		}
	}
	return nil
}

func (y Year) clone() Year {
	y.Disposals = slices.Clone(y.Disposals)
	y.Income = slices.Clone(y.Income)
	y.GiftsLost = slices.Clone(y.GiftsLost)
	y.ClosingLots = slices.Clone(y.ClosingLots)
	y.ClosingParked = slices.Clone(y.ClosingParked)
	return y
}

// YearStore keeps one <year>.yaml per closed year in a directory. It loads the files at
// construction only: hand edits need a restart.
type YearStore struct {
	dir, backupDir string

	mu    sync.Mutex
	years map[int]Year
}

// NewYearStore loads every <year>.yaml in dir. A missing dir is an empty store; an invalid
// file is an error.
func NewYearStore(dir, backupDir string) (*YearStore, error) {
	s := &YearStore{dir: dir, backupDir: backupDir, years: map[int]Year{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		year, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var y Year
		if err := yamlfile.Decode(raw, &y); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if y.Year != year {
			return nil, fmt.Errorf("%s: file holds year %d", e.Name(), y.Year)
		}
		if err := y.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		y.Version = yearVersion
		s.years[year] = y
	}
	return s, nil
}

func (s *YearStore) path(year int) string { return filepath.Join(s.dir, strconv.Itoa(year)+".yaml") }

// sorted returns the closed years ascending. Caller holds s.mu.
func (s *YearStore) sorted() []int {
	return slices.Sorted(maps.Keys(s.years))
}

// Years returns the closed years, ascending.
func (s *YearStore) Years() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted()
}

// Get returns a copy of a closed year.
func (s *YearStore) Get(year int) (Year, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	y, ok := s.years[year]
	return y.clone(), ok
}

// LastClosed returns the latest closed year.
func (s *YearStore) LastClosed() (Year, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ys := s.sorted()
	if len(ys) == 0 {
		return Year{}, false
	}
	return s.years[ys[len(ys)-1]].clone(), true
}

// Save stores y, replacing the same year if present. It refuses when a later year is
// closed: years are closed and reopened in order.
func (s *YearStore) Save(y Year) error {
	if err := y.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ys := s.sorted(); len(ys) > 0 && ys[len(ys)-1] > y.Year {
		return fmt.Errorf("tax year %d: %d is already closed; close or reopen years in order", y.Year, ys[len(ys)-1])
	}
	y = y.clone()
	y.Version = yearVersion
	data, err := yamlfile.Encode(y)
	if err != nil {
		return err
	}
	if err := yamlfile.WriteAtomic(s.path(y.Year), data, s.backupDir, yearBackups); err != nil {
		return err
	}
	s.years[y.Year] = y
	return nil
}

// Delete removes the latest closed year (reopening it); the file is backed up first.
func (s *YearStore) Delete(year int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.years[year]; !ok {
		return fmt.Errorf("tax year %d is not closed", year)
	}
	if ys := s.sorted(); ys[len(ys)-1] != year {
		return fmt.Errorf("tax year %d: only the latest closed year (%d) can be reopened; close or reopen years in order", year, ys[len(ys)-1])
	}
	if s.backupDir != "" {
		if err := yamlfile.Backup(s.path(year), s.backupDir, yearBackups); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	if err := os.Remove(s.path(year)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(s.years, year)
	return nil
}

// ReplaceAll makes the closed years exactly years: every year is validated first, then the
// files are written and the files of years not present are removed (each overwritten or
// removed file is backed up first).
func (s *YearStore) ReplaceAll(years []Year) error {
	next := make(map[int]Year, len(years))
	for _, y := range years {
		if err := y.validate(); err != nil {
			return err
		}
		if _, dup := next[y.Year]; dup {
			return fmt.Errorf("tax year %d appears twice", y.Year)
		}
		y = y.clone()
		y.Version = yearVersion
		next[y.Year] = y
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for year := range s.years {
		if _, keep := next[year]; keep {
			continue
		}
		if s.backupDir != "" {
			if err := yamlfile.Backup(s.path(year), s.backupDir, yearBackups); err != nil {
				return fmt.Errorf("backup: %w", err)
			}
		}
	}
	for year, y := range next {
		data, err := yamlfile.Encode(y)
		if err != nil {
			return err
		}
		if err := yamlfile.WriteAtomic(s.path(year), data, s.backupDir, yearBackups); err != nil {
			return err
		}
		s.years[year] = y
	}
	for year := range s.years {
		if _, keep := next[year]; keep {
			continue
		}
		if err := os.Remove(s.path(year)); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(s.years, year)
	}
	return nil
}
