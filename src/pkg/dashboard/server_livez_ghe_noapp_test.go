package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub"
)

// These tests frame the #2439 production scenario end to end at the probe level:
// a GHE-hosted spoke whose GitHub App is NOT yet installed. Its App
// installation-token mint fails/times out, so it can do no GitHub work and never
// reaches the hub — but its heartbeat loop is still beating (attempts advancing)
// and its HTTP server is serving. Such a pod must stay Ready and Live so the
// rolling upgrade completes and the hub stops showing "Upgrading"; it must NOT
// crash-loop (which a restart can never fix — the app is simply uninstalled).

// TestLivez_NoAppGHE_TokenMintFailing_StaysAlive is the core regression: token
// minting fails (so no beat ever reaches the hub → success is stale/never), but
// the loop keeps ATTEMPTING every tick. livez must be 200 — the bug was that a
// hung mint froze the attempt clock and misclassified this as a wedged loop,
// killing the pod.
func TestLivez_NoAppGHE_TokenMintFailing_StaysAlive(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()
	s.startedAt = time.Now().Add(-24 * time.Hour) // long past startup grace

	// Attempts advancing NOW (loop alive), but no beat has ever succeeded — the
	// GHE app is uninstalled so every collect/eval token mint soft-fails and the
	// hub is unreachable.
	hub.SetHeartbeatStateForTest(true, time.Now(), time.Time{})

	rec := getLivez(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-app GHE hive with a live (attempting) loop must stay alive: got %d, body %s",
			rec.Code, rec.Body.String())
	}
}

// TestLivez_NoAppGHE_WedgedLoopStillFails guards the other half: if the loop
// actually stops attempting (a genuinely wedged goroutine, not merely a failing
// external call), livez must STILL fail so kubelet restarts it. The fix must not
// weaken this.
func TestLivez_NoAppGHE_WedgedLoopStillFails(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()

	stalled := time.Now().Add(-livezHeartbeatStallMax - time.Minute)
	hub.SetHeartbeatStateForTest(true, stalled, time.Time{})

	if rec := getLivez(t, s); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a genuinely wedged loop must still fail liveness even on a no-app hive: got %d", rec.Code)
	}
}

// TestReadiness_ReachesReadyWithoutGitHubToken proves Part 2: readiness does not
// depend on a successful GitHub token mint. healthServer wires the dashboard
// deps but has NO GitHub App configured and mints no token; MarkReady alone must
// flip both probes to 200 so a no-app GHE hive can serve and complete a rollout.
func TestReadiness_ReachesReadyWithoutGitHubToken(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t) // no GitHub App, no token ever minted

	// A live heartbeat loop, no successes (can't reach hub / no token).
	hub.SetHeartbeatStateForTest(true, time.Now(), time.Time{})
	s.startedAt = time.Now()

	// Before MarkReady both gate closed.
	for _, path := range []string{"/api/health", "/api/livez"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s before MarkReady: want 503, got %d", path, rec.Code)
		}
	}

	// MarkReady with no GitHub token available at all must open both gates.
	s.MarkReady()
	for _, path := range []string{"/api/health", "/api/livez"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s after MarkReady (no GitHub token): want 200, got %d, body %s",
				path, rec.Code, rec.Body.String())
		}
	}
}
