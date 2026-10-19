package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestPaceSpacesRequestsAcrossClients: requests to a paced host start at least the interval
// apart, whichever client sends them; other hosts aren't held up.
func TestPaceSpacesRequestsAcrossClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	SetPace(host, 40*time.Millisecond)
	t.Cleanup(func() { SetPace(host, 0) })
	a, b := New(), New()
	t0 := time.Now()
	for _, c := range []*Client{a, b, a} {
		if err := c.GetJSON(context.Background(), srv.URL, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(t0); d < 80*time.Millisecond {
		t.Errorf("three paced requests took %v, want at least two intervals (80ms)", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.GetJSON(ctx, srv.URL, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait = %v, want context.Canceled", err)
	}
}

// TestPacedHostHoldsOnRetryAfter: a paced host's 429 with Retry-After holds every request to
// it, not only the one that was refused.
func TestPacedHostHoldsOnRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		first := len(arrivals) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	SetPace(host, time.Millisecond)
	t.Cleanup(func() { SetPace(host, 0) })

	var wg sync.WaitGroup
	wg.Go(func() { New().GetJSON(context.Background(), srv.URL, nil, nil) }) // refused, then retried
	for {
		mu.Lock()
		n := len(arrivals)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the 429 is back by now
	wg.Go(func() { New().GetJSON(context.Background(), srv.URL, nil, nil) })
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for _, at := range arrivals[1:] {
		if d := at.Sub(arrivals[0]); d < 900*time.Millisecond {
			t.Errorf("a request reached the server %v after the 429, inside its 1 s Retry-After", d)
		}
	}
}
