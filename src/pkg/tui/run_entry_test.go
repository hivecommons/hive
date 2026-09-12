package tui

// Coverage for the two entry-path pieces run()'s pipe test cannot reach:
// Run() itself — the os.Stdin/os.Stdout wrapper hivectl calls — and
// sseStream.terminalErr, the drop-side error selection.
//
// Run() is exercised through its preflight refusal: a dashboard answering 401
// makes Run return before bubbletea ever tries to take over the process's
// real terminal, so the test runs safely under `go test` with no TTY, while
// still executing the one statement Run owns (the os.Stdin/os.Stdout hand-off
// to run).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// TestRunRefusesOnUnauthorized: Run must surface preflight's 401 refusal —
// the shell-and-scrollback path an operator with a bad credential gets — and
// must do so without starting the program (returning at all, under a test
// runner with no terminal to give back, is the proof).
func TestRunRefusesOnUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	pinDashboard(t, server.URL)

	err := Run()
	if err == nil {
		t.Fatal("Run() = nil against a 401 dashboard, want the preflight refusal")
	}
	// The refusal must be preflight's help text, naming both credentials —
	// not a bubbletea terminal error, which would mean the program started.
	for _, want := range []string{client.TokenEnv, client.CookieEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error does not name %s:\n%v", want, err)
		}
	}
}

// TestSSEStreamTerminalErr covers the drop-side error selection: a buffered
// error wins, and every "nothing useful in errs" shape — empty-and-open,
// closed-empty, buffered nil — falls back to errSSEClosed rather than
// reporting a bare close that throws away nothing.
func TestSSEStreamTerminalErr(t *testing.T) {
	t.Run("buffered error is reported", func(t *testing.T) {
		sentinel := errors.New("stream broke")
		errs := make(chan error, 1)
		errs <- sentinel
		s := &sseStream{errs: errs}
		if got := s.terminalErr(); got != sentinel {
			t.Errorf("terminalErr() = %v, want the buffered error %v", got, sentinel)
		}
	})

	t.Run("closed channel still yields its buffered error", func(t *testing.T) {
		// The producer buffers its error and THEN closes (its defers run in
		// reverse); a receive from a closed buffered channel must still
		// deliver what it holds.
		sentinel := errors.New("buffered then closed")
		errs := make(chan error, 1)
		errs <- sentinel
		close(errs)
		s := &sseStream{errs: errs}
		if got := s.terminalErr(); got != sentinel {
			t.Errorf("terminalErr() = %v, want the buffered error %v", got, sentinel)
		}
	})

	t.Run("closed empty channel falls back to errSSEClosed", func(t *testing.T) {
		errs := make(chan error, 1)
		close(errs)
		s := &sseStream{errs: errs}
		if got := s.terminalErr(); got != errSSEClosed {
			t.Errorf("terminalErr() = %v, want errSSEClosed", got)
		}
	})

	t.Run("open empty channel falls back without blocking", func(t *testing.T) {
		s := &sseStream{errs: make(chan error, 1)}
		if got := s.terminalErr(); got != errSSEClosed {
			t.Errorf("terminalErr() = %v, want errSSEClosed", got)
		}
	})

	t.Run("buffered nil error falls back to errSSEClosed", func(t *testing.T) {
		errs := make(chan error, 1)
		errs <- nil
		s := &sseStream{errs: errs}
		if got := s.terminalErr(); got != errSSEClosed {
			t.Errorf("terminalErr() = %v, want errSSEClosed for a nil error", got)
		}
	})
}
