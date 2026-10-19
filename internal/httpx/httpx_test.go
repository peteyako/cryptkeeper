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

// TestHoldStopsRequestsAlreadyWaiting: requests already waiting their turn when a 429 comes
// back wait out its Retry-After too, instead of going on the turn they were waiting for.
func TestHoldStopsRequestsAlreadyWaiting(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		first := len(arrivals) == 1
		mu.Unlock()
		if first {
			time.Sleep(10 * time.Millisecond) // the other two are waiting by now
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	SetPace(host, 40*time.Millisecond) // turns at 0, 40ms and 80ms
	t.Cleanup(func() { SetPace(host, 0) })

	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			c := New()
			c.PaceOwn429 = true
			c.GetJSON(context.Background(), srv.URL, nil, nil)
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatalf("%d requests reached the server, want 3", len(arrivals))
	}
	for _, at := range arrivals[1:] {
		if d := at.Sub(arrivals[0]); d < 900*time.Millisecond {
			t.Errorf("a waiting request reached the server %v after the 429, inside its 1 s Retry-After", d)
		}
	}
}

// TestCancelledWaitGivesUpItsTurn: a waiting request that is cancelled leaves the line at
// once, and the request behind it goes on the turn it would have had.
func TestCancelledWaitGivesUpItsTurn(t *testing.T) {
	const host, interval = "cancel.test", 60 * time.Millisecond
	SetPace(host, interval)
	t.Cleanup(func() { SetPace(host, 0) })
	t0 := time.Now()
	if err := Wait(context.Background(), host); err != nil { // goes at once
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { cancelled <- Wait(ctx, host) }() // next in line, for the turn at 60ms
	time.Sleep(10 * time.Millisecond)
	next := make(chan time.Duration, 1)
	go func() { // behind it, for the turn at 120ms
		if err := Wait(context.Background(), host); err != nil {
			t.Error(err)
		}
		next <- time.Since(t0)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled wait = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Millisecond):
		t.Error("a cancelled wait didn't return at once")
	}
	if d := <-next; d >= 2*interval-10*time.Millisecond {
		t.Errorf("the request behind a cancelled one went after %v, want its turn at %v", d, interval)
	}
}
