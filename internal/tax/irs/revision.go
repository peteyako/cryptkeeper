package irs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	embeddedYear = 2025
	irsFormsURL  = "https://www.irs.gov/pub/irs-pdf/"
	file8949     = "f8949.pdf"
	fileSchedD   = "f1040sd.pdf"
	// unavailableMarker in <FormsDir>/<year> records a failed or rejected download; no
	// download is tried again while it is younger than unavailableFor.
	unavailableMarker = ".unavailable"
	unavailableFor    = 24 * time.Hour
	maxFormSize       = 20 << 20
)

// revision is the pair of blank forms a year is filled on.
type revision struct {
	f8949, schedD template
	// note is stamped on the first page when the forms are not the year's own revision.
	note string
}

var embeddedRevision = sync.OnceValues(func() (revision, error) {
	var rev revision
	for _, x := range []struct {
		name string
		t    *template
	}{{file8949, &rev.f8949}, {fileSchedD, &rev.schedD}} {
		pdf, err := embeddedForms.ReadFile("forms/2025/" + x.name)
		if err != nil {
			return revision{}, err
		}
		if *x.t, err = inspect(pdf); err != nil {
			return revision{}, fmt.Errorf("embedded %s: %w", x.name, err)
		}
	}
	return rev, nil
})

// revisionFor picks the forms for a tax year: the embedded 2025 revision for 2025; for a
// later year the <FormsDir>/<year> forms (downloaded from irs.gov when absent) if they are
// that year's revision with the 2025 fields. Any other year is filled on the embedded
// revision with a note on the first page. Only a done ctx is an error.
func revisionFor(ctx context.Context, year int, opts Options) (revision, error) {
	base, err := embeddedRevision()
	if err != nil || year == embeddedYear {
		return base, err
	}
	if year > embeddedYear {
		rev, ok, err := opts.newerRevision(ctx, year, base)
		if err != nil || ok {
			return rev, err
		}
	}
	base.note = fmt.Sprintf("Filled on the %d form revision; check irs.gov for the %d form.", embeddedYear, year)
	return base, nil
}

// accept returns the revision of a downloaded or cached pair if both forms are the year's
// revision with exactly the embedded fields.
func accept(year int, f8949, schedD []byte, base revision) (revision, error) {
	var rev revision
	for _, x := range []struct {
		name string
		pdf  []byte
		base template
		t    *template
	}{{file8949, f8949, base.f8949, &rev.f8949}, {fileSchedD, schedD, base.schedD, &rev.schedD}} {
		t, err := inspect(x.pdf)
		switch {
		case err != nil:
			return revision{}, fmt.Errorf("%s: %w", x.name, err)
		case t.year != year:
			return revision{}, fmt.Errorf("%s is the %d revision", x.name, t.year)
		case !t.sameFields(x.base):
			return revision{}, fmt.Errorf("%s fields differ from the %d revision", x.name, embeddedYear)
		}
		*x.t = t
	}
	return rev, nil
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// newerRevision returns the year's forms from FormsDir, downloading them once when absent.
// A cached pair that is rejected is removed so a download can replace it. A failed or
// rejected download leaves the unavailable marker, which holds off further downloads for
// 24 hours; accepting a revision clears it. Only a done ctx is an error.
func (o Options) newerRevision(ctx context.Context, year int, base revision) (revision, bool, error) {
	if o.FormsDir == "" {
		return revision{}, false, nil
	}
	dir := filepath.Join(o.FormsDir, strconv.Itoa(year))
	paths := [2]string{filepath.Join(dir, file8949), filepath.Join(dir, fileSchedD)}
	marker := filepath.Join(dir, unavailableMarker)

	f8949, err1 := os.ReadFile(paths[0])
	schedD, err2 := os.ReadFile(paths[1])
	if err1 == nil && err2 == nil {
		if rev, err := accept(year, f8949, schedD, base); err == nil {
			_ = os.Remove(marker)
			return rev, true, nil
		}
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
	if o.HTTPClient == nil || o.recentlyUnavailable(marker) {
		return revision{}, false, nil
	}

	f8949, err := o.download(ctx, file8949)
	if err == nil {
		schedD, err = o.download(ctx, fileSchedD)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return revision{}, false, ctxErr // the caller went away; irs.gov may be fine
	}
	var rev revision
	if err == nil {
		rev, err = accept(year, f8949, schedD, base)
	}
	if err != nil {
		o.markUnavailable(marker, err)
		return revision{}, false, nil
	}
	// A failed cache write only means downloading again next time.
	_ = writeCache(dir, map[string][]byte{file8949: f8949, fileSchedD: schedD})
	_ = os.Remove(marker)
	return rev, true, nil
}

// recentlyUnavailable reports whether the marker is younger than 24 hours (a marker dated
// in the future, after a clock change, does not count).
func (o Options) recentlyUnavailable(marker string) bool {
	fi, err := os.Stat(marker)
	if err != nil {
		return false
	}
	age := o.now().Sub(fi.ModTime())
	return age >= 0 && age < unavailableFor
}

// markUnavailable writes the marker, dated now and holding the reason.
func (o Options) markUnavailable(marker string, reason error) {
	if os.MkdirAll(filepath.Dir(marker), 0o755) != nil {
		return
	}
	if os.WriteFile(marker, []byte(reason.Error()+"\n"), 0o644) != nil {
		return
	}
	now := o.now()
	_ = os.Chtimes(marker, now, now)
}

func (o Options) download(ctx context.Context, name string) ([]byte, error) {
	base := o.FormsURL
	if base == "" {
		base = irsFormsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", name, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFormSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFormSize {
		return nil, errors.New(name + ": too large")
	}
	return b, nil
}

// writeCache writes each file atomically (temp file + rename) so a concurrent reader never
// sees a partial form.
func writeCache(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range files {
		tmp, err := os.CreateTemp(dir, name+".*.tmp")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if err := errors.Join(werr, cerr); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
			os.Remove(tmp.Name())
			return err
		}
	}
	return nil
}
