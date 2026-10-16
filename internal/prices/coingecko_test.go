package prices_test

import (
	"context"
	"testing"
	"time"
)

const cgMint = "MintTok3n1111111111111111111111111111111111"

var cgDay = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

func TestHistoricalMintFetchesAndStores(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42, "2026-05-02": 0.5}
	usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay)
	if err != nil || !ok || !near(usd, 0.42) {
		t.Fatalf("got %v %v %v", usd, ok, err)
	}
	n := e.fk.count("coingecko")
	if n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	// Whole window was stored: the neighbouring day and a restarted service need no request.
	for _, s := range []int{0, 1} {
		svc := e.svc
		if s == 1 {
			svc = e.newService()
		}
		usd, ok, err = svc.HistoricalMint(context.Background(), cgMint, cgDay.AddDate(0, 0, 1))
		if err != nil || !ok || !near(usd, 0.5) {
			t.Fatalf("day 2: %v %v %v", usd, ok, err)
		}
	}
	if got := e.fk.count("coingecko"); got != n {
		t.Fatalf("calls = %d, want %d (DB hit)", got, n)
	}
}

func TestHistoricalMintKeyHeader(t *testing.T) {
	e := newEnv(t)
	e.fk.cgKey = "k1"
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42}
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got := e.fk.cgHeaders[0].Get("x-cg-demo-api-key"); got != "k1" {
		t.Fatalf("header = %q, want k1", got)
	}
}

func TestHistoricalMintNoKeyHeader(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42}
	usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay)
	if err != nil || !ok || !near(usd, 0.42) {
		t.Fatalf("got %v %v %v", usd, ok, err)
	}
	if _, present := e.fk.cgHeaders[0]["X-Cg-Demo-Api-Key"]; present {
		t.Fatal("key header sent without a key")
	}
}

func TestHistoricalMintUnknownCached(t *testing.T) {
	e := newEnv(t)
	e.fk.cgStatus[cgMint] = 404
	for i := 0; i < 2; i++ {
		if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || ok {
			t.Fatalf("call %d: ok=%v err=%v", i, ok, err)
		}
	}
	if n := e.fk.count("coingecko"); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	e.clock.Set(t0.Add(7 * time.Hour)) // negative cache expires
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || ok {
		t.Fatalf("after expiry: ok=%v err=%v", ok, err)
	}
	if n := e.fk.count("coingecko"); n != 2 {
		t.Fatalf("calls after expiry = %d, want 2", n)
	}
}

func TestHistoricalMintEmptyIsUnknown(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{}
	for i := 0; i < 2; i++ {
		if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	}
	if n := e.fk.count("coingecko"); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

func TestHistoricalMintServerErrorNotCached(t *testing.T) {
	e := newEnv(t)
	e.fk.cgStatus[cgMint] = 429
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err == nil || ok {
		t.Fatalf("want error, got ok=%v err=%v", ok, err)
	}
	delete(e.fk.cgStatus, cgMint)
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42}
	if usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || !ok || !near(usd, 0.42) {
		t.Fatalf("retry: %v %v %v", usd, ok, err)
	}
}

func TestHistoricalMintTooOldOrFuture(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2025-09-01": 1}
	for _, d := range []time.Time{t0.AddDate(0, 0, -400), t0.AddDate(0, 0, 3)} {
		if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, d); err != nil || ok {
			t.Fatalf("%v: ok=%v err=%v", d, ok, err)
		}
	}
	if n := e.fk.count("coingecko"); n != 0 {
		t.Fatalf("calls = %d, want 0", n)
	}
}

func TestHistoricalMintStoredOldDayStillServed(t *testing.T) {
	e := newEnv(t)
	old := t0.AddDate(0, 0, -400)
	if _, err := e.db.Exec(`INSERT INTO price_history (asset, day, usd) VALUES (?, ?, ?)`,
		"spl:"+cgMint, old.Format("2006-01-02"), 0.77); err != nil {
		t.Fatal(err)
	}
	usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, old)
	if err != nil || !ok || !near(usd, 0.77) {
		t.Fatalf("got %v %v %v", usd, ok, err)
	}
	if n := e.fk.count("coingecko"); n != 0 {
		t.Fatalf("calls = %d, want 0", n)
	}
}

func TestHistoricalMintTodayNewestPointNotStored(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2026-10-15": 0.9, "2026-10-16": 0.95}
	usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, t0)
	if err != nil || !ok || !near(usd, 0.95) {
		t.Fatalf("got %v %v %v", usd, ok, err)
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM price_history WHERE asset = ?`, "spl:"+cgMint).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored rows = %d err=%v, want 0", n, err)
	}
}

func TestHistoricalMintTodayEmptyNotMarkedUnknown(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42} // nothing in the short range
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, t0); err != nil || ok {
		t.Fatalf("today: ok=%v err=%v", ok, err)
	}
	usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay)
	if err != nil || !ok || !near(usd, 0.42) {
		t.Fatalf("historical after empty today: %v %v %v", usd, ok, err)
	}
}
