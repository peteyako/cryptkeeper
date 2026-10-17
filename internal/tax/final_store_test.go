package tax

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestWalletBoxValidated: walletBox letters are upper-cased, and anything but G, H or I is
// refused with the wallet named.
func TestWalletBoxValidated(t *testing.T) {
	s, err := ParseSettings([]byte("version: 1\nwalletBox:\n  cb: h\n  kr: G\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.WalletBox["cb"] != "H" || s.WalletBox["kr"] != "G" {
		t.Errorf("walletBox = %v, want upper case", s.WalletBox)
	}
	for _, bad := range []string{"J", "C", "", "gh"} {
		_, err := ParseSettings([]byte("version: 1\nwalletBox:\n  phantom: \"" + bad + "\"\n"))
		if err == nil || !strings.Contains(err.Error(), "walletBox[phantom]") {
			t.Errorf("walletBox %q: err = %v, want one naming the wallet", bad, err)
		}
	}
}

// validYear is a closed year that passes validation; tests break one field at a time.
func validYear() Year {
	y := Year{
		Year: 2025, Source: "cryptkeep",
		Disposals:   []Disposal{{Sold: at(2025, 5, 1, 0), Acquired: at(2025, 1, 1, 0), Asset: "SOL", Amount: 1, CostUSD: 10, ProceedsUSD: 20, GainUSD: 10, Wallet: "w", Term: ShortTerm}},
		Income:      []Income{{Date: at(2025, 6, 1, 0), Asset: "SOL", Amount: 1, ValueUSD: 5, Type: "reward", Wallet: "w"}},
		ClosingLots: []Lot{{Wallet: "w", Asset: "SOL", Amount: 2, CostUSD: 30, Acquired: at(2025, 1, 1, 0)}},
	}
	y.Summary = Summarize(y.Disposals, y.Income)
	return y
}

// TestYearValidationThorough: closing lots, disposals, gifts and income are checked for
// sane numbers, terms and identity, and the timezone must load.
func TestYearValidationThorough(t *testing.T) {
	if err := validYear().Validate(); err != nil {
		t.Fatalf("valid year refused: %v", err)
	}
	nan := math.NaN()
	cases := map[string]func(*Year){
		"lot cost negative":   func(y *Year) { y.ClosingLots[0].CostUSD = -1 },
		"lot cost NaN":        func(y *Year) { y.ClosingLots[0].CostUSD = nan },
		"lot wallet empty":    func(y *Year) { y.ClosingLots[0].Wallet = "" },
		"lot asset empty":     func(y *Year) { y.ClosingLots[0].Asset = "" },
		"disposal NaN":        func(y *Year) { y.Disposals[0].ProceedsUSD = nan },
		"disposal Inf":        func(y *Year) { y.Disposals[0].GainUSD = math.Inf(1) },
		"disposal amount NaN": func(y *Year) { y.Disposals[0].Amount = nan },
		"disposal term":       func(y *Year) { y.Disposals[0].Term = "medium" },
		"gift term":           func(y *Year) { y.GiftsLost = []Disposal{y.Disposals[0]}; y.GiftsLost[0].Term = "" },
		"income NaN":          func(y *Year) { y.Income[0].ValueUSD = nan },
		"income amount Inf":   func(y *Year) { y.Income[0].Amount = math.Inf(-1) },
		"bad timezone":        func(y *Year) { y.Timezone = "Mars/Olympus_Mons" },
	}
	for name, breakIt := range cases {
		y := validYear()
		breakIt(&y)
		if err := y.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestOpenYearStartUsesClosingZone: the open year starts at Jan 1 in the zone the last year
// was closed in, whatever the current tax zone.
func TestOpenYearStartUsesClosingZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	if got := OpenYearStart(Year{Year: 2025, Timezone: "UTC"}, true, ny); !got.Equal(at(2026, 1, 1, 0)) {
		t.Errorf("start = %v, want 2026-01-01 UTC", got)
	}
	if got := OpenYearStart(Year{Year: 2025}, true, ny); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, ny)) {
		t.Errorf("start without a stored zone = %v, want Jan 1 in the current zone", got)
	}
}
