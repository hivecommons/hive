package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func healthServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

func TestCovHealth_Livez_NotReady(t *testing.T) {
	s := healthServer(t)
	// Not ready → 503.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/livez", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("livez not-ready: want 503, got %d", rec.Code)
	}
}

func TestCovHealth_Livez_Ready(t *testing.T) {
	s := healthServer(t)
	s.MarkReady()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/livez", nil)
	s.mux.ServeHTTP(rec, req)
	// No heartbeat loop configured in tests → status ok.
	if rec.Code != http.StatusOK {
		t.Fatalf("livez ready: want 200, got %d", rec.Code)
	}
}

// TestReadinessIsAgentIndependent locks in the startup-ordering fix: the
// dashboard becomes Ready purely from MarkReady() with its deps wired, WITHOUT
// any agent having been launched. healthServer wires the API deps (testDeps)
// but starts zero agents — the exact state that exists right after the HTTP
// listener comes up and BEFORE the (now-backgrounded) staggered agent-launch
// loop in cmd/hive/main.go. Both /api/livez and /api/health must flip to 200 as
// soon as MarkReady() is called, proving the readiness gate does not depend on
// the agent fleet being up. This is what lets a pod become Ready in seconds
// instead of waiting out the multi-minute staggered launch.
func TestReadinessIsAgentIndependent(t *testing.T) {
	s := healthServer(t) // deps wired, NO agents started

	// Before MarkReady: readiness gate closed → 503 on both probes.
	for _, path := range []string{"/api/livez", "/api/health"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s before MarkReady: want 503, got %d", path, rec.Code)
		}
	}

	// MarkReady with no agents launched — the reorder makes this reachable in
	// seconds on the real startup path.
	s.MarkReady()

	// After MarkReady: both probes are 200 despite zero agents running.
	for _, path := range []string{"/api/livez", "/api/health"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s after MarkReady (no agents): want 200, got %d", path, rec.Code)
		}
	}
}

func TestCovHealth_HealthDeep_NotReady(t *testing.T) {
	s := healthServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/deep", nil)
	s.mux.ServeHTTP(rec, req)
	// Degraded (not ready, no GH auth) but the handler still responds 200 with a
	// JSON body describing checks.
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health deep: unexpected %d", rec.Code)
	}
}

func TestCovHealth_HealthDeep_Ready(t *testing.T) {
	s := healthServer(t)
	s.MarkReady()
	s.UpdateStatus(&StatusPayload{Agents: []FrontendAgent{{Name: "scanner"}}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/deep", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health deep ready: unexpected %d", rec.Code)
	}
}

func TestCovHealth_HealthAndStatus(t *testing.T) {
	s := healthServer(t)
	// Not ready → health 503; after MarkReady → 200 (both branches covered).
	if rec := doGet(s, "/api/health"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health not-ready: %d", rec.Code)
	}
	s.MarkReady()
	s.UpdateStatus(&StatusPayload{Agents: []FrontendAgent{{Name: "scanner"}}})
	if rec := doGet(s, "/api/health"); rec.Code != http.StatusOK {
		t.Fatalf("health ready: %d", rec.Code)
	}
	if rec := doGet(s, "/api/status"); rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
}
