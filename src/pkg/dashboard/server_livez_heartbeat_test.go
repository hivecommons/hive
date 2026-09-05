package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hub "github.com/hivecommons/hive/pkg/hub/spoke"
)

// getLivez drives a ready server's /api/livez and returns the recorder.
func getLivez(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	return rec
}

// TestLivez_StaleHeartbeatSuccessStaysAlive is the regression test for the
// self-inflicted crash loop: a spoke that cannot reach the hub (firewalled
// cluster, hub outage) has an arbitrarily old last *success*, but its loop is
// still beating. Liveness must stay 200 — restarting the pod cannot fix a
// network partition, and failing here crash-loops a healthy process.
func TestLivez_StaleHeartbeatSuccessStaysAlive(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()

	// Loop is beating right now, but the hub last accepted a beat an hour ago.
	hub.SetHeartbeatStateForTest(true, time.Now(), time.Now().Add(-time.Hour))

	if rec := getLivez(t, s); rec.Code != http.StatusOK {
		t.Fatalf("stale heartbeat success must NOT fail liveness: got %d, body %s",
			rec.Code, rec.Body.String())
	}
}

// TestLivez_NeverSucceededButAttemptingStaysAlive covers a spoke that has
// never once reached the hub (misconfigured secret, hub down since boot) well
// past the startup grace. The loop is attempting, so the process is healthy
// and must not be restarted — the hub already greys its dot.
func TestLivez_NeverSucceededButAttemptingStaysAlive(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()
	s.startedAt = time.Now().Add(-24 * time.Hour) // long past startup grace

	hub.SetHeartbeatStateForTest(true, time.Now(), time.Time{})

	if rec := getLivez(t, s); rec.Code != http.StatusOK {
		t.Fatalf("never-successful but attempting must stay alive: got %d, body %s",
			rec.Code, rec.Body.String())
	}
}

// TestLivez_StalledAttemptsFail is the other half of the contract: when the
// loop stops attempting entirely it is genuinely wedged, and a restart is the
// only remedy. This is the condition /api/livez exists to catch.
func TestLivez_StalledAttemptsFail(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()

	stalled := time.Now().Add(-livezHeartbeatStallMax - time.Minute)
	// Successes are fresh-ish relative to attempts; only attempts matter here.
	hub.SetHeartbeatStateForTest(true, stalled, stalled)

	rec := getLivez(t, s)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stalled heartbeat loop must fail liveness: got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode livez body: %v", err)
	}
	if body["detail"] != "heartbeat loop stalled" {
		t.Fatalf("unexpected detail: %q", body["detail"])
	}
}

// TestLivez_NoAttemptWithinStartupGrace verifies a booting pod is healthy
// before the loop's first send (waitForReady legitimately delays it for
// minutes), and unhealthy once the grace has elapsed with no attempt at all.
func TestLivez_NoAttemptWithinStartupGrace(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()

	t.Run("within grace is alive", func(t *testing.T) {
		s := healthServer(t)
		s.MarkReady()
		s.startedAt = time.Now()
		hub.SetHeartbeatStateForTest(true, time.Time{}, time.Time{})

		if rec := getLivez(t, s); rec.Code != http.StatusOK {
			t.Fatalf("booting pod must pass liveness: got %d", rec.Code)
		}
	})

	t.Run("past grace fails", func(t *testing.T) {
		s := healthServer(t)
		s.MarkReady()
		s.startedAt = time.Now().Add(-livezStartupGrace - time.Minute)
		hub.SetHeartbeatStateForTest(true, time.Time{}, time.Time{})

		rec := getLivez(t, s)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("loop that never started must fail liveness: got %d", rec.Code)
		}
	})
}

// TestLivez_NoHubConfiguredIgnoresHeartbeat guards the hub-less case: such a
// hive never runs a loop, so it must never be gated on heartbeat state.
func TestLivez_NoHubConfiguredIgnoresHeartbeat(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()
	s.startedAt = time.Now().Add(-24 * time.Hour)

	// Disabled loop, no attempts, no successes — still alive.
	hub.SetHeartbeatStateForTest(false, time.Time{}, time.Time{})

	if rec := getLivez(t, s); rec.Code != http.StatusOK {
		t.Fatalf("hub-less hive must ignore heartbeat state: got %d", rec.Code)
	}
}

// TestHealthDeep_ReportsHeartbeatStaleness confirms the connectivity signal
// removed from liveness is still surfaced — as a warning, not a pod kill.
func TestHealthDeep_ReportsHeartbeatStaleness(t *testing.T) {
	defer hub.ResetHeartbeatStateForTest()
	s := healthServer(t)
	s.MarkReady()

	hub.SetHeartbeatStateForTest(true, time.Now(), time.Now().Add(-time.Hour))

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))

	var body struct {
		Checks map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health/deep: %v", err)
	}
	hb, ok := body.Checks["hub_heartbeat"].(map[string]any)
	if !ok {
		t.Fatalf("health/deep missing hub_heartbeat check: %s", rec.Body.String())
	}
	if hb["status"] != "warn" {
		t.Fatalf("stale heartbeat should warn in health/deep, got %v", hb["status"])
	}
}
