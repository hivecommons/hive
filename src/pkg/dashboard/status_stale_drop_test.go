package dashboard

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// staleDropLoggedAt reports whether the stale-drop line was emitted at the
// given level. Unrelated WARN lines (e.g. the run-projection registry being
// absent in a bare test server) must not influence the assertions.
func staleDropLoggedAt(logs, level string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "dropping stale status snapshot") && strings.Contains(line, "level="+level) {
			return true
		}
	}
	return false
}

// TestUpdateStatusIfFresh_PublishLoopConvergesAfterMidBuildMutation drives
// the real publish loop the eval cycle runs — begin snapshot, build, publish,
// next cycle — with a state mutation landing mid-build. The racing build must
// be dropped (and counted, at Info), and the NEXT build, which captures the
// post-mutation epoch, must publish and advance StatusSeq. A guard that
// never converged would leave StatusSeq frozen at its pre-mutation value.
func TestUpdateStatusIfFresh_PublishLoopConvergesAfterMidBuildMutation(t *testing.T) {
	var logBuf bytes.Buffer
	srv := NewServer(0, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Cycle 1: a clean build/publish.
	epoch := srv.BeginStatusSnapshot()
	if !srv.UpdateStatusIfFresh(&StatusPayload{}, epoch) {
		t.Fatal("cycle 1: clean snapshot should publish")
	}
	if got := srv.currentStatusSeq(); got != 1 {
		t.Fatalf("cycle 1: statusSeq = %d, want 1", got)
	}

	// Cycle 2: a mutation lands after the build began.
	epoch = srv.BeginStatusSnapshot()
	srv.noteStatusMutation()
	if srv.UpdateStatusIfFresh(&StatusPayload{}, epoch) {
		t.Fatal("cycle 2: snapshot built before a mutation must be dropped")
	}
	if got := srv.currentStatusSeq(); got != 1 {
		t.Fatalf("cycle 2: dropped snapshot must not advance statusSeq, got %d", got)
	}
	if !strings.Contains(logBuf.String(), "dropping stale status snapshot") ||
		!strings.Contains(logBuf.String(), "staleDrops=1") {
		t.Fatalf("drop must be logged with the counter, got:\n%s", logBuf.String())
	}
	if staleDropLoggedAt(logBuf.String(), "WARN") {
		t.Fatalf("a single stale drop must not log at WARN, got:\n%s", logBuf.String())
	}

	// Cycle 3: the next build re-reads the epoch and converges.
	epoch = srv.BeginStatusSnapshot()
	payload := &StatusPayload{}
	if !srv.UpdateStatusIfFresh(payload, epoch) {
		t.Fatal("cycle 3: build started after the mutation must publish")
	}
	if got := srv.currentStatusSeq(); got != 2 {
		t.Fatalf("cycle 3: statusSeq = %d, want 2", got)
	}
	if payload.StatusStaleDrops != 1 {
		t.Fatalf("published payload must carry the drop counter, got %d", payload.StatusStaleDrops)
	}
}

// TestUpdateStatusIfFresh_StaleDropCounterAccumulates checks the counter is
// process-cumulative and travels on every subsequent publish, so an operator
// reading /api/status can tell "the guard dropped N snapshots" apart from
// "nothing is publishing".
func TestUpdateStatusIfFresh_StaleDropCounterAccumulates(t *testing.T) {
	srv := newTestServer()
	for i := 0; i < 3; i++ {
		epoch := srv.BeginStatusSnapshot()
		srv.noteStatusMutation()
		if srv.UpdateStatusIfFresh(&StatusPayload{}, epoch) {
			t.Fatalf("iteration %d: stale snapshot published", i)
		}
	}
	srv.statusMu.RLock()
	drops := srv.statusStaleDrops
	srv.statusMu.RUnlock()
	if drops != 3 {
		t.Fatalf("statusStaleDrops = %d, want 3", drops)
	}
	payload := &StatusPayload{}
	if !srv.UpdateStatusIfFresh(payload, srv.BeginStatusSnapshot()) {
		t.Fatal("fresh snapshot should publish")
	}
	if payload.StatusStaleDrops != 3 {
		t.Fatalf("payload.StatusStaleDrops = %d, want 3", payload.StatusStaleDrops)
	}
}

func TestUpdateStatusIfFresh_StarvationWarnsAfterThreshold(t *testing.T) {
	var logBuf bytes.Buffer
	srv := NewServer(0, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	for i := 0; i < staleDropStarvationThreshold-1; i++ {
		epoch := srv.BeginStatusSnapshot()
		srv.noteStatusMutation()
		srv.UpdateStatusIfFresh(&StatusPayload{}, epoch)
	}
	if staleDropLoggedAt(logBuf.String(), "WARN") {
		t.Fatalf("below the threshold must not WARN, got:\n%s", logBuf.String())
	}
	epoch := srv.BeginStatusSnapshot()
	srv.noteStatusMutation()
	srv.UpdateStatusIfFresh(&StatusPayload{}, epoch)
	if !staleDropLoggedAt(logBuf.String(), "WARN") {
		t.Fatalf("threshold-th consecutive drop must WARN, got:\n%s", logBuf.String())
	}
}
