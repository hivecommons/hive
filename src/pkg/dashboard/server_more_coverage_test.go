package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCovH2_IsPublicPath exercises every branch of the public-path allowlist.
func TestCovH2_IsPublicPath(t *testing.T) {
	publics := []string{
		"/api/health",
		"/api/health/deep",
		"/api/livez",
		"/api/auth/token",
		"/snapshot",
		"/snapshot/foo",
		"/api/snapshot/x",
		"/contribute",
		"/contribute/join",
		"/api/contribute/ws",
		"/leaderboard",
		"/leaderboard/top",
		"/api/leaderboard",
		"/api/gh-user-auth/start",
		openRouterCallbackPath,
		"/sso",
	}
	for _, p := range publics {
		if !isPublicPath(p) {
			t.Errorf("expected %q to be public", p)
		}
	}
	privates := []string{"/api/status", "/api/agents", "/", "/dashboard", "/api/config/github", "/api/gh-user-auth/logout"}
	for _, p := range privates {
		if isPublicPath(p) {
			t.Errorf("expected %q to NOT be public", p)
		}
	}
}

// TestCovH2_InferenceEndpoints covers Update/get/build inference endpoint paths.
func TestCovH2_InferenceEndpoints(t *testing.T) {
	s, _ := apiServer(t)

	// Register then read back.
	s.UpdateInferenceEndpoint("litellm", []string{"http://lite:4000"})
	if eps, ok := s.getInferenceEndpoints("litellm"); !ok || len(eps) != 1 {
		t.Fatalf("expected litellm endpoint registered, got %v %v", eps, ok)
	}

	// buildInferenceBackends should now include litellm (offline discovery → empty models).
	backends := s.buildInferenceBackends()
	var haveLite bool
	for _, b := range backends {
		if b.ID == "litellm" {
			haveLite = true
		}
	}
	if !haveLite {
		t.Fatalf("expected litellm in inference backends after registration")
	}

	// Empty list unregisters the backend (delete branch).
	s.UpdateInferenceEndpoint("litellm", nil)
	if _, ok := s.getInferenceEndpoints("litellm"); ok {
		t.Fatalf("expected litellm unregistered after empty update")
	}

	// SetInferenceEndpoints from a nil map, then update creates the map lazily.
	s.SetInferenceEndpoints(nil)
	s.UpdateInferenceEndpoint("vllm", []string{"http://vllm:8000"})
	if eps, ok := s.getInferenceEndpoints("vllm"); !ok || len(eps) != 1 {
		t.Fatalf("expected vllm endpoint after lazy-create, got %v %v", eps, ok)
	}
}

// TestCovH2_HealthDeepAndLivez drives the health/livez routes ready and not-ready.
func TestCovH2_HealthDeepAndLivez(t *testing.T) {
	s, _ := apiServer(t)

	// Not ready: health/deep reports degraded ready-check; livez 503.
	if rec := doGet(s, "/api/health/deep"); rec.Code != http.StatusOK {
		// handleHealthDeep always writes JSON with 200 (overall in body).
		t.Fatalf("health/deep (not ready) expected 200, got %d", rec.Code)
	}
	if rec := doGet(s, "/api/livez"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("livez (not ready) expected 503, got %d", rec.Code)
	}

	// Mark ready and re-drive — exercises the ready branches + agent/governor/config checks.
	s.MarkReady()
	if rec := doGet(s, "/api/health/deep"); rec.Code != http.StatusOK {
		t.Fatalf("health/deep (ready) expected 200, got %d", rec.Code)
	}
	if rec := doGet(s, "/api/livez"); rec.Code != http.StatusOK {
		t.Fatalf("livez (ready) expected 200, got %d", rec.Code)
	}
	if rec := doGet(s, "/api/health"); rec.Code != http.StatusOK {
		t.Fatalf("health (ready) expected 200, got %d", rec.Code)
	}
}

// TestCovH2_HealthSummary calls HealthSummary directly for both ready states.
func TestCovH2_HealthSummary(t *testing.T) {
	s, _ := apiServer(t)

	sum := s.HealthSummary()
	if sum == nil {
		t.Fatalf("HealthSummary returned nil")
	}
	if _, ok := sum["checks"]; !ok {
		if _, ok2 := sum["overall"]; !ok2 {
			t.Fatalf("HealthSummary missing expected keys: %v", sum)
		}
	}

	s.MarkReady()
	if sum := s.HealthSummary(); sum == nil {
		t.Fatalf("HealthSummary (ready) returned nil")
	}
}

// TestCovH2_UpdateStatusAndBroadcast exercises UpdateStatus and BroadcastAgentStatus.
func TestCovH2_UpdateStatusAndBroadcast(t *testing.T) {
	s, _ := apiServer(t)

	// UpdateStatus builds ACMM/inference/alerts/banner and stores the status.
	s.AddSystemAlert("a1", "warning", "test alert")
	s.SetHubBanner("b1", "hello", "blue")
	status := &StatusPayload{}
	s.UpdateStatus(status)
	if s.status == nil {
		t.Fatalf("expected status stored after UpdateStatus")
	}

	// BroadcastAgentStatus: right after a full broadcast it should skip (recentFull).
	s.BroadcastAgentStatus(&AgentStatusPayload{GovMode: "idle"})

	// Force lastFullBroadcast into the past so the broadcast path runs.
	s.statusMu.Lock()
	s.lastFullBroadcast = time.Now().Add(-time.Hour)
	s.statusMu.Unlock()
	s.BroadcastAgentStatus(&AgentStatusPayload{GovMode: "busy"})
}

// TestCovH2_HandleHealthReadyStates covers handleHealth via httptest directly.
func TestCovH2_HandleHealthReadyStates(t *testing.T) {
	s, _ := apiServer(t)

	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleHealth (not ready) expected 503, got %d", rec.Code)
	}
	s.MarkReady()
	rec = httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("handleHealth (ready) expected 200, got %d", rec.Code)
	}
}
