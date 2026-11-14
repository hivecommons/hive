package connector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func statusNames(s *Syncer) []string {
	var out []string
	for _, st := range s.Statuses() {
		out = append(out, st.Name)
	}
	return out
}

// Reconfigure adds, replaces and removes connectors in place (#18091).
func TestSyncerReconfigure(t *testing.T) {
	a, b := &fakeConn{typ: "fake"}, &fakeConn{typ: "fake"}
	s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("a")}, map[string]Connector{"a": a, "b": b}, nil)
	if _, err := s.SyncNow(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}

	changed := fakeCfg("a")
	changed.Interval = 2 * time.Hour
	if err := s.Reconfigure([]ConnectorConfig{fakeCfg("b"), changed}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(statusNames(s), ","); got != "b,a" {
		t.Fatalf("statuses = %s, want b,a (config order)", got)
	}
	st, _ := s.Status("a")
	if st.Interval != (2 * time.Hour).String() {
		t.Fatalf("changed interval not applied: %+v", st)
	}
	if st.LastSync.IsZero() {
		t.Fatal("a changed connector must keep its sync status, as across a restart")
	}
	if _, err := s.SyncNow(context.Background(), "b"); err != nil {
		t.Fatalf("added connector not syncable: %v", err)
	}

	if err := s.Reconfigure([]ConnectorConfig{fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(statusNames(s), ","); got != "b" {
		t.Fatalf("statuses = %s, want b", got)
	}
	if _, err := s.SyncNow(context.Background(), "a"); !errors.Is(err, ErrUnknownConnector) {
		t.Fatalf("removed connector SyncNow err = %v, want ErrUnknownConnector", err)
	}
	if _, ok := s.Connector("a"); ok {
		t.Fatal("removed connector still resolvable")
	}

	if err := s.Reconfigure([]ConnectorConfig{fakeCfg("b"), fakeCfg("b")}); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("duplicate err = %v", err)
	}
	if got := strings.Join(statusNames(s), ","); got != "b" {
		t.Fatalf("a rejected Reconfigure changed the syncer: %s", got)
	}
}

// A connector whose config failed to build recovers once Reconfigure gives it
// a buildable config, and its stale build error is cleared.
func TestSyncerReconfigureClearsBuildError(t *testing.T) {
	s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{}, nil)
	if st, _ := s.Status("x"); st.LastError == "" {
		t.Fatal("expected a build error for x")
	}
	// Swap the factory's answer by re-registering under a new type.
	if err := s.opts.Registry.Register("fake2", func(ConnectorConfig, Deps) (Connector, error) {
		return &fakeConn{typ: "fake2"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	fixed := fakeCfg("x")
	fixed.Type = "fake2"
	if err := s.Reconfigure([]ConnectorConfig{fixed}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("x"); st.LastError != "" || st.Type != "fake2" {
		t.Fatalf("status after fix = %+v", st)
	}
	if _, ok := s.Connector("x"); !ok {
		t.Fatal("fixed connector not built")
	}
}

// While Run is active, Reconfigure starts loops for added connectors and
// stops loops for removed or disabled ones.
func TestSyncerReconfigureWhileRunning(t *testing.T) {
	a, b := &fakeConn{typ: "fake"}, &fakeConn{typ: "fake"}
	s, _ := newTestSyncer(t, nil, map[string]Connector{"a": a, "b": b}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	waitCalls := func(c *fakeConn, n int32) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for c.calls.Load() < n {
			select {
			case <-deadline:
				t.Fatalf("connector synced %d times, want >= %d", c.calls.Load(), n)
			case <-time.After(time.Millisecond):
			}
		}
	}

	fast := fakeCfg("a")
	fast.Interval = 5 * time.Millisecond
	if err := s.Reconfigure([]ConnectorConfig{fast}); err != nil {
		t.Fatal(err)
	}
	waitCalls(a, 2)

	off := fast
	off.Enabled = false
	if err := s.Reconfigure([]ConnectorConfig{off}); err != nil {
		t.Fatal(err)
	}
	// a was the only loop, so s.loops drains exactly when disabling it has
	// stopped its loop (after at most one in-flight sync).
	stopped := make(chan struct{})
	go func() {
		s.loops.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("disabled connector's loop did not stop")
	}
	before := a.calls.Load()

	if err := s.Reconfigure([]ConnectorConfig{off, fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	waitCalls(b, 1)
	if after := a.calls.Load(); after != before {
		t.Fatalf("disabled connector kept syncing: %d -> %d", before, after)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
