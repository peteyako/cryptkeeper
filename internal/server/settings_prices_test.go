package server

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSettingsPriceInterval: Settings offers how often prices refresh, up to with every
// balance refresh. Saving changes the running refresher at once, a value past the balance
// interval is held to it, and the saved choice applies again after a restart.
func TestSettingsPriceInterval(t *testing.T) {
	e := newEnv(t, "")
	_, page := e.do("GET", "/settings", nil, nil)
	for _, want := range []string{`id="f-priceMinutes"`, `<option value="1" selected>Every minute</option>`, "With the balances (every 5 min)"} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings lacks %q", want)
		}
	}

	save := func(minutes string) string {
		body, hdr := form(url.Values{"priceMinutes": {minutes}})
		_, out := e.do("POST", "/settings/price-interval", body, hdr)
		return out
	}
	if out := save("2"); !strings.Contains(out, `id="settings-refresh"`) {
		t.Fatalf("saving answered: %.400s", out)
	}
	if got := e.ref.PriceEvery(); got != 2*time.Minute {
		t.Errorf("PriceEvery = %v after saving 2 min", got)
	}
	if _, page = e.do("GET", "/settings", nil, nil); !strings.Contains(page, `<option value="2" selected>`) {
		t.Error("Settings doesn't show the saved 2 min")
	}

	save("99")
	if got := e.ref.PriceEvery(); got != 5*time.Minute {
		t.Errorf("PriceEvery = %v after saving 99 min, want the balance interval (5 min)", got)
	}

	e.ref.SetPriceInterval(time.Minute)
	New(e.deps)
	if got := e.ref.PriceEvery(); got != 5*time.Minute {
		t.Errorf("after a restart PriceEvery = %v, want the saved 5 min", got)
	}
}

// TestResetRestoresPriceInterval: a reset erases the saved price interval, so prices go back
// to refreshing as config.yaml says.
func TestResetRestoresPriceInterval(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"priceMinutes": {"3"}})
	e.do("POST", "/settings/price-interval", body, hdr)
	body, hdr = form(url.Values{"confirm": {"RESET"}})
	e.do("POST", "/backup/reset", body, hdr)
	if got := e.ref.PriceEvery(); got != time.Minute {
		t.Errorf("after a reset PriceEvery = %v, want config.yaml's 1 min", got)
	}
}
