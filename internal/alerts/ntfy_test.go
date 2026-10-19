package alerts

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/httpx"
)

type captured struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

func ntfyServer(t *testing.T, status int, resp string) (*httptest.Server, func() []captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{r.Method, r.URL.Path, r.Header.Clone(), string(body)})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		return append([]captured(nil), got...)
	}
}

func noRetryClient() *httpx.Client {
	return &httpx.Client{HTTP: &http.Client{Timeout: 5 * time.Second}, MaxRetries: 0, BaseDelay: time.Millisecond}
}

func TestNtfySendHeadersAndBody(t *testing.T) {
	srv, reqs := ntfyServer(t, 200, `{"id":"x"}`)
	n := &Ntfy{URL: srv.URL + "/cryptkeeper-topic", Token: "tk_secret", Client: noRetryClient()}
	err := n.Send(context.Background(), Notification{
		RuleID: "cold-drop", Title: "Balance drop", Body: "Cold storage: BTC balance dropped 12%",
		Priority: 5, Tags: []string{"rotating_light", "warning"}, Click: "http://cryptkeeper.tailnet:8787/w/cold",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := reqs()
	if len(got) != 1 {
		t.Fatalf("%d requests", len(got))
	}
	r := got[0]
	if r.Method != http.MethodPost || r.Path != "/cryptkeeper-topic" {
		t.Fatalf("%s %s", r.Method, r.Path)
	}
	if r.Body != "Cold storage: BTC balance dropped 12%" {
		t.Fatalf("body %q", r.Body)
	}
	for k, want := range map[string]string{
		"Title":         "Balance drop",
		"Priority":      "5",
		"Tags":          "rotating_light,warning",
		"Click":         "http://cryptkeeper.tailnet:8787/w/cold",
		"Authorization": "Bearer tk_secret",
	} {
		if v := r.Header.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type %q", ct)
	}
}

func TestNtfyOmitsOptionalHeaders(t *testing.T) {
	srv, reqs := ntfyServer(t, 200, "")
	n := &Ntfy{URL: srv.URL + "/t", Client: noRetryClient()}
	if err := n.Send(context.Background(), Notification{Title: "T", Body: "B", Priority: 0}); err != nil {
		t.Fatal(err)
	}
	h := reqs()[0].Header
	for _, k := range []string{"Authorization", "Click", "Tags"} {
		if _, ok := h[k]; ok {
			t.Errorf("header %s must be absent, got %q", k, h.Get(k))
		}
	}
	if p := h.Get("Priority"); p != "3" {
		t.Errorf("default priority %q, want 3", p)
	}
}

func TestNtfyClampsPriority(t *testing.T) {
	srv, reqs := ntfyServer(t, 200, "")
	n := &Ntfy{URL: srv.URL + "/t", Client: noRetryClient()}
	for _, p := range []int{-4, 9} {
		if err := n.Send(context.Background(), Notification{Title: "T", Body: "B", Priority: p}); err != nil {
			t.Fatal(err)
		}
	}
	got := reqs()
	if got[0].Header.Get("Priority") != "1" || got[1].Header.Get("Priority") != "5" {
		t.Fatalf("priorities %q %q", got[0].Header.Get("Priority"), got[1].Header.Get("Priority"))
	}
}

func TestNtfyEncodesNonASCIITitle(t *testing.T) {
	srv, reqs := ntfyServer(t, 200, "")
	n := &Ntfy{URL: srv.URL + "/t", Client: noRetryClient()}
	title := "Café wallet\r\nX-Injected: 1"
	if err := n.Send(context.Background(), Notification{Title: title, Body: "B"}); err != nil {
		t.Fatal(err)
	}
	r := reqs()[0]
	raw := r.Header.Get("Title")
	if strings.ContainsAny(raw, "\r\n") || r.Header.Get("X-Injected") != "" {
		t.Fatalf("header injection: %q", raw)
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil || !strings.HasPrefix(dec, "Café wallet") {
		t.Fatalf("title %q decodes to %q (%v)", raw, dec, err)
	}
	for _, c := range raw {
		if c > 127 {
			t.Fatalf("raw non-ASCII in header %q", raw)
		}
	}
}

func TestNtfyNon2xxIsError(t *testing.T) {
	srv, _ := ntfyServer(t, 403, `{"code":40301,"error":"forbidden"}`)
	n := &Ntfy{URL: srv.URL + "/secret-topic", Token: "tk_secret", Client: noRetryClient()}
	err := n.Send(context.Background(), Notification{Title: "T", Body: "B"})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should carry status and server message: %v", err)
	}
	if strings.Contains(err.Error(), "secret-topic") || strings.Contains(err.Error(), "tk_secret") {
		t.Fatalf("error leaks the topic URL or token: %v", err)
	}
}

func TestNtfyTransportErrorDoesNotLeakURL(t *testing.T) {
	srv, _ := ntfyServer(t, 200, "")
	url := srv.URL + "/secret-topic"
	srv.Close()
	n := &Ntfy{URL: url, Client: noRetryClient()}
	err := n.Send(context.Background(), Notification{Title: "T", Body: "B"})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-topic") {
		t.Fatalf("error leaks the topic URL: %v", err)
	}
}

func TestNtfyRetriesServerErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n := &Ntfy{URL: srv.URL + "/t", Client: &httpx.Client{HTTP: srv.Client(), MaxRetries: 2, BaseDelay: time.Millisecond}}
	if err := n.Send(context.Background(), Notification{Title: "T", Body: "B"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("%d calls, want 2", calls)
	}
}

func TestNtfyConfigErrors(t *testing.T) {
	if err := (&Ntfy{}).Send(context.Background(), Notification{Body: "B"}); err == nil {
		t.Fatal("empty URL must be an error")
	}
	if err := (&Ntfy{URL: "ftp://x/y"}).Send(context.Background(), Notification{Body: "B"}); err == nil {
		t.Fatal("non-http URL must be an error")
	}
}

func TestNtfyDefaultClient(t *testing.T) {
	srv, reqs := ntfyServer(t, 200, "")
	n := &Ntfy{URL: srv.URL + "/t"} // nil Client falls back to httpx.New()
	if err := n.Send(context.Background(), Notification{Title: "T", Body: "B"}); err != nil {
		t.Fatal(err)
	}
	if len(reqs()) != 1 {
		t.Fatal("no request")
	}
}

func TestNtfyContextCancel(t *testing.T) {
	srv, _ := ntfyServer(t, 200, "")
	n := &Ntfy{URL: srv.URL + "/t", Client: noRetryClient()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := n.Send(ctx, Notification{Title: "T", Body: "B"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

var _ Notifier = (*Ntfy)(nil)
