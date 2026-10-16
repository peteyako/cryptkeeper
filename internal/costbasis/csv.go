package costbasis

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseOptions tunes the CSV importers.
type ParseOptions struct {
	// SourceID overrides the default source id ("coinbase-csv" / "kraken-csv"). Pass the id
	// of the live source when the CSV backfills it, so (SourceID, ExternalID) de-duplicates
	// against API activity.
	SourceID string
	// OnSkip, when set, is told about every row that is ignored on purpose (fiat deposits,
	// transfers to Coinbase Pro, unknown transaction types, zero-amount ledger rows).
	OnSkip func(line int, reason string)
}

func resolveOptions(opts []ParseOptions, defaultSource string) ParseOptions {
	var o ParseOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if strings.TrimSpace(o.SourceID) == "" {
		o.SourceID = defaultSource
	}
	return o
}

func (o ParseOptions) skip(line int, format string, args ...any) {
	if o.OnSkip != nil {
		o.OnSkip(line, fmt.Sprintf(format, args...))
	}
}

// newCSVInput strips a UTF-8 byte order mark, which Excel and some exports prepend.
func newCSVInput(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	return br
}

// parseMoney parses "$1,234.56", "-$5", "$-5", "($5.00)" and plain numbers. Blank or
// unparsable input reports ok=false (the value is unknown, not zero).
func parseMoney(s string) (v float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, s[1:len(s)-1]
	}
	s = strings.NewReplacer("$", "", ",", "", " ", "", " ", "").Replace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if neg {
		f = -f
	}
	return f, true
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 UTC",
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

// parseTime parses the timestamp styles of the exports; zone-less values are UTC.
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

// headerIndex maps lower-cased, trimmed header names to column positions.
type headerIndex map[string]int

func newHeaderIndex(rec []string) headerIndex {
	h := make(headerIndex, len(rec))
	for i, name := range rec {
		name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))
		if _, dup := h[name]; !dup {
			h[name] = i
		}
	}
	return h
}

// col returns the first matching column or -1.
func (h headerIndex) col(names ...string) int {
	for _, n := range names {
		if i, ok := h[n]; ok {
			return i
		}
	}
	return -1
}

// prefixCol returns the first column whose name starts with prefix, or -1.
func (h headerIndex) prefixCol(prefix string) int {
	best := -1
	for name, i := range h {
		if strings.HasPrefix(name, prefix) && (best == -1 || i < best) {
			best = i
		}
	}
	return best
}

func field(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func fptr(v float64) *float64 { return &v }
