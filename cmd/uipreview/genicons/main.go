// Command genicons renders the CryptKeep app icon (the accent rounded square with the wallet
// glyph from the design boards) to PNG files. Run it once and commit the output:
//
//	go run ./cmd/uipreview/genicons -out web/static/icons
//
// It uses only the standard library: the glyph is rasterised from signed distance
// functions with 4x4 supersampling.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// Token values from the dark theme in web/static/tokens.css.
var (
	accent = [3]float64{0x6E, 0x9B, 0xFF} // --accent
	ink    = [3]float64{0x0D, 0x0F, 0x12} // --on-accent
)

type variant struct {
	file    string
	size    int
	rounded bool    // rounded square with transparent corners, else full bleed
	glyph   float64 // glyph box (24 units) as a fraction of the icon size
}

func main() {
	out := flag.String("out", "web/static/icons", "output directory")
	flag.Parse()

	variants := []variant{
		{"icon-192.png", 192, true, 20.0 / 36},
		{"icon-512.png", 512, true, 20.0 / 36},
		// Full bleed: the OS masks it. Glyph kept inside the maskable safe zone.
		{"icon-maskable-512.png", 512, false, 0.5},
		{"apple-touch-icon.png", 180, false, 20.0 / 36},
	}
	for _, v := range variants {
		img := render(v)
		path := filepath.Join(*out, v.file)
		f, err := os.Create(path)
		if err != nil {
			fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			fatal(err)
		}
		if err := f.Close(); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", path)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

const (
	strokeW = 2.2
	ss      = 4 // samples per pixel axis
)

func render(v variant) *image.NRGBA {
	n := v.size
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	unit := v.glyph * float64(n) / 24 // pixels per glyph unit
	off := (float64(n) - 24*unit) / 2 // glyph box is centred
	radius := 10.0 / 36 * float64(n)

	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if v.rounded && sdRoundRect(x-float64(n)/2, y-float64(n)/2, float64(n)/2, float64(n)/2, radius) > 0 {
						continue // outside the rounded square: transparent
					}
					c := accent
					gx, gy := (x-off)/unit, (y-off)/unit
					if inGlyph(gx, gy) {
						c = ink
					}
					r, g, b, a = r+c[0], g+c[1], b+c[2], a+1
				}
			}
			if a == 0 {
				continue
			}
			total := float64(ss * ss)
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(r / a)),
				G: uint8(math.Round(g / a)),
				B: uint8(math.Round(b / a)),
				A: uint8(math.Round(255 * a / total)),
			})
		}
	}
	return img
}

// inGlyph reports whether a point in glyph space (24x24 units) is inside the stroked
// wallet mark: a rounded rect outline, a divider line and a small ring.
func inGlyph(x, y float64) bool {
	half := strokeW / 2
	// rect x=3 y=6 w=18 h=14 rx=3, centred at (12, 13)
	if math.Abs(sdRoundRect(x-12, y-13, 9, 7, 3)) <= half {
		return true
	}
	// line from (3,10) to (21,10) with round caps
	if distSegment(x, y, 3, 10, 21, 10) <= half {
		return true
	}
	// circle at (16.5,15) r=1.5, stroked
	return math.Abs(math.Hypot(x-16.5, y-15)-1.5) <= half
}

// sdRoundRect is the signed distance to a rounded rectangle centred on the origin with
// half-extents (hx, hy) and corner radius r (negative inside).
func sdRoundRect(x, y, hx, hy, r float64) float64 {
	qx := math.Abs(x) - hx + r
	qy := math.Abs(y) - hy + r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

func distSegment(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}
