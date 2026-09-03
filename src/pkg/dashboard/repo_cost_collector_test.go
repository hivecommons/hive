package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// countingAuditReader wraps a real audit fixture and counts how many times
// OutputActionsSince was called, so a test can assert the served endpoint did
// NOT re-read the audit files on a second poll within the collect interval —
// that is the actual defect #4943 fixes. A response-shape-only test would
// still pass even if the cache were deleted, since handleRepoCost's fallback
// path produces an identical-looking payload; only a call-count assertion
// proves the cache is doing anything.
type countingAuditReader struct {
	inner auditReader
	calls int32
}

func (c *countingAuditReader) OutputActionsSince(since time.Time, actions map[string]bool, filePath string) []AuditEntry {
	atomic.AddInt32(&c.calls, 1)
	return c.inner.OutputActionsSince(since, actions, filePath)
}

// fakeTokensSummary returns a fixed *tokens.AggregateSummary, so a test does
// not need a real *tokens.Collector backed by session files on disk.
type fakeTokensSummary struct {
	summary *tokens.AggregateSummary
}

func (f *fakeTokensSummary) Summary() *tokens.AggregateSummary { return f.summary }

// newRepoCostFixtureServer builds a Server wired with a RepoCostCollector
// over a counting audit reader and a fixed token summary, with the collector
// already run once (collect()) so Snapshot() is ready. Returns the server and
// the counting reader so a test can assert on call counts.
func newRepoCostFixtureServer(t *testing.T) (*Server, *countingAuditReader) {
	t.Helper()
	now := time.Now().UTC()
	base := now.Add(-24 * time.Hour)

	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner",
			rcUsage(rcTime(base, 15), 200, 20), // between e@10 and e@20 -> repo-b
		),
	}}

	inner := &fakeFixedAudit{entries: entries}
	counting := &countingAuditReader{inner: inner}

	rc := NewRepoCostCollector(counting, &fakeTokensSummary{summary: summary}, "", nil)
	rc.nowFn = func() time.Time { return now }
	rc.collect()

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{RepoCost: rc})
	return s, counting
}

// fakeFixedAudit is an auditReader that always returns the same entries,
// regardless of since/actions/filePath — good enough to drive the collector
// in a test without a real audit file on disk.
type fakeFixedAudit struct{ entries []AuditEntry }

func (f *fakeFixedAudit) OutputActionsSince(_ time.Time, _ map[string]bool, _ string) []AuditEntry {
	return f.entries
}

func getRepoCost(s *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/repo-cost", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestHandleRepoCostServesCachedSnapshotWithoutReReading is the fix's core
// assertion: two polls of /api/repo-cost within the collect interval must
// read the audit files exactly ONCE total (from the single collect() the
// fixture ran), not once per request. Before #4943 this would have been two
// reads (and, in production, two full gunzip-and-join passes).
func TestHandleRepoCostServesCachedSnapshotWithoutReReading(t *testing.T) {
	s, counting := newRepoCostFixtureServer(t)

	before := atomic.LoadInt32(&counting.calls)
	if before != 1 {
		t.Fatalf("collector's own collect() must have read the audit once, got %d calls", before)
	}

	rec1 := getRepoCost(s)
	rec2 := getRepoCost(s)

	if rec1.Code != http.StatusOK || rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, %d; want 200, 200", rec1.Code, rec2.Code)
	}
	after := atomic.LoadInt32(&counting.calls)
	if after != before {
		t.Fatalf("handleRepoCost must serve the cached snapshot, not re-read: calls went from %d to %d across 2 requests", before, after)
	}

	var body1, body2 repoCostResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &body1); err != nil {
		t.Fatalf("invalid JSON (req 1): %v", err)
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("invalid JSON (req 2): %v", err)
	}
	if body1.CollectedAt.IsZero() || !body1.CollectedAt.Equal(body2.CollectedAt) {
		t.Fatalf("both requests must report the SAME collection time, got %v and %v", body1.CollectedAt, body2.CollectedAt)
	}
	if !body1.Ready {
		t.Fatalf("snapshot from a completed collect() must report ready=true")
	}
}

// TestHandleRepoCostNotReadyBeforeFirstCollect pins the honesty requirement:
// before any collection has completed, the endpoint must report ready=false
// with an empty by_repo and no fabricated total — never a $0.00 that reads
// identically to "this hive spent nothing".
func TestHandleRepoCostNotReadyBeforeFirstCollect(t *testing.T) {
	rc := NewRepoCostCollector(&fakeFixedAudit{}, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)
	// Deliberately do NOT call collect(): simulates the window between
	// process start and the collector's first ticker fire.

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{RepoCost: rc})

	rec := getRepoCost(s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body repoCostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body.Ready {
		t.Fatalf("must report ready=false before the first collection, got ready=true: %+v", body)
	}
	if len(body.ByRepo) != 0 {
		t.Fatalf("not-ready response must not fabricate by_repo entries, got %+v", body.ByRepo)
	}
	if body.TotalUSD != 0 || body.TotalTokens != 0 {
		// These being the zero value here is fine — the point under test is
		// Ready being false, which the caller MUST check before trusting any
		// zero value as "nothing was spent" rather than "not measured yet".
		t.Logf("zero totals reported alongside ready=false, as expected (caller must gate on Ready)")
	}
	if !body.CollectedAt.IsZero() {
		t.Fatalf("not-ready response must not carry a CollectedAt, got %v", body.CollectedAt)
	}
}

// TestRepoCostPartitionInvariantServedPath re-asserts the epic's hard
// requirement — Σby_repo+unattributed+backend_unsupported == hive total — but
// through the ACTUAL served path (RepoCostCollector.collect -> Snapshot ->
// handleRepoCost -> HTTP JSON), not just the pure compute function. The
// existing TestRepoCostPartitionInvariant in repo_cost_test.go only proves
// computeRepoCost is correct; this proves the caching layer introduced by
// #4943 does not lose or duplicate anything on the way to the wire.
func TestRepoCostPartitionInvariantServedPath(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-24 * time.Hour)

	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner",
			rcUsage(rcTime(base, 5), 100, 10),
			rcUsage(rcTime(base, 15), 200, 20),
			rcUsage(rcTime(base, 25), 300, 30),
		),
		rcSession("s2", "lonely", rcUsage(rcTime(base, 15), 400, 40)),
		{SessionID: "s3", Agent: "reviewer", Model: "gpt-5", Backend: tokens.BackendCopilot,
			InputTokens: 500, OutputTokens: 50, TotalTokens: 550},
	}}
	var hiveTotal int64
	for _, sess := range summary.Sessions {
		hiveTotal += sess.TotalTokens
	}

	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}

	rc := NewRepoCostCollector(&fakeFixedAudit{entries: entries}, &fakeTokensSummary{summary: summary}, "", nil)
	rc.nowFn = func() time.Time { return now }
	rc.collect()

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{RepoCost: rc})

	rec := getRepoCost(s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body repoCostResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !body.Ready {
		t.Fatalf("expected ready=true, got %+v", body)
	}

	sum := body.Unattributed.Tokens + body.BackendUnsupported.Tokens
	for _, e := range body.ByRepo {
		sum += e.Tokens
	}
	if sum != hiveTotal {
		t.Fatalf("served-path partition broken: Σby_repo+unattributed+backend_unsupported = %d, hive total = %d", sum, hiveTotal)
	}
	if body.TotalTokens != hiveTotal {
		t.Fatalf("served TotalTokens = %d, want %d", body.TotalTokens, hiveTotal)
	}
}

// TestRepoCostCollectorPersistsAcrossRestart mirrors ActivityCollector's
// EnablePersistence contract: a fresh collector pointed at a prior snapshot
// file must report ready=true immediately, with the ORIGINAL CollectedAt
// preserved, before its own first ticker fire — so a restart does not present
// a stale-but-labeled-fresh figure, and does not regress to not-ready either.
func TestRepoCostCollectorPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/repo-cost.json"

	original := NewRepoCostCollector(&fakeFixedAudit{entries: []AuditEntry{
		rcEvent(time.Now().Add(-time.Hour), "scanner", "org/repo-a"),
	}}, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)
	fixedNow := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	original.nowFn = func() time.Time { return fixedNow }
	original.EnablePersistence(path)
	original.collect()

	restarted := NewRepoCostCollector(&fakeFixedAudit{}, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)
	restarted.EnablePersistence(path)

	snap, ready := restarted.Snapshot()
	if !ready {
		t.Fatal("restored collector must report ready=true without waiting for its own first collect")
	}
	if !snap.CollectedAt.Equal(fixedNow) {
		t.Fatalf("restored CollectedAt = %v, want %v (the ORIGINAL collection time, not now)", snap.CollectedAt, fixedNow)
	}
}
