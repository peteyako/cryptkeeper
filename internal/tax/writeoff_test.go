package tax

import (
	"math"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

func TestComputeToExcludesLater(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, ny)
	early := mk("early", "w", time.Date(2026, 12, 31, 12, 0, 0, 0, ny), core.TxSend, lg("SOL", -1, usd(200)))
	late := mk("late", "w", time.Date(2027, 1, 2, 12, 0, 0, 0, ny), core.TxSend, lg("SOL", -1, usd(300)))
	r := Compute(Input{
		From: from2026, To: to, Loc: ny,
		Opening: []Lot{lot("w", "SOL", 3, 300, at(2025, 1, 1, 0))},
		Manual:  []Lot{{Wallet: "w", Asset: "SOL", Amount: 5, CostUSD: 1, Acquired: time.Date(2027, 2, 1, 0, 0, 0, 0, ny)}},
		Txs:     []core.Tx{early, late},
		WriteOffs: []WriteOff{
			{Wallet: "w", Asset: "SOL", Amount: 1, Date: time.Date(2027, 3, 1, 0, 0, 0, 0, ny)},
		},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].TxID != early.ID {
		t.Fatalf("disposals = %+v, want only the early sale", r.Disposals)
	}
	near(t, "held", heldIn(r, "w", "SOL"), 2)
}

func writeOffInput(amount, proceeds float64) Input {
	dep := mk("dep", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -2, usd(-600)))
	return Input{
		From: from2026, Opening: []Lot{lot("w", "SOL", 2, 300, at(2025, 1, 1, 0))},
		Txs:       []core.Tx{dep},
		Tags:      map[string]Tag{dep.ID: TagPosition},
		WriteOffs: []WriteOff{{Wallet: "w", Asset: "SOL", Amount: amount, Date: at(2026, 6, 1, 0), ProceedsUSD: proceeds}},
	}
}

func TestWriteOffDisposesParked(t *testing.T) {
	r := Compute(writeOffInput(2, 50))
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v", r.Disposals)
	}
	d := r.Disposals[0]
	near(t, "amount", d.Amount, 2)
	near(t, "cost", d.CostUSD, 300)
	near(t, "proceeds", d.ProceedsUSD, 50)
	near(t, "gain", d.GainUSD, -250)
	if d.TxID != "writeoff:0" || d.Note != "" || d.Wallet != "w" || !d.Sold.Equal(at(2026, 6, 1, 0)) {
		t.Errorf("disposal = %+v", d)
	}
	if len(r.Parked) != 0 || len(r.Lots) != 0 || len(r.Issues) != 0 {
		t.Errorf("parked = %+v lots = %+v issues = %+v, want none", r.Parked, r.Lots, r.Issues)
	}
}

func TestWriteOffExceedsParked(t *testing.T) {
	in := writeOffInput(2.5, 50)
	r := Compute(in)
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v", r.Disposals)
	}
	near(t, "amount", r.Disposals[0].Amount, 2)
	near(t, "proceeds", r.Disposals[0].ProceedsUSD, 40) // 50 * 2 / 2.5
	is := issuesOf(r, IssueWriteOffExceedsParked)
	if len(is) != 1 {
		t.Fatalf("issues = %+v", r.Issues)
	}
	near(t, "issue amount", is[0].Amount, 0.5)
	if is[0].TxID != "writeoff:0" || is[0].Wallet != "w" || is[0].Asset != "SOL" {
		t.Errorf("issue = %+v", is[0])
	}
}

func TestPositionGainTag(t *testing.T) {
	dep := mk("dep", "w", at(2026, 3, 1, 0), core.TxSend, lg(jup, -1000, usd(-800)))
	wd := mk("wd", "w", at(2026, 4, 1, 0), core.TxReceive, lg(jup, 1050, usd(52.5)))
	r := Compute(Input{
		From: from2026, Opening: jupOpening(), Txs: []core.Tx{dep, wd},
		Tags: map[string]Tag{dep.ID: TagPosition, wd.ID: TagPositionGain},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v", r.Disposals)
	}
	d := r.Disposals[0]
	near(t, "amount", d.Amount, 50)
	near(t, "cost", d.CostUSD, 0)
	near(t, "proceeds", d.ProceedsUSD, 2.5)
	if d.Term != ShortTerm || d.Note != "" || d.TxID != wd.ID || d.Source != SourcePositionGain {
		t.Errorf("disposal = %+v", d)
	}
	l := lotsIn(r, "w", jup)
	if len(l) != 2 {
		t.Fatalf("lots = %+v", l)
	}
	near(t, "lot amount", l[1].Amount, 50)
	near(t, "lot cost", l[1].CostUSD, 2.5)
	if len(r.Income) != 0 || len(issuesOf(r, IssuePositionExcess)) != 0 {
		t.Errorf("income = %+v issues = %+v", r.Income, r.Issues)
	}
}

func TestParkedExposed(t *testing.T) {
	dep := mk("dep", "w", at(2026, 3, 1, 0), core.TxSend, lg(jup, -1000, usd(-800)))
	r := Compute(Input{From: from2026, Opening: jupOpening(), Txs: []core.Tx{dep}, Tags: map[string]Tag{dep.ID: TagPosition}})
	if len(r.Parked) != 1 || r.Parked[0] != (Parked{Wallet: "w", Asset: jup, Amount: 1000}) {
		t.Fatalf("parked = %+v", r.Parked)
	}
}

func TestSettingsWriteOffValidation(t *testing.T) {
	ok := WriteOff{Wallet: "w", Asset: "SOL", Amount: 1, Date: at(2026, 1, 1, 0), ProceedsUSD: 0}
	if _, err := (Settings{WriteOffs: []WriteOff{ok}}).Normalized(); err != nil {
		t.Fatalf("valid write-off (proceeds 0): %v", err)
	}
	bad := map[string]func(*WriteOff){
		"amount": func(w *WriteOff) { w.Amount = 0 },
		"nan":    func(w *WriteOff) { w.ProceedsUSD = math.NaN() },
		"neg":    func(w *WriteOff) { w.ProceedsUSD = -1 },
		"wallet": func(w *WriteOff) { w.Wallet = "" },
		"asset":  func(w *WriteOff) { w.Asset = "" },
		"date":   func(w *WriteOff) { w.Date = time.Time{} },
		"infamt": func(w *WriteOff) { w.Amount = math.Inf(1) },
	}
	for name, mut := range bad {
		w := ok
		mut(&w)
		_, err := (Settings{WriteOffs: []WriteOff{w}}).Normalized()
		if err == nil || !strings.Contains(err.Error(), "writeOffs[0]") {
			t.Errorf("%s: err = %v, want writeOffs[0] error", name, err)
		}
	}
}
