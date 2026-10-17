package templates

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/portfolio"
)

func TestAxisLabels(t *testing.T) {
	for _, tc := range []struct {
		vals []float64
		want []string
	}{
		{[]float64{61234, 48000, 35000}, []string{"$61k", "$48k", "$35k"}},
		{[]float64{57250, 57230, 57210}, []string{"$57.25k", "$57.23k", "$57.21k"}},
		{[]float64{1500000, 1000000, 500000}, []string{"$1.5M", "$1.0M", "$0.5M"}},
		{[]float64{5210, 4800, 4300}, []string{"$5,210", "$4,800", "$4,300"}},
		{[]float64{182.4, 181.9, 181.4}, []string{"$182.4", "$181.9", "$181.4"}},
		{[]float64{0.9, 0.5, 0.1}, []string{"$0.9000", "$0.5000", "$0.1000"}},
	} {
		if got := AxisLabels(tc.vals, PriceFiat); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("AxisLabels(%v) = %v, want %v", tc.vals, got, tc.want)
		}
	}
}

func TestChartAxesOf(t *testing.T) {
	defer func(l *time.Location) { Location = l }(Location)
	Location = time.UTC
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var pts []portfolio.Point
	for i := range 8 {
		pts = append(pts, portfolio.Point{T: base.AddDate(0, 0, i), V: 1000 + float64(i)*100})
	}
	axes := ChartAxesOf(pts, 800, 220, Fiat)
	if len(axes.Y) != 3 || len(axes.X) != 3 {
		t.Fatalf("axes = %+v", axes)
	}
	// Gridlines sit at 25/50/75% of the height; the line spans 7.5%..92.5%, 1000..1700.
	if axes.Y[0] != "$1,556" || axes.Y[1] != "$1,350" || axes.Y[2] != "$1,144" {
		t.Errorf("Y = %v", axes.Y)
	}
	// The middle tick sits at 50%: between the 4th and 5th point, Oct 13 12:00 + 12h.
	if axes.X[0] != (ChartTick{0, "Oct 10"}) || axes.X[1] != (ChartTick{50, "Oct 14"}) || axes.X[2] != (ChartTick{100, "Oct 17"}) {
		t.Errorf("X = %+v", axes.X)
	}

	day := ChartAxesOf([]portfolio.Point{{T: base, V: 1}, {T: base.Add(6 * time.Hour), V: 2}}, 800, 220, Fiat)
	if day.X[0].Label != "12:00" || day.X[2].Label != "18:00" {
		t.Errorf("a short span shows clock times: %+v", day.X)
	}
	year := ChartAxesOf([]portfolio.Point{{T: base, V: 1}, {T: base.AddDate(1, 0, 0), V: 2}}, 800, 220, Fiat)
	if year.X[0].Label != "Oct 2026" || year.X[2].Label != "Oct 2027" {
		t.Errorf("a long span shows months: %+v", year.X)
	}

	flat := ChartAxesOf([]portfolio.Point{{T: base, V: 5}, {T: base.Add(time.Hour), V: 5}}, 800, 220, Fiat)
	if flat.Y[0] != "" || flat.Y[1] != "$5.00" || flat.Y[2] != "" {
		t.Errorf("flat series labels only the middle gridline: %v", flat.Y)
	}
	if a := ChartAxesOf(pts[:1], 800, 220, Fiat); len(a.Y) != 0 || len(a.X) != 0 {
		t.Errorf("one point has no axes: %+v", a)
	}
}

func TestChartHoverJSON(t *testing.T) {
	defer func(l *time.Location) { Location = l }(Location)
	Location = time.UTC
	base := time.Date(2026, 10, 10, 12, 30, 0, 0, time.UTC)
	raw := ChartHoverJSON([]portfolio.Point{{T: base, V: 10}, {T: base.Add(time.Hour), V: 20}}, 800, 220, Fiat)
	var hs []struct {
		T string  `json:"t"`
		V string  `json:"v"`
		Y float64 `json:"y"`
	}
	if err := json.Unmarshal([]byte(raw), &hs); err != nil || len(hs) != 2 {
		t.Fatalf("%v: %s", err, raw)
	}
	if hs[0].T != "Oct 10, 12:30" || hs[0].V != "$10.00" || hs[0].Y != 0.925 || hs[1].Y != 0.075 {
		t.Errorf("%+v", hs)
	}
	if ChartHoverJSON([]portfolio.Point{{T: base, V: 10}}, 800, 220, Fiat) != "" {
		t.Error("one point has no hover data")
	}
}

func TestGridAndTicks(t *testing.T) {
	if g := gridPath(800, 220); g != "M0 55H800M0 110H800M0 165H800" {
		t.Errorf("grid = %q", g)
	}
	for pct, want := range map[float64]string{0: "left:0", 100: "right:0", 50: "left:50.00%;transform:translateX(-50%)"} {
		if got := tickStyle(ChartTick{Pct: pct}); got != want {
			t.Errorf("tickStyle(%v) = %q, want %q", pct, got, want)
		}
	}
}

func TestSplitShort(t *testing.T) {
	if got := SplitShort(0.176); got != "82% / 18%" {
		t.Errorf("SplitShort = %q", got)
	}
}

func TestSplitText(t *testing.T) {
	for in, want := range map[float64]string{0: "100% invested · 0% stablecoins", 0.176: "82% invested · 18% stablecoins", 1: "0% invested · 100% stablecoins", 1.5: "0% invested · 100% stablecoins"} {
		if got := SplitText(in); got != want {
			t.Errorf("SplitText(%v) = %q, want %q", in, got, want)
		}
	}
}
