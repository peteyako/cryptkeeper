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
	"time"
)

const userAgent = "cryptkeep/1.0"

// Client wraps http.Client with retry behaviour.
type Client struct {
	HTTP       *http.Client
	MaxRetries int
	BaseDelay  time.Duration
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
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = &retryAfterError{StatusError{resp.StatusCode, string(body), safeURL(req)}, retryAfter(resp)}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return body, &StatusError{resp.StatusCode, string(body), safeURL(req)}
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

func retryAfter(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 60 {
			return time.Duration(n) * time.Second
		}
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
