package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A sync moves the status through idle → listing → fetching/writing → done,
// counting pages as they are written, and a failed sync ends in failed with
// the error on the progress block.
func TestSyncerProgressTransitions(t *testing.T) {
	c := &fakeConn{typ: "fake", started: make(chan struct{}), release: make(chan struct{})}
	c.set([]Page{
		{ID: "a", Title: "First page", Markdown: "hello"},
		{ID: "b", Title: strings.Repeat("x", 200), Markdown: "world!!"},
	}, "c1", nil)
	s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, nil)

	st, _ := s.Status("x")
	if st.Progress.Phase != ProgressIdle || st.Progress.Active() || st.Syncing() {
		t.Fatalf("before first sync: %+v", st.Progress)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.SyncNow(context.Background(), "x")
		done <- err
	}()
	<-c.started
	st, _ = s.Status("x")
	if st.Progress.Phase != ProgressListing || !st.Progress.Active() || !st.Syncing() || st.Progress.StartedAt.IsZero() || st.Progress.ItemsDone != 0 {
		t.Fatalf("while listing: %+v", st.Progress)
	}
	close(c.release)
	if err := <-done; err != nil {
		t.Fatalf("sync: %v", err)
	}

	st, _ = s.Status("x")
	p := st.Progress
	if p.Phase != ProgressDone || p.Active() || st.Syncing() {
		t.Fatalf("after sync: %+v", p)
	}
	if p.ItemsDone != 2 || p.FactsWritten != 2 || p.BytesWritten != int64(len("hello")+len("world!!")) {
		t.Fatalf("counts: %+v", p)
	}
	if p.Current != "" || p.LastError != "" || p.FinishedAt.IsZero() || p.FinishedAt.Before(p.StartedAt) {
		t.Fatalf("done block: %+v", p)
	}

	// A failed sync keeps what was done and records why it stopped.
	c.set(nil, "", errors.New("upstream 500"))
	c.started, c.release = nil, nil
	if _, err := s.SyncNow(context.Background(), "x"); err == nil {
		t.Fatal("want sync error")
	}
	st, _ = s.Status("x")
	if st.Progress.Phase != ProgressFailed || st.Progress.LastError != "upstream 500" || st.Progress.Active() {
		t.Fatalf("after failure: %+v", st.Progress)
	}

	// A partial sync is done, not failed, but carries the partial error.
	c.set([]Page{{ID: "a", Title: "First page", Markdown: "hello"}}, "c2", &PartialError{Err: errors.New("1 skipped")})
	if _, err := s.SyncNow(context.Background(), "x"); !IsPartial(err) {
		t.Fatalf("want partial error, got %v", err)
	}
	st, _ = s.Status("x")
	if st.Progress.Phase != ProgressDone || st.Progress.LastError != "1 skipped" || st.Progress.ItemsDone != 1 {
		t.Fatalf("after partial: %+v", st.Progress)
	}
}

// The progress block is additive on the status JSON: old fields keep their
// names and `progress` carries the live fields the dashboard reads.
func TestStatusProgressJSONShape(t *testing.T) {
	st := Status{Name: "x", Type: "fake", Layer: "project", Progress: Progress{
		Phase: ProgressFetching, ItemsTotal: 200, ItemsDone: 12, Current: "Drive home",
		StartedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), BytesWritten: 4096, FactsWritten: 12,
	}}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "type", "layer", "enabled", "interval", "pages", "facts", "deprecated", "running", "truncated", "progress"} {
		if _, ok := got[key]; !ok {
			t.Errorf("status JSON missing %q: %s", key, data)
		}
	}
	p, _ := got["progress"].(map[string]any)
	if p["phase"] != "fetching" || p["items_total"] != float64(200) || p["items_done"] != float64(12) || p["current"] != "Drive home" {
		t.Fatalf("progress JSON = %v", p)
	}
	if p["started_at"] != "2026-11-14T09:00:00Z" || p["bytes_written"] != float64(4096) || p["facts_written"] != float64(12) {
		t.Fatalf("progress JSON = %v", p)
	}
	if _, ok := p["last_error"]; ok {
		t.Fatalf("empty last_error must be omitted: %v", p)
	}
	// Idle status: no total, no current, no error.
	data, _ = json.Marshal(Status{Progress: Progress{Phase: ProgressIdle}})
	if s := string(data); !strings.Contains(s, `"progress":{"phase":"idle","items_done":0,`) || strings.Contains(s, "items_total") || strings.Contains(s, `"current"`) {
		t.Fatalf("idle progress JSON = %s", s)
	}
}

func TestReportProgressHelpers(t *testing.T) {
	// No reporter in the context: a no-op, never a panic.
	reportProgress(context.Background(), func(p *Progress) { p.ItemsDone++ })

	var got Progress
	ctx := withProgressReporter(context.Background(), func(fn func(*Progress)) { fn(&got) })
	reportProgress(ctx, func(p *Progress) { p.Phase, p.ItemsTotal = ProgressFetching, 3 })
	if got.Phase != ProgressFetching || got.ItemsTotal != 3 {
		t.Fatalf("reported = %+v", got)
	}

	if s := progressTitle("  Standup ideas "); s != "Standup ideas" {
		t.Fatalf("trim = %q", s)
	}
	long := strings.Repeat("é", progressCurrentMaxRunes+5)
	if s := progressTitle(long); len([]rune(s)) != progressCurrentMaxRunes+1 || !strings.HasSuffix(s, "…") {
		t.Fatalf("truncate = %q", s)
	}
	for _, phase := range []string{ProgressIdle, ProgressDone, ProgressFailed, ""} {
		if (Progress{Phase: phase}).Active() {
			t.Errorf("%q must not be active", phase)
		}
	}
	for _, phase := range []string{ProgressListing, ProgressFetching, ProgressWriting} {
		if !(Progress{Phase: phase}).Active() {
			t.Errorf("%q must be active", phase)
		}
	}
}

// Plaud reports the recording count after list_files, the recording being
// fetched, and counts skipped recordings (which never reach emit) itself.
func TestPlaudSyncReportsProgress(t *testing.T) {
	pc := newTestPlaud(t, newPlaudTestServer(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var got Progress
	var phases []string
	var currents []string
	ctx = withProgressReporter(ctx, func(fn func(*Progress)) {
		fn(&got)
		if n := len(phases); n == 0 || phases[n-1] != got.Phase {
			phases = append(phases, got.Phase)
		}
		if n := len(currents); got.Current != "" && (n == 0 || currents[n-1] != got.Current) {
			currents = append(currents, got.Current)
		}
	})
	emitted := 0
	_, err := pc.Sync(ctx, "", func(Page) error { emitted++; return nil })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// rec-old is dropped by since_days, so two recordings are listed. Both
	// are emitted: an untranscribed recording (rec-2) is a pending page, not
	// a skipped one, so the connector counts nothing itself (the syncer counts
	// emitted pages).
	if got.ItemsTotal != 2 || emitted != 2 {
		t.Fatalf("total = %d, emitted = %d", got.ItemsTotal, emitted)
	}
	if got.ItemsDone != 0 {
		t.Fatalf("items_done = %d, want 0 (nothing skipped)", got.ItemsDone)
	}
	if want := []string{ProgressListing, ProgressFetching}; strings.Join(phases, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v", phases)
	}
	if want := []string{"Standup ideas", "Drive home"}; strings.Join(currents, ",") != strings.Join(want, ",") {
		t.Fatalf("current titles = %v", currents)
	}
}
