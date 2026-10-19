package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestErrorsNeverCarryQueryStrings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	err := New().GetJSON(context.Background(), srv.URL+"/rpc?api-key=SECRET", nil, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "/rpc") {
		t.Fatalf("error must name the endpoint without its query: %v", err)
	}
}

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New()
	c.BaseDelay = 1
	var out struct{ OK bool }
	if err := c.GetJSON(context.Background(), srv.URL, nil, &out); err != nil || !out.OK || n != 3 {
		t.Fatalf("%v %v %d", err, out, n)
	}
}

// TestPaceOwn429ReturnsAtOnce: a caller that paces its own retries gets a 429 back after one
// request, with the server's Retry-After; other failures still retry.
func TestPaceOwn429ReturnsAtOnce(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New()
	c.BaseDelay, c.PaceOwn429 = 1, true
	err := c.GetJSON(context.Background(), srv.URL, nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests || se.RetryAfter != 7*time.Second || n != 1 {
		t.Fatalf("err %v after %d requests, want one 429 with Retry-After 7s", err, n)
	}
}
