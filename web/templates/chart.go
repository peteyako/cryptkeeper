package templates

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/portfolio"
)

// chartGeom is the geometry SVGPath, the axes and the hover data share for one chart: the
// finite points spread evenly along x, and the value scale that pads the line to about
// 85% of the height (7.5% above, 7.5% below). A flat series sits in the middle.
type chartGeom struct {
	pts    []portfolio.Point // the finite points, in order
	lo, hi float64
	w, h   float64
}

func newChartGeom(points []portfolio.Point, w, h float64) chartGeom {
	g := chartGeom{w: w, h: h}
	for _, p := range points {
		if finite(p.V) {
			g.pts = append(g.pts, p)
		}
	}
	if len(g.pts) == 0 {
		return g
	}
	g.lo, g.hi = g.pts[0].V, g.pts[0].V
	for _, p := range g.pts {
		g.lo, g.hi = math.Min(g.lo, p.V), math.Max(g.hi, p.V)
	}
	return g
}

func (g chartGeom) top() float64    { return g.h * 0.075 }
func (g chartGeom) usable() float64 { return g.h * 0.85 }

// y maps a value to a viewBox y.
func (g chartGeom) y(v float64) float64 {
	if g.hi == g.lo {
		return g.h / 2
	}
	return g.top() + g.usable() - (v-g.lo)/(g.hi-g.lo)*g.usable()
}

// value is the inverse of y: the value drawn at a viewBox y (NaN for a flat series).
func (g chartGeom) value(y float64) float64 {
	if g.hi == g.lo {
		return math.NaN()
	}
	return g.lo + (g.top()+g.usable()-y)/g.usable()*(g.hi-g.lo)
}

// x maps a point index to a viewBox x.
func (g chartGeom) x(i int) float64 {
	if len(g.pts) < 2 {
		return 0
	}
	return float64(i) * g.w / float64(len(g.pts)-1)
}

// span is the time between the first and last point.
func (g chartGeom) span() time.Duration {
	if len(g.pts) < 2 {
		return 0
	}
	return g.pts[len(g.pts)-1].T.Sub(g.pts[0].T)
}

// ChartTick is one x-axis label and where it sits, as a percentage of the plot width.
type ChartTick struct {
	Pct   float64
	Label string
}

// ChartAxes are the labels drawn around a chart: Y has one label per gridline, top to
// bottom (an empty label is a gridline without a value), X the ticks along the bottom.
type ChartAxes struct {
	Y []string
	X []ChartTick
}

// gridlineFractions are the gridlines' heights as fractions of the chart, top to bottom;
// chartFigure draws them at the same places.
var gridlineFractions = []float64{0.25, 0.5, 0.75}

// ChartAxesOf labels a chart's three gridlines with the values drawn there and its x axis
// with the time at its left edge, middle and right edge (points are spread evenly, so the
// middle time is interpolated between its neighbours). format turns a value into text
// (Fiat for a portfolio, PriceFiat for a coin); the gridline labels are compacted and
// never duplicate each other. A flat series labels only the middle gridline, with its
// value. Fewer than two points have no axes.
func ChartAxesOf(points []portfolio.Point, w, h float64, format func(float64) string) ChartAxes {
	g := newChartGeom(points, w, h)
	if len(g.pts) < 2 {
		return ChartAxes{}
	}
	var axes ChartAxes
	if g.hi == g.lo {
		axes.Y = []string{"", format(g.lo), ""}
	} else {
		vals := make([]float64, len(gridlineFractions))
		for i, f := range gridlineFractions {
			vals[i] = g.value(h * f)
		}
		axes.Y = AxisLabels(vals, format)
	}
	layout := axisTimeLayout(g.span())
	for _, f := range []float64{0, 0.5, 1} {
		axes.X = append(axes.X, ChartTick{Pct: f * 100, Label: Local(g.timeAt(f)).Format(layout)})
	}
	return axes
}

// timeAt is the time at a fraction of the plot width, interpolated between the two
// points either side.
func (g chartGeom) timeAt(f float64) time.Time {
	n := len(g.pts)
	pos := f * float64(n-1)
	i := int(math.Floor(pos))
	if i >= n-1 {
		return g.pts[n-1].T
	}
	frac := pos - float64(i)
	return g.pts[i].T.Add(time.Duration(frac * float64(g.pts[i+1].T.Sub(g.pts[i].T))))
}

// axisTimeLayout picks the x-axis time format for a span: clock times within two days,
// day and month up to half a year, month and year beyond.
func axisTimeLayout(span time.Duration) string {
	switch {
	case span <= 48*time.Hour:
		return "15:04"
	case span <= 183*24*time.Hour:
		return "Jan 2"
	}
	return "Jan 2006"
}

// hoverTimeLayout is the tooltip's time format: with the clock time within two days.
func hoverTimeLayout(span time.Duration) string {
	if span <= 48*time.Hour {
		return "Jan 2, 15:04"
	}
	return "Jan 2, 2006"
}

// AxisLabels formats gridline values compactly: "$1.2M", "$57.2k", "$182", or format(v)
// below $10 (prices need their decimals). The decimals (up to three with a suffix, two
// without) are the fewest that resolve half the spacing between the values, so neighbours
// never round to the same label; the sign is the Fiat minus.
func AxisLabels(vals []float64, format func(float64) string) []string {
	out := make([]string, len(vals))
	maxAbs, step := 0.0, math.Inf(1)
	for i, v := range vals {
		maxAbs = math.Max(maxAbs, math.Abs(v))
		if i > 0 {
			step = math.Min(step, math.Abs(v-vals[i-1]))
		}
	}
	if maxAbs < 10 || !finite(maxAbs) {
		for i, v := range vals {
			out[i] = format(v)
		}
		return out
	}
	div, suffix, maxDec := 1.0, "", 2
	switch {
	case maxAbs >= 1e6:
		div, suffix, maxDec = 1e6, "M", 3
	case maxAbs >= 1e4:
		div, suffix, maxDec = 1e3, "k", 3
	}
	dec := 0
	for dec < maxDec && math.Pow(10, -float64(dec)) > step/div/2 {
		dec++
	}
	for i, v := range vals {
		s := "$" + groupDecimal(strconv.FormatFloat(math.Abs(v)/div, 'f', dec, 64)) + suffix
		if v < 0 && !isZeroDecimal(s) {
			s = Minus + s
		}
		out[i] = s
	}
	return out
}

// chartHover is one point's hover data: its time, value text and the line's height there
// as a fraction of the chart (0 top, 1 bottom). Points are evenly spaced along x, so the
// index is the position.
type chartHover struct {
	T string  `json:"t"`
	V string  `json:"v"`
	Y float64 `json:"y"`
}

// ChartHoverJSON is the hover data for chart.js, a JSON array with one entry per drawn
// point, or "" with fewer than two points.
func ChartHoverJSON(points []portfolio.Point, w, h float64, format func(float64) string) string {
	g := newChartGeom(points, w, h)
	if len(g.pts) < 2 {
		return ""
	}
	layout := hoverTimeLayout(g.span())
	hs := make([]chartHover, len(g.pts))
	for i, p := range g.pts {
		hs[i] = chartHover{T: Local(p.T).Format(layout), V: format(p.V), Y: math.Round(g.y(p.V)/h*1000) / 1000}
	}
	b, _ := json.Marshal(hs)
	return string(b)
}

// SVGPath turns chart points into SVG path data for a viewBox of w by h, like the
// Portfolio board: points are spread evenly along x, and y is padded so the line uses
// about 85% of the height (7.5% above, 7.5% below). The area path is the line closed down
// to the bottom edge. A flat series is drawn through the middle. Non-finite values are
// skipped; with no usable points both results are empty.
func SVGPath(points []portfolio.Point, w, h float64) (line, area string) {
	g := newChartGeom(points, w, h)
	n := len(g.pts)
	if n == 0 {
		return "", ""
	}
	var b strings.Builder
	if n == 1 {
		y := num1(g.y(g.pts[0].V))
		b.WriteString("M0 " + y + " L" + num1(w) + " " + y)
	} else {
		for i, p := range g.pts {
			if i == 0 {
				b.WriteByte('M')
			} else {
				b.WriteString(" L")
			}
			b.WriteString(num1(g.x(i)))
			b.WriteByte(' ')
			b.WriteString(num1(g.y(p.V)))
		}
	}
	line = b.String()
	area = line + " L" + numTrim(w) + " " + numTrim(h) + " L0 " + numTrim(h) + " Z"
	return line, area
}

// gridPath is the SVG path of the gridlines for a viewBox of w by h.
func gridPath(w, h float64) string {
	var b strings.Builder
	for _, f := range gridlineFractions {
		b.WriteString("M0 " + numTrim(h*f) + "H" + numTrim(w))
	}
	return b.String()
}

// tickStyle positions an x-axis tick: the first sits at the left edge, the last at the
// right, the rest centred on their position.
func tickStyle(t ChartTick) string {
	switch {
	case t.Pct <= 0:
		return "left:0"
	case t.Pct >= 100:
		return "right:0"
	}
	return "left:" + strconv.FormatFloat(t.Pct, 'f', 2, 64) + "%;transform:translateX(-50%)"
}
