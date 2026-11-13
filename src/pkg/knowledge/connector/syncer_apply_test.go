package connector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// applyTestTimeout bounds every wait in the Apply tests.
const applyTestTimeout = 5 * time.Second

func statusNames(s *Syncer) []string {
	var out []string
	for _, st := range s.Statuses() {
		out = append(out, st.Name)
	}
	return out
}

func TestSyncerApply(t *testing.T) {
	a := &fakeConn{typ: "fake"}
	a.set([]Page{{ID: "a1", Title: "A", Markdown: "a"}}, "ca", nil)
	b := &fakeConn{typ: "fake"}
	b.set([]Page{{ID: "b1", Title: "B", Markdown: "b"}}, "cb", nil)
	statePath := filepath.Join(t.TempDir(), "status.json")
	s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("a")}, map[string]Connector{"a": a, "b": b}, func(o *SyncerOptions) {
		o.StatePath = statePath
	})
	if _, err := s.SyncNow(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}

	// Add: the existing connector keeps its status, the new one is pending.
	if err := s.Apply([]ConnectorConfig{fakeCfg("a"), fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	if got := statusNames(s); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("names after add = %v", got)
	}
	if st, _ := s.Status("a"); st.Cursor != "ca" || st.LastSync.IsZero() || st.Pages != 1 {
		t.Fatalf("a after add = %+v, want status preserved", st)
	}
	if st, _ := s.Status("b"); !st.LastSync.IsZero() || st.Type != "fake" || !st.Enabled {
		t.Fatalf("b after add = %+v, want pending", st)
	}
	if _, err := s.SyncNow(context.Background(), "b"); err != nil {
		t.Fatalf("SyncNow added connector: %v", err)
	}

	// Edit (interval, enabled) keeps the cursor; a type or layer change resets it.
	edited := fakeCfg("a")
	edited.Enabled, edited.Interval = false, 2*time.Hour
	if err := s.Apply([]ConnectorConfig{edited, fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("a"); st.Enabled || st.Interval != (2*time.Hour).String() || st.Cursor != "ca" {
		t.Fatalf("a after edit = %+v, want disabled, 2h, cursor kept", st)
	}
	moved := fakeCfg("a")
	moved.Layer = "org"
	if err := s.Apply([]ConnectorConfig{moved, fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("a"); st.Cursor != "" || st.Pages != 0 || st.Layer != "org" || !st.Enabled {
		t.Fatalf("a after layer change = %+v, want reset", st)
	}

	// Reorder follows the applied list.
	if err := s.Apply([]ConnectorConfig{fakeCfg("b"), moved}); err != nil {
		t.Fatal(err)
	}
	if got := statusNames(s); got[0] != "b" || got[1] != "a" {
		t.Fatalf("names after reorder = %v", got)
	}

	// Duplicates are rejected and leave the syncer untouched.
	if err := s.Apply([]ConnectorConfig{fakeCfg("b"), fakeCfg("b")}); err == nil {
		t.Fatal("duplicate names must fail")
	}
	if got := statusNames(s); len(got) != 2 {
		t.Fatalf("names after rejected apply = %v", got)
	}

	// Remove: gone from statuses, SyncNow, Connector and the persisted state.
	if err := s.Apply([]ConnectorConfig{fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	if got := statusNames(s); len(got) != 1 || got[0] != "b" {
		t.Fatalf("names after remove = %v", got)
	}
	if _, err := s.SyncNow(context.Background(), "a"); !errors.Is(err, ErrUnknownConnector) {
		t.Fatalf("SyncNow removed = %v, want ErrUnknownConnector", err)
	}
	if _, ok := s.Connector("a"); ok {
		t.Fatal("Connector must not return a removed connector")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if _, has := persisted["a"]; has {
		t.Fatalf("removed connector still persisted: %s", data)
	}
	if _, has := persisted["b"]; !has {
		t.Fatalf("kept connector missing from state: %s", data)
	}

	// Everything removed leaves an empty, still-usable syncer.
	if err := s.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if got := s.Statuses(); len(got) != 0 {
		t.Fatalf("statuses after removing all = %+v", got)
	}
}

func TestSyncerApplyStartsLoopWhileRunning(t *testing.T) {
	b := &fakeConn{typ: "fake"}
	b.set([]Page{{ID: "b1", Title: "B", Markdown: "b"}}, "cb", nil)
	s, _ := newTestSyncer(t, nil, map[string]Connector{"b": b}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	// A connector added while Run is active gets its interval loop.
	if err := s.Apply([]ConnectorConfig{fakeCfg("b")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(applyTestTimeout)
	for b.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("added connector was never synced by the run loop")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Removing it stops the loop and drops the status; Run still returns.
	if err := s.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if got := s.Statuses(); len(got) != 0 {
		t.Fatalf("statuses after remove = %+v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(applyTestTimeout):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSyncerUpdateIgnoresReplacedEntry(t *testing.T) {
	a := &fakeConn{typ: "fake", started: make(chan struct{}, 1), release: make(chan struct{})}
	a.set([]Page{{ID: "a1", Title: "A", Markdown: "a"}}, "ca", nil)
	s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("a")}, map[string]Connector{"a": a}, nil)

	// A sync is mid-flight when the connector is removed: its completion
	// must not resurrect the status.
	result := make(chan error, 1)
	go func() {
		_, err := s.SyncNow(context.Background(), "a")
		result <- err
	}()
	select {
	case <-a.started:
	case <-time.After(applyTestTimeout):
		t.Fatal("sync never started")
	}
	if err := s.Apply(nil); err != nil {
		t.Fatal(err)
	}
	close(a.release)
	select {
	case <-result:
	case <-time.After(applyTestTimeout):
		t.Fatal("sync never finished")
	}
	if got := s.Statuses(); len(got) != 0 {
		t.Fatalf("finished sync resurrected a removed connector: %+v", got)
	}
}
