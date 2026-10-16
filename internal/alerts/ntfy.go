package alerts

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/httpx"
)

// Notification is one message to deliver.
type Notification struct {
	RuleID   string
	Title    string
	Body     string
	Priority int // 1..5, ntfy scale (5 = urgent); 0 means default (3)
	Tags     []string
	Click    string // URL opened when the notification is tapped; empty = none
	Time     time.Time
}

// Notifier delivers notifications. Implementations must be safe for concurrent use.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
}

// Ntfy delivers notifications to an ntfy topic with a plain-text POST:
// the body is the message, and Title / Priority / Tags / Click / Authorization are headers.
type Ntfy struct {
	URL    string // full topic URL, e.g. https://ntfy.sh/my-secret-topic
	Token  string // access token, sent as "Authorization: Bearer"; empty = anonymous
	Client *httpx.Client
}

// Send posts n to the topic. Any non-2xx response is an error. The topic URL and token are
// never part of the returned error text, since errors end up in the alert log and the UI.
func (nt *Ntfy) Send(ctx context.Context, n Notification) error {
	u, err := url.Parse(nt.URL)
	if err != nil || nt.URL == "" {
		return errors.New("ntfy: no topic URL configured")
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return errors.New("ntfy: topic URL must be an http(s) URL")
	}
	c := nt.Client
	if c == nil {
		c = httpx.New()
	}
	priority := n.Priority
	if priority == 0 {
		priority = 3
	}
	priority = min(max(priority, 1), 5)

	_, err = c.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, nt.URL, strings.NewReader(n.Body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		if n.Title != "" {
			req.Header.Set("Title", headerValue(n.Title))
		}
		req.Header.Set("Priority", strconv.Itoa(priority))
		if len(n.Tags) > 0 {
			req.Header.Set("Tags", headerValue(strings.Join(n.Tags, ",")))
		}
		if n.Click != "" {
			req.Header.Set("Click", headerValue(n.Click))
		}
		if nt.Token != "" {
			req.Header.Set("Authorization", "Bearer "+nt.Token)
		}
		return req, nil
	})
	if err == nil {
		return nil
	}
	var se *httpx.StatusError
	if errors.As(err, &se) {
		body := strings.TrimSpace(se.Body)
		if len(body) > 200 {
			body = body[:200] + "…"
		}
		return fmt.Errorf("ntfy: HTTP %d: %s", se.Status, body)
	}
	msg := err.Error()
	for _, secret := range []string{nt.URL, u.Redacted(), nt.Token} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return &sanitizedError{msg: "ntfy: " + msg, err: err}
}

// sanitizedError carries a scrubbed message but still unwraps to the cause, so callers can
// test for context.Canceled and friends.
type sanitizedError struct {
	msg string
	err error
}

func (e *sanitizedError) Error() string { return e.msg }
func (e *sanitizedError) Unwrap() error { return e.err }

// headerValue makes s safe as an HTTP header value: line breaks become spaces (no header
// injection) and non-ASCII text is RFC 2047 encoded, which ntfy decodes.
func headerValue(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return mime.QEncoding.Encode("utf-8", s)
}
