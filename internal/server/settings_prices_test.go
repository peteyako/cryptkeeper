package server

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSettingsIntervals: Settings picks how often balances refresh, how often prices
// refresh in between (up to with every balance refresh) and how often activity syncs. Saving changes the running refresher
// at once; prices "with the balances" follow the balance interval; the saved choices apply
// again after a restart.
func TestSettingsIntervals(t *testing.T) {
	e := newEnv(t, "")
	_, page := e.do("GET", "/settings", nil, nil)
	for _, want := range []string{`id="f-refreshMinutes"`, `<option value="5" selected>Every 5 min</option>`,
		`id="f-priceMinutes"`, `<option value="1" selected>Every minute</option>`, "With the balances (every 5 min)",
		`id="f-activityMinutes"`, `<option value="15" selected>Every 15 min</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings lacks %q", want)
		}
	}

	save := func(v url.Values) string {
		body, hdr := form(v)
		_, out := e.do("POST", "/settings/intervals", body, hdr)
		return out
	}
	check := func(what string, balances, prices time.Duration) {
		t.Helper()
		if b, p := e.ref.RefreshEvery(), e.ref.PriceEvery(); b != balances || p != prices {
			t.Errorf("%s: balances every %v, prices every %v; want %v, %v", what, b, p, balances, prices)
		}
	}
	if out := save(url.Values{"refreshMinutes": {"10"}, "priceMinutes": {"2"}}); !strings.Contains(out, `id="settings-refresh"`) {
		t.Fatalf("saving answered: %.400s", out)
	}
	check("10 and 2", 10*time.Minute, 2*time.Minute)
	if _, page = e.do("GET", "/settings", nil, nil); !strings.Contains(page, `<option value="10" selected>`) || !strings.Contains(page, `<option value="2" selected>`) {
		t.Error("Settings doesn't show the saved 10 and 2 min")
	}

	save(url.Values{"refreshMinutes": {"10"}, "priceMinutes": {"0"}})
	check("prices with the balances", 10*time.Minute, 10*time.Minute)
	save(url.Values{"refreshMinutes": {"30"}, "priceMinutes": {"0"}})
	check("prices still with the balances", 30*time.Minute, 30*time.Minute)

	save(url.Values{"refreshMinutes": {"2"}, "priceMinutes": {"5"}})
	check("prices held to the balance interval", 2*time.Minute, 2*time.Minute)

	save(url.Values{"refreshMinutes": {"2"}, "priceMinutes": {"5"}, "activityMinutes": {"60"}})
	if got := e.syncer.Every(); got != time.Hour {
		t.Errorf("activity syncs every %v after saving 60 min", got)
	}
	if _, page = e.do("GET", "/settings", nil, nil); !strings.Contains(page, `<option value="60" selected>Every hour</option>`) {
		t.Error("Settings doesn't show activity syncing every hour")
	}
	e.syncer.SetInterval(time.Minute)

	e.ref.SetIntervals(time.Hour, time.Minute)
	New(e.deps)
	check("after a restart", 2*time.Minute, 2*time.Minute)
	if got := e.syncer.Every(); got != time.Hour {
		t.Errorf("after a restart activity syncs every %v, want the saved hour", got)
	}
}

// TestResetRestoresIntervals: a reset erases the saved intervals, so balances and prices go
// back to refreshing as config.yaml says.
func TestResetRestoresIntervals(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"refreshMinutes": {"15"}, "priceMinutes": {"3"}, "activityMinutes": {"60"}})
	e.do("POST", "/settings/intervals", body, hdr)
	body, hdr = form(url.Values{"confirm": {"RESET"}})
	e.do("POST", "/backup/reset", body, hdr)
	if b, p := e.ref.RefreshEvery(), e.ref.PriceEvery(); b != 5*time.Minute || p != time.Minute {
		t.Errorf("after a reset: balances every %v, prices every %v; want config.yaml's 5 min and 1 min", b, p)
	}
	if got := e.syncer.Every(); got != 15*time.Minute {
		t.Errorf("after a reset activity syncs every %v, want config.yaml's 15 min", got)
	}
}
