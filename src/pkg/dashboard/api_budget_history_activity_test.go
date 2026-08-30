package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// --- handleBudgetHistory ---

// A hive that has never seen a window roll and has no live status must still
// answer with an empty windows array (never null) and no "current" key.
func TestBudgetHistory_EmptyHistoryNoStatus(t *testing.T) {
	s, _ := apiServer(t)

	rec := doGet(s, "/api/budget/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	windows, ok := body["windows"].([]interface{})
	if !ok {
		t.Fatalf("windows should be an array, got %T (%v)", body["windows"], body["windows"])
	}
	if len(windows) != 0 {
		t.Errorf("windows = %v, want empty", windows)
	}
	if _, ok := body["current"]; ok {
		t.Errorf("current should be absent when no status has been published, got %v", body["current"])
	}
}

// With a live status carrying an open window, the report includes a "current"
// block with the open window's spend and both bounds, alongside the closed rows.
func TestBudgetHistory_CurrentWindowFromStatus(t *testing.T) {
	s, _ := apiServer(t)

	s.SeedBudgetWindowHistory([]BudgetWindowEntry{
		{WindowStart: 1000, WindowEnd: 2000, Limit: 500, Used: 500, PctUsed: 100, Exhausted: true},
	})
	s.statusMu.Lock()
	s.status = &StatusPayload{Budget: FrontendBudget{
		WeeklyBudget:   1_000_000,
		Used:           250_000,
		PctUsed:        25,
		Exhausted:      false,
		WindowStartsAt: "2026-08-24T00:00:00Z",
		WindowEndsAt:   "2026-08-31T00:00:00Z",
	}}
	s.statusMu.Unlock()

	rec := doGet(s, "/api/budget/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)

	windows, ok := body["windows"].([]interface{})
	if !ok || len(windows) != 1 {
		t.Fatalf("windows = %v, want 1 closed row", body["windows"])
	}
	current, ok := body["current"].(map[string]interface{})
	if !ok {
		t.Fatalf("current missing or wrong type: %v", body["current"])
	}
	if got := current["limit"].(float64); got != 1_000_000 {
		t.Errorf("current.limit = %v, want 1000000", got)
	}
	if got := current["used"].(float64); got != 250_000 {
		t.Errorf("current.used = %v, want 250000", got)
	}
	if got := current["pctUsed"].(float64); got != 25 {
		t.Errorf("current.pctUsed = %v, want 25", got)
	}
	if got := current["exhausted"].(bool); got {
		t.Errorf("current.exhausted = true, want false")
	}
	if got := current["windowStart"]; got != "2026-08-24T00:00:00Z" {
		t.Errorf("current.windowStart = %v", got)
	}
	if got := current["windowEnd"]; got != "2026-08-31T00:00:00Z" {
		t.Errorf("current.windowEnd = %v", got)
	}
}

// When no weekly limit is set the status carries empty window bounds; the
// current block must omit windowStart/windowEnd rather than emit empty strings.
func TestBudgetHistory_CurrentWindowOmitsEmptyBounds(t *testing.T) {
	s, _ := apiServer(t)

	s.statusMu.Lock()
	s.status = &StatusPayload{Budget: FrontendBudget{Exhausted: true}}
	s.statusMu.Unlock()

	rec := doGet(s, "/api/budget/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	current, ok := body["current"].(map[string]interface{})
	if !ok {
		t.Fatalf("current missing: %v", body)
	}
	if _, ok := current["windowStart"]; ok {
		t.Errorf("windowStart should be omitted when unset, got %v", current["windowStart"])
	}
	if _, ok := current["windowEnd"]; ok {
		t.Errorf("windowEnd should be omitted when unset, got %v", current["windowEnd"])
	}
	if got := current["exhausted"].(bool); !got {
		t.Errorf("current.exhausted = false, want true")
	}
}

// --- document/knowledge handler 503 gates ---

// Every knowledge-backed endpoint must refuse with 503 when the server has no
// dependencies at all (ensureKnowledge false), instead of dereferencing nil.
func TestKnowledgeHandlers_NilDepsServiceUnavailable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(0, logger) // no RegisterAPI: s.deps stays nil

	handlers := map[string]http.HandlerFunc{
		"documents list":    s.handleDocumentsList,
		"documents import":  s.handleDocumentsImport,
		"document get":      s.handleDocumentGet,
		"document delete":   s.handleDocumentDelete,
		"document reimport": s.handleDocumentReimport,
		"cleanup orphans":   s.handleCleanupOrphans,
		"context7 search":   s.handleContext7Search,
	}
	for name, h := range handlers {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", name, rec.Code)
		}
	}
}

// --- ActivityCollector ---

// stubAuditReader is a canned auditReader so Start/collect run without a real
// audit log on disk.
type stubAuditReader struct {
	entries []AuditEntry
	calls   atomic.Int32
}

func (f *stubAuditReader) OutputActionsSince(time.Time, map[string]bool, string) []AuditEntry {
	f.calls.Add(1)
	return f.entries
}

func TestActivityCollector_CollectedAt(t *testing.T) {
	var nilAC *ActivityCollector
	if !nilAC.CollectedAt().IsZero() {
		t.Error("nil collector CollectedAt should be zero")
	}

	ac := NewActivityCollector(&stubAuditReader{}, "", nil)
	if !ac.CollectedAt().IsZero() {
		t.Error("fresh collector CollectedAt should be zero")
	}
	ac.collect()
	if ac.CollectedAt().IsZero() {
		t.Error("CollectedAt should be set after a collect")
	}
}

// Start must be a no-op on a nil collector and on one with no audit reader.
func TestActivityCollector_StartInert(t *testing.T) {
	done := make(chan struct{})
	go func() {
		var nilAC *ActivityCollector
		nilAC.Start(context.Background())
		NewActivityCollector(nil, "", nil).Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return immediately for inert collectors")
	}
}

// Start collects once up front, again on each tick, and exits on ctx cancel.
func TestActivityCollector_StartCollectsAndStops(t *testing.T) {
	oldInterval := activityCollectInterval
	activityCollectInterval = 5 * time.Millisecond
	defer func() { activityCollectInterval = oldInterval }()

	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(time.Now()), Action: "agent_pr_created", Detail: "repo=o/r, agent=quality"},
	}}
	ac := NewActivityCollector(stub, "ignored", nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ac.Start(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for stub.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after ctx cancel")
	}
	if stub.calls.Load() < 2 {
		t.Errorf("collect calls = %d, want >=2 (upfront + tick)", stub.calls.Load())
	}
	snap, ready := ac.Snapshot()
	if !ready {
		t.Fatal("snapshot should be ready after collect")
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Repo != "o/r" {
		t.Errorf("snapshot repos = %+v, want one entry for o/r", snap.Repos)
	}
}

// persistLocked failure paths: an unwritable temp path and a rename target that
// is a directory must both be swallowed (logged), never panic or persist junk.
func TestActivityCollector_PersistLockedFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// No persist path configured → early return.
	ac := NewActivityCollector(&stubAuditReader{}, "", logger)
	ac.persistLocked()

	// Temp-file write fails: parent directory does not exist.
	ac.persistPath = filepath.Join(t.TempDir(), "missing-subdir", "activity.json")
	ac.persistLocked()

	// Rename fails: destination is an existing directory.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "activity.json")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	ac.persistPath = blocked
	ac.persistLocked()
	if _, err := os.Stat(filepath.Join(dir, "activity.json.tmp")); err != nil {
		t.Fatalf("temp sidecar should exist after failed rename: %v", err)
	}
}

// EnablePersistence restore paths: corrupt JSON and a zero collected_at are
// both "start fresh"; a valid sidecar restores snapshot + timestamp.
func TestActivityCollector_EnablePersistenceRestore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()

	// Corrupt sidecar → logged, ignored, not ready.
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ac := NewActivityCollector(&stubAuditReader{}, "", logger)
	ac.EnablePersistence(corrupt)
	if _, ready := ac.Snapshot(); ready {
		t.Error("corrupt sidecar must not mark the collector ready")
	}

	// Zero collected_at → treated as empty, not ready.
	zero := filepath.Join(dir, "zero.json")
	if err := os.WriteFile(zero, []byte(`{"snapshot":{},"collected_at":"0001-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ac2 := NewActivityCollector(&stubAuditReader{}, "", logger)
	ac2.EnablePersistence(zero)
	if _, ready := ac2.Snapshot(); ready {
		t.Error("zero collected_at must not mark the collector ready")
	}

	// Round-trip: collect+persist in one collector, restore in a second.
	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(time.Now()), Action: "pr_merged", Detail: "repo=o/persisted, agent=quality"},
	}}
	sidecar := filepath.Join(dir, "activity.json")
	writer := NewActivityCollector(stub, "ignored", logger)
	writer.EnablePersistence(sidecar)
	writer.collect()

	restored := NewActivityCollector(&stubAuditReader{}, "", logger)
	restored.EnablePersistence(sidecar)
	snap, ready := restored.Snapshot()
	if !ready {
		t.Fatal("restored collector should be ready")
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Repo != "o/persisted" {
		t.Errorf("restored repos = %+v, want o/persisted", snap.Repos)
	}
	if restored.CollectedAt().IsZero() {
		t.Error("restored CollectedAt should be non-zero")
	}
}
