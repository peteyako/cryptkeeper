package alerts

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// fmtUSD renders a dollar amount: "$100,250", "$80.25", "$0.0123".
func fmtUSD(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	var s string
	switch {
	case v >= 1000:
		s = groupThousands(strconv.FormatFloat(v, 'f', 0, 64))
	case v >= 1 || v == 0:
		s = strconv.FormatFloat(v, 'f', 2, 64)
	default:
		dec := min(int(math.Ceil(-math.Log10(v)))+3, 10)
		s = strconv.FormatFloat(v, 'f', dec, 64)
		// keep at least two decimals, drop the rest of the trailing zeros
		for strings.HasSuffix(s, "0") && len(s) > strings.Index(s, ".")+3 {
			s = s[:len(s)-1]
		}
	}
	return sign + "$" + s
}

func groupThousands(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// fmtAmount renders a coin amount with up to 8 decimals and no trailing zeros.
func fmtAmount(v float64) string {
	s := strconv.FormatFloat(v, 'f', 8, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// fmtPct renders the magnitude of a percentage without sign or percent mark: "12", "3.4", "<0.1".
func fmtPct(p float64) string {
	p = math.Abs(p)
	switch {
	case p < 0.1:
		return "<0.1"
	case p >= 10:
		return strconv.FormatFloat(math.Round(p), 'f', 0, 64)
	}
	return strconv.FormatFloat(math.Round(p*10)/10, 'f', -1, 64)
}

// fmtWindow renders a window: "24h", "90m", "7d".
func fmtWindow(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// fmtOutage renders how long a source has been down: "61 min", "3h 5m".
func fmtOutage(d time.Duration) string {
	m := int(d.Minutes())
	if m < 120 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh %dm", m/60, m%60)
}
