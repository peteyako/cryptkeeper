package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// TestNavToGoneBrowserIsNotAnError: a navigation whose browser has already moved on (its
// request context is cancelled) can't be patched. That happens whenever someone clicks
// through pages quickly, so it isn't logged as an error; a page that fails to render for a
// browser still waiting is.
func TestNavToGoneBrowserIsNotAnError(t *testing.T) {
	var logged bytes.Buffer
	s := &Server{Deps: Deps{Log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError}))}}
	nav := func(ctx context.Context) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/w/cold", nil).WithContext(ctx)
		r.Header.Set("Datastar-Request", "true")
		w := httptest.NewRecorder()
		s.render(w, r, templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := io.WriteString(w, `<main id="main">ok</main>`)
			return err
		}))
		return w
	}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	nav(gone)
	if logged.Len() != 0 {
		t.Errorf("a navigation to a browser that left was logged as an error:\n%s", logged.String())
	}

	logged.Reset()
	r := httptest.NewRequest("GET", "/w/cold", nil)
	r.Header.Set("Datastar-Request", "true")
	s.render(httptest.NewRecorder(), r, templ.ComponentFunc(func(context.Context, io.Writer) error {
		return errors.New("template broke")
	}))
	if !strings.Contains(logged.String(), "render nav") || !strings.Contains(logged.String(), "template broke") {
		t.Errorf("a page that failed to render wasn't logged as an error: %q", logged.String())
	}
}
