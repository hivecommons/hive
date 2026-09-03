package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/reach"
	"github.com/hivecommons/hive/pkg/tracing"
)

// The admin advisory-diagnostics endpoint answers the fleet false-negative
// prevalence question; the snapshot, log line, and HTTP surface must all
// agree with buildAdvisoryDiagnostics over the live registry.
func TestAdvisoryDiagnosticsEndpoint(t *testing.T) {
	srv := newHubServerForTest(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	srv.registry.Hives = []RegistryEntry{
		{ID: "h-fresh", Online: true, AdvisoryLastPostedAt: now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)},
		{ID: "h-none", Online: true},
	}

	rep := srv.advisoryDiagnostics(now)
	if rep.TotalHives != 2 {
		t.Fatalf("TotalHives = %d, want 2", rep.TotalHives)
	}
	if rep.Counts[advisoryClassNotParticipating] != 1 {
		t.Errorf("not-participating count = %d, want 1", rep.Counts[advisoryClassNotParticipating])
	}

	// The structured log line must render without panicking and carry the
	// headline counters.
	var buf bytes.Buffer
	srv.logger = slog.New(slog.NewTextHandler(&buf, nil))
	srv.logAdvisoryDiagnostics(rep)
	if !bytes.Contains(buf.Bytes(), []byte("advisory diagnostics")) {
		t.Errorf("log line missing: %s", buf.String())
	}

	rr := httptest.NewRecorder()
	srv.handleAdvisoryDiagnostics(rr, httptest.NewRequest("GET", "/api/saas/admin/advisory-diagnostics", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var got AdvisoryDiagnosticsReport
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if got.TotalHives != 2 || len(got.Hives) != 2 {
		t.Errorf("HTTP report = %d hives (%d rows), want 2", got.TotalHives, len(got.Hives))
	}
}

// The diagnostics ticker must honor context cancellation so the pollers all
// stop when the composition root's context ends (and so tests can bound it).
// It performs one immediate sample, then exits on a cancelled context.
func TestStartAdvisoryDiagnosticsStopsOnCancel(t *testing.T) {
	srv := newHubServerForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		srv.StartAdvisoryDiagnostics(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartAdvisoryDiagnostics did not return on cancelled context")
	}
}

// RegistryReachReporter is the producer→consumer wiring of the reach epic:
// it must snapshot only well-formed entries and skip hives without reports.
func TestRegistryReachReporterLatestReach(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.registry.Hives = []RegistryEntry{
		{ID: "h-good", ComponentReach: &tracing.ReachReport{Entries: []tracing.ReachEntry{
			{Component: "hub", Commit: "abc1234", SpansTotal: 10, SpansError: 1,
				FirstSeen: "2026-08-22T01:00:00Z", LastSeen: "2026-08-22T02:00:00Z"},
			{Component: "corrupt", Commit: "def5678", FirstSeen: "garbage", LastSeen: "2026-08-22T02:00:00Z"},
		}}},
		{ID: "h-empty", ComponentReach: &tracing.ReachReport{}},
		{ID: "h-none"},
		{ID: "h-all-corrupt", ComponentReach: &tracing.ReachReport{Entries: []tracing.ReachEntry{
			{Component: "x", FirstSeen: "nope", LastSeen: "nope"},
		}}},
	}

	got := srv.RegistryReachReporter().LatestReach()
	if len(got) != 1 {
		t.Fatalf("LatestReach hives = %d, want 1 (only h-good): %v", len(got), got)
	}
	list := got["h-good"]
	if len(list) != 1 || list[0].Component != "hub" || list[0].SpansTotal != 10 {
		t.Errorf("h-good entries = %+v, want the one well-formed entry", list)
	}
}

func TestSetReachWiring(t *testing.T) {
	srv := newHubServerForTest(t)

	// nil reporter must keep the stub (fail-safe default), non-nil replaces it.
	before := srv.reachReporter
	srv.SetReachReporter(nil)
	if srv.reachReporter != before {
		t.Error("SetReachReporter(nil) must not clear the default reporter")
	}
	rep := srv.RegistryReachReporter()
	srv.SetReachReporter(rep)
	if srv.reachReporter != rep {
		t.Error("SetReachReporter did not install the reporter")
	}

	src := NewGitHubPRSource(nil, "v4")
	srv.SetReachPRSource(src)
	if srv.reachPRSource != src {
		t.Error("SetReachPRSource did not install the source")
	}
}

// newReachAncestry picks the local-clone backend only when the operator set
// HIVE_REACH_REPO_DIR; the container default is the compare-API adapter.
func TestNewReachAncestryBackendSelection(t *testing.T) {
	logger := slog.Default()

	t.Setenv(reachRepoDirEnv, "")
	if _, ok := newReachAncestry(logger).(compareAncestry); !ok {
		t.Error("without HIVE_REACH_REPO_DIR the compare-API backend must be used")
	}

	t.Setenv(reachRepoDirEnv, t.TempDir())
	if _, ok := newReachAncestry(logger).(*reach.GitAncestry); !ok {
		t.Error("with HIVE_REACH_REPO_DIR the git-clone backend must be used")
	}
}

func TestEvictOldestCursor(t *testing.T) {
	hist := &reachComponentHistory{Cursors: map[string]reachHiveCursor{
		"h-old": {Bucket: time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)},
		"h-new": {Bucket: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)},
	}}
	evictOldestCursor(hist)
	if _, still := hist.Cursors["h-old"]; still {
		t.Error("oldest cursor h-old should have been evicted")
	}
	if _, kept := hist.Cursors["h-new"]; !kept {
		t.Error("newest cursor h-new must survive eviction")
	}
	// Empty map: a no-op, not a panic.
	evictOldestCursor(&reachComponentHistory{Cursors: map[string]reachHiveCursor{}})
}

func TestReachHistoryMaybeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reach.json")
	store := newReachHistoryStore(path, slog.Default())

	now := time.Now()

	// Clean store: nothing to persist.
	store.maybeSave(now)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("clean store must not write")
	}

	// Dirty but inside the save interval: deferred.
	store.mu.Lock()
	store.dirty = true
	store.lastSave = now
	store.mu.Unlock()
	store.maybeSave(now.Add(time.Second))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("save interval not honored")
	}

	// Dirty and past the interval: persists and clears dirty.
	store.maybeSave(now.Add(reachHistorySaveInterval + time.Second))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected persisted history file: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.dirty {
		t.Error("dirty flag must clear after save")
	}
}

// write failures are telemetry, never fatal: a bad path logs a warning and
// the store carries on.
func TestReachHistoryWriteFailureIsNonFatal(t *testing.T) {
	var buf bytes.Buffer
	store := newReachHistoryStore(
		filepath.Join(t.TempDir(), "no-such-dir", "reach.json"),
		slog.New(slog.NewTextHandler(&buf, nil)))
	store.write([]byte("{}"))
	if !bytes.Contains(buf.Bytes(), []byte("reach history save failed")) {
		t.Errorf("write failure not logged: %s", buf.String())
	}
}
