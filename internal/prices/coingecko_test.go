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

func TestHistoricalMintUnknownRememberedForAWeek(t *testing.T) {
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
	// "Coin not found" is kept in the database: a restarted service does not ask again, today's
	// price included, until the week is over.
	e.clock.Set(t0.Add(6 * 24 * time.Hour))
	svc := e.newService()
	for _, d := range []time.Time{cgDay, e.clock.Now()} {
		if _, ok, err := svc.HistoricalMint(context.Background(), cgMint, d); err != nil || ok {
			t.Fatalf("after restart, %v: ok=%v err=%v", d, ok, err)
		}
	}
	if n := e.fk.count("coingecko"); n != 1 {
		t.Fatalf("calls after restart = %d, want 1", n)
	}
	e.clock.Set(t0.Add(7*24*time.Hour + time.Hour))
	if _, ok, err := svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || ok {
		t.Fatalf("after expiry: ok=%v err=%v", ok, err)
	}
	if n := e.fk.count("coingecko"); n != 2 {
		t.Fatalf("calls after expiry = %d, want 2", n)
	}
}

func TestHistoricalMintRateLimitPausesCoinGecko(t *testing.T) {
	e := newEnv(t)
	other := "OtherMint111111111111111111111111111111111"
	e.fk.cgStatus[cgMint] = 429
	e.fk.cg[other] = map[string]float64{"2026-05-01": 0.3}
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err == nil || ok {
		t.Fatalf("want error, got ok=%v err=%v", ok, err)
	}
	// Paused: every mint fails fast, with no request, for five minutes.
	e.clock.Advance(4 * time.Minute)
	for _, m := range []string{cgMint, other} {
		if _, ok, err := e.svc.HistoricalMint(context.Background(), m, cgDay); err == nil || ok {
			t.Fatalf("%s during the pause: ok=%v err=%v", m, ok, err)
		}
	}
	if n := e.fk.count("coingecko"); n != 1 {
		t.Fatalf("calls during the pause = %d, want 1", n)
	}
	e.clock.Advance(time.Minute + time.Second)
	if usd, ok, err := e.svc.HistoricalMint(context.Background(), other, cgDay); err != nil || !ok || !near(usd, 0.3) {
		t.Fatalf("after the pause: %v %v %v", usd, ok, err)
	}
}

func TestHistoricalMintRetryAfterLengthensPause(t *testing.T) {
	e := newEnv(t)
	e.fk.cgStatus[cgMint] = 429
	e.fk.cgRetryAfter = "600"
	e.svc.HistoricalMint(context.Background(), cgMint, cgDay)
	delete(e.fk.cgStatus, cgMint)
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42}
	e.clock.Advance(9 * time.Minute)
	if _, _, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err == nil {
		t.Fatal("still paused at 9 minutes: want an error")
	}
	e.clock.Advance(time.Minute + time.Second)
	if usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || !ok || !near(usd, 0.42) {
		t.Fatalf("after Retry-After: %v %v %v", usd, ok, err)
	}
	if n := e.fk.count("coingecko"); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestHistoricalMintStoreFailureNotCovered(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42, "2026-05-02": 0.5}
	if _, err := e.db.Exec(`DROP TABLE price_history`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	// Nothing was stored, so the window is not "fetched": the neighbouring day asks again.
	if usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay.AddDate(0, 0, 1)); err != nil || !ok || !near(usd, 0.5) {
		t.Fatalf("next day: %v %v %v", usd, ok, err)
	}
	if n := e.fk.count("coingecko"); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestHistoricalMintCloseIsNextMidnight(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{
		"2026-10-13": 0.1, "2026-10-14": 0.2, "2026-10-15": 0.3, "2026-10-16": 0.4, "2026-10-16T09:00": 0.45,
	}
	for _, c := range []struct {
		day  time.Time
		want float64
	}{
		{time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), 0.2}, // the point at Oct 14 00:00
		{time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), 0.3},
		{time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), 0.4},  // yesterday: today's 00:00, not the newest
		{time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), 0.45}, // today: the newest price so far
	} {
		usd, ok, err := e.svc.HistoricalMintClose(context.Background(), cgMint, c.day)
		if err != nil || !ok || !near(usd, c.want) {
			t.Errorf("%s: %v %v %v, want %v", c.day.Format("2006-01-02"), usd, ok, err, c.want)
		}
	}
	if _, ok, err := e.svc.HistoricalMintClose(context.Background(), cgMint, t0.AddDate(0, 0, 1)); err != nil || ok {
		t.Errorf("tomorrow: ok=%v err=%v", ok, err)
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
	e.fk.cgStatus[cgMint] = 503
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

// Today's answer is reused for a few minutes: five same-day lookups make one request, and a
// lookup after the TTL asks again.
func TestHistoricalMintTodayCachedBriefly(t *testing.T) {
	e := newEnv(t)
	e.fk.cg[cgMint] = map[string]float64{"2026-10-15": 0.9, "2026-10-16": 0.95}
	for range 5 {
		if usd, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, t0); err != nil || !ok || !near(usd, 0.95) {
			t.Fatal(usd, ok, err)
		}
	}
	if n := e.fk.count("coingecko"); n != 1 {
		t.Fatalf("coingecko requests = %d, want 1", n)
	}
	e.clock.Advance(6 * time.Minute)
	if _, _, err := e.svc.HistoricalMint(context.Background(), cgMint, t0.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := e.fk.count("coingecko"); n != 2 {
		t.Fatalf("coingecko requests after the TTL = %d, want 2", n)
	}
}

// A failed load of the remembered "coin not found" answers is retried by the next lookup, not
// given up for the life of the process.
func TestCGNotFoundLoadRetriesAfterFailure(t *testing.T) {
	e := newEnv(t)
	const other = "OtherMint1111111111111111111111111111111111"
	if _, err := e.db.Exec(`ALTER TABLE price_unknown RENAME TO price_unknown_away`); err != nil {
		t.Fatal(err)
	}
	e.fk.cg[cgMint] = map[string]float64{"2026-05-01": 0.42}
	if _, ok, err := e.svc.HistoricalMint(context.Background(), cgMint, cgDay); err != nil || !ok {
		t.Fatalf("first lookup (table unreadable): ok=%v err=%v", ok, err)
	}
	if _, err := e.db.Exec(`ALTER TABLE price_unknown_away RENAME TO price_unknown`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO price_unknown (key, until) VALUES (?, ?)`, "cg:"+other, t0.Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	before := e.fk.count("coingecko")
	if _, ok, err := e.svc.HistoricalMint(context.Background(), other, cgDay); err != nil || ok {
		t.Fatalf("remembered miss: ok=%v err=%v", ok, err)
	}
	if n := e.fk.count("coingecko"); n != before {
		t.Errorf("the remembered miss was not loaded: %d requests", n-before)
	}
}
