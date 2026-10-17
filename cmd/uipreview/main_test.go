package main

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"crypto-portfolio/internal/demo"
)

func do(t *testing.T, h http.Handler, method, target string, body io.Reader, ctype string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func TestEveryPageRenders(t *testing.T) {
	h := router()
	for _, path := range []string{
		"/", "/wallets", "/settings", "/portfolios",
		"/activity", "/activity?source=coinbase&asset=BTC&kind=trade", "/activity?empty=1",
		"/reports", "/reports?empty=1", "/reports/koinly", "/reports/koinly?empty=1", "/reports/koinly?blocked=1",
		"/reports/koinly?step=1", "/reports/koinly?step=2", "/reports/koinly?step=4",
		"/reports/tax", "/reports/tax?year=2025", "/reports/tax?year=2026&tab=income", "/reports/tax?year=2026&tab=holdings",
		"/reports/tax?year=2026&tab=review", "/reports/tax?empty=1", "/reports/tax/2026/8949", "/reports/tax/2025/8949",
		"/reports/tax/2026/8949?empty=1", "/reports/tax/2026/report", "/reports/tax/2025/report", "/reports/tax/2026/report?empty=1",
		"/alerts", "/alerts?empty=1", "/alerts?error=1", "/alerts/new", "/alerts/new?invalid=1",
		"/alerts/" + demo.AlertColdDrop + "/edit", "/alerts/" + demo.AlertBTCAbove + "/edit", "/alerts/" + demo.AlertDayMove + "/edit",
		"/alerts/" + demo.AlertKrakenOff + "/edit", "/alerts/" + demo.AlertBigSend + "/edit",
		"/a/SOL", "/a/BTC", "/w/coinbase?tab=activity", "/w/cold?tab=activity", "/w/mom-safe?tab=activity",
		"/export/holdings.csv", "/export/history.csv?range=All", "/export/pnl.csv", "/export/activity.csv",
	} {
		code, body := do(t, h, "GET", path, nil, "")
		if code != 200 || len(body) < 50 {
			t.Errorf("%s: %d, %d bytes", path, code, len(body))
		}
	}
	if code, _ := do(t, h, "GET", "/alerts/nope/edit", nil, ""); code != http.StatusSeeOther {
		t.Errorf("an unknown alert redirects, got %d", code)
	}
}

func TestActivityList(t *testing.T) {
	h := router()
	signals := func(js string) string { return "/activity/list?datastar=" + url.QueryEscape(js) }
	// A filter change replaces the whole list.
	_, body := do(t, h, "GET", signals(`{"actKind":"reward"}`), nil, "")
	if !strings.Contains(body, `id="activity-list"`) || !strings.Contains(body, "Staking reward") || strings.Contains(body, "Swapped SOL") {
		t.Errorf("filtered list:\n%.600s", body)
	}
	// "before" appends rows and replaces the control.
	first := demo.Activity("", "", "", "", activityNow)
	_, body = do(t, h, "GET", signals(`{}`)+"&before="+url.QueryEscape(first.More), nil, "")
	if !strings.Contains(body, "selector #activity-rows") || !strings.Contains(body, "mode append") || !strings.Contains(body, `id="activity-more"`) {
		t.Errorf("older page:\n%.600s", body)
	}
	if strings.Contains(body, `id="activity-list"`) {
		t.Error("appending must not replace the list")
	}
}

func TestReportsStubs(t *testing.T) {
	h := router()
	// The manual cost form is gone: the tax lots are the only source of cost basis.
	_, page := do(t, h, "GET", "/reports", nil, "")
	if strings.Contains(page, `id="basis-panel"`) || strings.Contains(page, "/reports/basis") || !strings.Contains(page, `id="pnl"`) {
		t.Error("Reports must show the P&L without the manual cost panel")
	}
	if code, _ := do(t, h, "POST", "/reports/basis", strings.NewReader("asset=BTC"), "application/x-www-form-urlencoded"); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("POST /reports/basis: %d", code)
	}

	// CSV import: a file succeeds, a name with "bad" fails, no file is asked for.
	upload := func(name string) string {
		var buf strings.Builder
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("source", "kraken")
		if name != "" {
			fw, _ := mw.CreateFormFile("file", name)
			_, _ = fw.Write([]byte("a,b\n1,2\n"))
		}
		_ = mw.Close()
		_, out := do(t, h, "POST", "/reports/import", strings.NewReader(buf.String()), mw.FormDataContentType())
		return out
	}
	if out := upload("ledgers.csv"); !strings.Contains(out, "notice--gain") || !strings.Contains(out, "Imported 148 transactions") {
		t.Errorf("import ok:\n%.600s", out)
	}
	if out := upload("bad.csv"); !strings.Contains(out, "notice--loss") || !strings.Contains(out, "Nothing was imported") {
		t.Errorf("import bad:\n%.600s", out)
	}
	if out := upload(""); !strings.Contains(out, "Choose a CSV file to import.") {
		t.Errorf("import without a file:\n%.600s", out)
	}
}

func TestAlertsStubs(t *testing.T) {
	h := router()
	form := func(kv ...string) io.Reader {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Add(kv[i], kv[i+1])
		}
		return strings.NewReader(v.Encode())
	}
	const ct = "application/x-www-form-urlencoded"
	_, _ = do(t, h, "GET", "/alerts?reset=1", nil, "")

	_, body := do(t, h, "POST", "/alerts/test", nil, "")
	if !strings.Contains(body, `id="alert-test"`) || !strings.Contains(body, "Test notification sent.") {
		t.Errorf("test patch:\n%.600s", body)
	}
	// A price alert with nothing filled in patches the errors.
	_, body = do(t, h, "POST", "/alerts", form("type", "price"), ct)
	if !strings.Contains(body, `id="form-errors"`) || !strings.Contains(body, "Choose an asset.") || strings.Contains(body, "/alerts\n") {
		t.Errorf("error patch:\n%.600s", body)
	}
	// A valid one redirects, and the rule shows in the list.
	_, body = do(t, h, "POST", "/alerts", form("type", "price", "asset", "ETH", "above", "4000", "name", "ETH 4k", "enabled", "on"), ct)
	if !strings.Contains(body, "/alerts") || strings.Contains(body, `id="form-errors"`) {
		t.Errorf("save should redirect:\n%.600s", body)
	}
	_, page := do(t, h, "GET", "/alerts", nil, "")
	if !strings.Contains(page, "ETH 4k") || !strings.Contains(page, "ETH above $4000") {
		t.Error("the new rule is listed")
	}
	// Deleting patches the list.
	_, body = do(t, h, "POST", "/alerts/"+demo.AlertColdDrop+"/delete", nil, "")
	if !strings.Contains(body, `id="alert-rules"`) || strings.Contains(body, "Cold storage watch") {
		t.Errorf("delete patch:\n%.600s", body)
	}
	_, _ = do(t, h, "GET", "/alerts?reset=1", nil, "")
}
