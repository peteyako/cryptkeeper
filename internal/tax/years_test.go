package tax

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newYearStore(t *testing.T) (*YearStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewYearStore(filepath.Join(dir, "tax"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	return s, filepath.Join(dir, "tax")
}

func TestYearStoreOrder(t *testing.T) {
	s, _ := newYearStore(t)
	if _, ok := s.LastClosed(); ok {
		t.Fatal("empty store has no last closed year")
	}
	for _, y := range []int{2025, 2026} {
		if err := s.Save(Year{Version: 1, Year: y, Source: "koinly", ClosedAt: at(y+1, 1, 5, 0)}); err != nil {
			t.Fatalf("Save %d: %v", y, err)
		}
	}
	if err := s.Save(Year{Year: 2025, Source: "koinly"}); err == nil {
		t.Error("Save 2025 while 2026 exists: want error")
	}
	if err := s.Delete(2025); err == nil {
		t.Error("Delete 2025 while 2026 exists: want error")
	}
	if got := s.Years(); !reflect.DeepEqual(got, []int{2025, 2026}) {
		t.Errorf("Years = %v", got)
	}
	if err := s.Delete(2026); err != nil {
		t.Fatal(err)
	}
	if y, ok := s.LastClosed(); !ok || y.Year != 2025 {
		t.Errorf("LastClosed = %d, %v", y.Year, ok)
	}
	if err := s.Save(Year{Year: 2025, Source: "koinly"}); err != nil {
		t.Errorf("re-Save of latest year: %v", err)
	}
}

func TestYearRoundTripPreservesTimes(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	s, dir := newYearStore(t)
	sold := time.Date(2025, 7, 4, 15, 4, 5, 0, ny)
	acq := time.Date(2024, 12, 31, 23, 59, 59, 0, ny)
	y := Year{
		Version: 1, Year: 2025, Source: "koinly", ClosedAt: sold,
		Disposals:     []Disposal{{Sold: sold, Acquired: acq, Various: true, Asset: "BTC", Amount: 1, Term: ShortTerm}},
		ClosingLots:   []Lot{{Wallet: "w", Asset: "BTC", Amount: 2, Acquired: acq}},
		ClosingParked: []Parked{{Wallet: "w", Asset: "JUP", Amount: 5}},
	}
	if err := s.Save(y); err != nil {
		t.Fatal(err)
	}
	s2, err := NewYearStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(2025)
	if !ok {
		t.Fatal("year not loaded")
	}
	if !got.Disposals[0].Sold.Equal(sold) || !got.Disposals[0].Acquired.Equal(acq) || !got.ClosedAt.Equal(sold) || !got.ClosingLots[0].Acquired.Equal(acq) {
		t.Fatalf("times changed: %+v", got)
	}
	if len(got.ClosingParked) != 1 {
		t.Fatalf("parked = %+v", got.ClosingParked)
	}
}

func TestYearRejectsBad(t *testing.T) {
	s, _ := newYearStore(t)
	if err := s.Save(Year{Year: 2025, ClosingLots: []Lot{{Wallet: "w", Asset: "A", Amount: 0}}}); err == nil {
		t.Error("zero-amount closing lot: want error")
	}
	if err := s.Save(Year{Year: 2025, ClosingParked: []Parked{{Wallet: "w", Asset: "A", Amount: 0}}}); err == nil {
		t.Error("dust parked: want error")
	}
	if err := s.Save(Year{Year: 0}); err == nil {
		t.Error("year 0: want error")
	}
	if len(s.Years()) != 0 {
		t.Error("rejected years must not be stored")
	}
}

func TestSummarize(t *testing.T) {
	d := []Disposal{
		{Term: ShortTerm, ProceedsUSD: 110, CostUSD: 100, GainUSD: 10},
		{Term: ShortTerm, ProceedsUSD: 96, CostUSD: 100, GainUSD: -4},
		{Term: LongTerm, ProceedsUSD: 17, CostUSD: 10, GainUSD: 7},
		{Term: ShortTerm, Note: "Fee", ProceedsUSD: 0, CostUSD: 1, GainUSD: 1},
	}
	got := Summarize(d, []Income{{ValueUSD: 3}})
	near(t, "short gain", got.ShortGain, 7) // the Fee row is a short-term row too
	near(t, "short proceeds", got.ShortProceeds, 206)
	near(t, "short cost", got.ShortCost, 201)
	near(t, "long proceeds", got.LongProceeds, 17)
	near(t, "long cost", got.LongCost, 10)
	near(t, "long gain", got.LongGain, 7)
	near(t, "fees", got.Fees, 1)
	near(t, "income", got.Income, 3)
}

func TestOpenYearStart(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	got := OpenYearStart(Year{Year: 2025}, true, ny)
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, ny); !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
	if !OpenYearStart(Year{}, false, ny).IsZero() {
		t.Error("!ok: want zero time")
	}
}
