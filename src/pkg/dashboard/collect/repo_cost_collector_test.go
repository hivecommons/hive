package collect

// Collector-level RepoCostCollector tests, moved from pkg/dashboard with the
// collector (kubestellar/hive#5565 slice 2). The /api/repo-cost handler tests
// that exercise the served path stay in pkg/dashboard.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// fakeFixedAudit is an AuditReader that always returns the same entries,
// regardless of since/actions/filePath — good enough to drive the collector
// in a test without a real audit file on disk.
type fakeFixedAudit struct{ entries []AuditEntry }

func (f *fakeFixedAudit) OutputActionsSince(_ time.Time, _ map[string]bool, _ string) []AuditEntry {
	return f.entries
}

// fakeTokensSummary returns a fixed *tokens.AggregateSummary, so a test does
// not need a real *tokens.Collector backed by session files on disk.
type fakeTokensSummary struct {
	summary *tokens.AggregateSummary
}

func (f *fakeTokensSummary) Summary() *tokens.AggregateSummary { return f.summary }

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

// TestRepoCostCollector_StartCollectsAndStops asserts Start performs the
// up-front collect (Snapshot ready, CollectedAt set), keeps collecting on the
// ticker, and exits on ctx cancel.
func TestRepoCostCollector_StartCollectsAndStops(t *testing.T) {
	origInterval := repoCostCollectInterval
	repoCostCollectInterval = 5 * time.Millisecond
	t.Cleanup(func() { repoCostCollectInterval = origInterval })

	audit := &fakeFixedAudit{}
	rc := NewRepoCostCollector(audit, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)

	if !rc.CollectedAt().IsZero() {
		t.Fatalf("CollectedAt = %v before any collect, want zero", rc.CollectedAt())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rc.Start(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ready := rc.Snapshot(); ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Start never produced a ready snapshot")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if rc.CollectedAt().IsZero() {
		t.Error("CollectedAt still zero after a successful collect")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel — goroutine leak")
	}
}

// TestRepoCostCollector_StartInertWithoutInputs asserts the documented inert
// contract: Start returns immediately (rather than spinning a ticker) when
// either side of the audit/tokens join is missing, and on a nil receiver.
func TestRepoCostCollector_StartInertWithoutInputs(t *testing.T) {
	cases := map[string]*RepoCostCollector{
		"nil collector": nil,
		"nil audit":     NewRepoCostCollector(nil, &fakeTokensSummary{}, "", nil),
		"nil tokens":    NewRepoCostCollector(&fakeFixedAudit{}, nil, "", nil),
	}
	for name, rc := range cases {
		done := make(chan struct{})
		go func() {
			rc.Start(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Start did not return immediately", name)
		}
		if _, ready := rc.Snapshot(); ready {
			t.Errorf("%s: inert collector must never be ready", name)
		}
	}
}

// EnablePersistence restore paths mirror ActivityCollector's: corrupt JSON and
// a zero collected_at both "start fresh"; persist failures are swallowed.
func TestRepoCostCollector_PersistenceEdgeCases(t *testing.T) {
	dir := t.TempDir()

	// Corrupt sidecar → ignored, not ready.
	corrupt := dir + "/corrupt.json"
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := NewRepoCostCollector(&fakeFixedAudit{}, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)
	rc.EnablePersistence(corrupt)
	if _, ready := rc.Snapshot(); ready {
		t.Error("corrupt sidecar must not mark the collector ready")
	}

	// Zero collected_at → treated as empty, not ready.
	zero := dir + "/zero.json"
	if err := os.WriteFile(zero, []byte(`{"snapshot":{},"collected_at":"0001-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc2 := NewRepoCostCollector(&fakeFixedAudit{}, &fakeTokensSummary{summary: &tokens.AggregateSummary{}}, "", nil)
	rc2.EnablePersistence(zero)
	if _, ready := rc2.Snapshot(); ready {
		t.Error("zero collected_at must not mark the collector ready")
	}

	// No persist path configured → persistLocked is an early-return no-op.
	rc2.persistLocked()

	// Nil receiver → EnablePersistence is a no-op, never a panic.
	var nilRC *RepoCostCollector
	nilRC.EnablePersistence(dir + "/unused.json")
	if _, ready := nilRC.Snapshot(); ready {
		t.Error("nil collector must never be ready")
	}
	if !nilRC.CollectedAt().IsZero() {
		t.Error("nil collector CollectedAt must be zero")
	}
}
