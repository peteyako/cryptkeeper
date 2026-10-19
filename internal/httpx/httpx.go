// Package httpx is the shared HTTP client: timeouts, JSON helpers and retry with backoff
// on 429 and 5xx responses.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const userAgent = "cryptkeep/1.0"

// Client wraps http.Client with retry behaviour.
type Client struct {
	HTTP       *http.Client
	MaxRetries int
	BaseDelay  time.Duration
	// PaceOwn429 returns a 429 at once instead of retrying it, for callers that pace their
	// own requests to a rate-limited server. Other failures still retry.
	PaceOwn429 bool
}

// New returns a client with a 20 s per-request timeout and 3 retries.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}, MaxRetries: 3, BaseDelay: 500 * time.Millisecond}
}

// StatusError is returned for non-2xx responses after retries are exhausted.
type StatusError struct {
	Status int
	Body   string
	URL    string
	// RetryAfter is the Retry-After header of a 429 or 5xx answer, in full (0 when absent).
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.URL, e.Status, b)
}

// Do sends the request built by newReq (called again for each retry, so bodies can be re-read)
// and returns the response body. Retries on 429, 5xx and transport errors.
func (c *Client) Do(ctx context.Context, newReq func(context.Context) (*http.Request, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := c.BaseDelay << (attempt - 1)
			if se, ok := lastErr.(*retryAfterError); ok && se.after > 0 {
				delay = se.after
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := newReq(ctx)
		if err != nil {
			return nil, err
		}
		if err := Wait(ctx, req.URL.Host); err != nil {
			return nil, err
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", userAgent)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests && c.PaceOwn429 {
			hold(req.URL.Host, retryAfterHeader(resp))
			return nil, &StatusError{Status: resp.StatusCode, Body: string(body), URL: safeURL(req), RetryAfter: retryAfterHeader(resp)}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			hold(req.URL.Host, retryAfterHeader(resp))
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			se := StatusError{Status: resp.StatusCode, Body: string(body), URL: safeURL(req), RetryAfter: retryAfterHeader(resp)}
			lastErr = &retryAfterError{se, retryAfter(resp)}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return body, &StatusError{Status: resp.StatusCode, Body: string(body), URL: safeURL(req)}
		}
		return body, nil
	}
	if ra, ok := lastErr.(*retryAfterError); ok {
		return nil, &ra.StatusError
	}
	return nil, lastErr
}

// safeURL is the request URL for error messages without the query string or user info:
// providers put API keys in query parameters (?api-key=…), and error text reaches the UI.
func safeURL(req *http.Request) string {
	return req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
}

type retryAfterError struct {
	StatusError
	after time.Duration
}

// retryAfter is the delay before retrying: the Retry-After header when it is at most a minute.
func retryAfter(resp *http.Response) time.Duration {
	if d := retryAfterHeader(resp); d <= time.Minute {
		return d
	}
	return 0
}

// retryAfterHeader is the Retry-After header in seconds, 0 when absent or not a number.
func retryAfterHeader(resp *http.Response) time.Duration {
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// GetJSON GETs url and decodes the JSON response into out.
func (c *Client) GetJSON(ctx context.Context, url string, headers map[string]string, out any) error {
	body, err := c.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	return decode(body, out, url)
}

// PostJSON POSTs payload as JSON and decodes the JSON response into out.
func (c *Client) PostJSON(ctx context.Context, url string, headers map[string]string, payload, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.PostRaw(ctx, url, "application/json", headers, raw, out)
}

// PostRaw POSTs a raw body with the given content type and decodes the JSON response into out.
func (c *Client) PostRaw(ctx context.Context, url, contentType string, headers map[string]string, raw []byte, out any) error {
	body, err := c.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	return decode(body, out, url)
}

func decode(body []byte, out any, url string) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// Some public APIs block an IP that keeps going over their rate limit, so requests to their
// hosts are spaced out process-wide, whichever Client sends them.
var pacers sync.Map // host → *pacer

type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

// SetPace spaces the requests to host (as in a URL, with any port) at least interval apart,
// across every Client in the process; the longest interval set for a host wins, and 0 removes
// the pacing. Callers may pace finer-grained keys of their own (a host and an RPC method) and
// wait on them with Wait.
func SetPace(host string, interval time.Duration) {
	if interval <= 0 {
		pacers.Delete(host)
		return
	}
	v, _ := pacers.LoadOrStore(host, &pacer{interval: interval})
	p := v.(*pacer)
	p.mu.Lock()
	p.interval = max(p.interval, interval)
	p.mu.Unlock()
}

// hold keeps every request to a paced host waiting for d after it answered 429, so the
// others don't run into the same refusal one by one.
func hold(host string, d time.Duration) {
	v, ok := pacers.Load(host)
	if !ok || d <= 0 {
		return
	}
	p := v.(*pacer)
	p.mu.Lock()
	defer p.mu.Unlock()
	if until := time.Now().Add(d); until.After(p.next) {
		p.next = until
	}
}

// Pace is the interval set for host, 0 when it isn't paced.
func Pace(host string) time.Duration {
	v, ok := pacers.Load(host)
	if !ok {
		return 0
	}
	p := v.(*pacer)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interval
}

// Wait blocks until a request to host (or another paced key) may start.
func Wait(ctx context.Context, host string) error {
	v, ok := pacers.Load(host)
	if !ok {
		return ctx.Err()
	}
	p := v.(*pacer)
	p.mu.Lock()
	now := time.Now()
	start := now
	if p.next.After(now) {
		start = p.next
	}
	p.next = start.Add(p.interval)
	p.mu.Unlock()
	if d := start.Sub(now); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}
