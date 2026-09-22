package requestwatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A nil *Watcher must be a no-op: boot wiring passes the watcher through
// unconditionally, so the nil case is the "GitHub credentials absent" path.
func TestRunNilWatcherReturnsNil(t *testing.T) {
	var w *Watcher
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run on nil watcher = %v, want nil", err)
	}
}

// A watcher built over a nil client (the hive booted without usable GitHub
// credentials) must return nil immediately rather than block on ctx.
func TestRunNilClientReturnsNil(t *testing.T) {
	w := New(nil, nil, nil, nil, nil)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run with nil client = %v, want nil", err)
	}
}

// New must carry every dependency through to the struct: Run hands them
// verbatim to the two Start*RequestWatcher calls, so a dropped field would
// silently disable authorization or hold-label checks.
func TestNewWiresAllFields(t *testing.T) {
	c := github.NewClientForTest("http://127.0.0.1:0", "org", []string{"repo"}, discardLogger())
	prAuthz := func(agent string, fileUID int) error { return nil }
	issueAuthz := func(agent string, fileUID int, kind string) error { return nil }
	holdLabel := func(agent string) bool { return agent == "quality" }
	nowFn := func() time.Time { return time.Unix(42, 0) }

	w := New(c, prAuthz, issueAuthz, holdLabel, nowFn)
	if w.client != c {
		t.Errorf("New dropped client")
	}
	if w.prAuthz == nil || w.issueAuthz == nil {
		t.Errorf("New dropped an authorizer: pr=%v issue=%v", w.prAuthz, w.issueAuthz)
	}
	if w.holdLabel == nil || !w.holdLabel("quality") || w.holdLabel("other") {
		t.Errorf("New dropped or mangled holdLabel")
	}
	if w.nowFn == nil || !w.nowFn().Equal(time.Unix(42, 0)) {
		t.Errorf("New dropped or mangled nowFn")
	}
}

// With a real client, Run must block until the context ends and then surface
// ctx.Err(). The context is cancelled before Run so neither watcher loop ever
// gets a tick: the test never scans a request directory, which keeps it
// hermetic on hosts where /var/run/hive-metrics exists (live hives) and on
// hosts where it cannot be created (CI sandboxes) alike.
func TestRunReturnsContextErrorAfterCancel(t *testing.T) {
	c := github.NewClientForTest("http://127.0.0.1:0", "org", []string{"repo"}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- New(c, nil, nil, nil, nil).Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
