package dashboard

// Tests for four previously-untested API handlers (0% coverage):
// handleBreakerState (GET /api/breaker), handleGovernorWatchdog
// (PUT /api/config/governor/watchdog), handleProvidersHeadroom
// (GET /api/providers/headroom), and handleRepoCost (GET /api/repo-cost).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- handleBreakerState ---

func TestHandleBreakerStateUnavailableWithoutManager(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/breaker", nil)
	w := httptest.NewRecorder()

	s.handleBreakerState(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil-deps breaker state status = %d, want 503", w.Code)
	}
}

func TestHandleBreakerStateReportsDisengagedFleet(t *testing.T) {
	s := newFullServer(t)

	rec := doOwnerGet(s, "/api/breaker")

	if rec.Code != http.StatusOK {
		t.Fatalf("breaker state status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		OK      bool     `json:"ok"`
		Engaged bool     `json:"engaged"`
		Agents  []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !body.OK || body.Engaged {
		t.Fatalf("fresh fleet must be ok and disengaged, got %+v", body)
	}
	if len(body.Agents) != 0 {
		t.Fatalf("disengaged breaker must hold no agents, got %v", body.Agents)
	}
}

func TestHandleBreakerStateReflectsEngagedBreaker(t *testing.T) {
	s := newFullServer(t)
	s.deps.AgentMgr.EngageBreaker()

	rec := doOwnerGet(s, "/api/breaker")

	if rec.Code != http.StatusOK {
		t.Fatalf("breaker state status = %d, want 200", rec.Code)
	}
	var body struct {
		Engaged bool `json:"engaged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !body.Engaged {
		t.Fatalf("state must report the breaker engaged, got %q", rec.Body.String())
	}
}

// --- handleGovernorWatchdog ---

func putGovernorWatchdog(s *Server, body string, owner bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/watchdog", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if owner {
		markOwnerRequest(req)
	} else {
		req.Header.Set("X-Hive-Role", "read-write")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleGovernorWatchdogRejectsNonOwner(t *testing.T) {
	s := newFullServer(t)

	rec := putGovernorWatchdog(s, `{"mode":"heal"}`, false)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner watchdog update status = %d, want 403", rec.Code)
	}
	if got := s.deps.Config.Governor.Watchdog.Mode; got != "" {
		t.Fatalf("rejected update must not mutate config, mode = %q", got)
	}
}

func TestHandleGovernorWatchdogRejectsMalformedBody(t *testing.T) {
	s := newFullServer(t)

	rec := putGovernorWatchdog(s, `{not json`, true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", rec.Code)
	}
}

func TestHandleGovernorWatchdogValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown mode", `{"mode":"turbo"}`},
		{"probe interval below floor", `{"probeIntervalS":5}`},
		{"probe interval above cap", `{"probeIntervalS":100000}`},
		{"crash loop above cap", `{"crashLoopAfter":500}`},
		{"unparseable healthy reset", `{"healthyReset":"soon"}`},
		{"healthy reset below floor", `{"healthyReset":"5s"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFullServer(t)

			rec := putGovernorWatchdog(s, tc.body, true)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleGovernorWatchdogPersistsValidUpdate(t *testing.T) {
	s := newFullServer(t)
	enabled := true
	// A pre-existing legacy Enabled flag must be cleared once Mode is set here,
	// so the deprecated switch stops overriding the operator's choice on reload.
	s.deps.Config.Governor.Watchdog.Enabled = &enabled //nolint:staticcheck // SA1019: seeding the deprecated field is the test setup for verifying it gets cleared.

	rec := putGovernorWatchdog(s,
		`{"mode":"heal","probeIntervalS":60,"crashLoopAfter":3,"healthyReset":"30m","authProbe":false}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("valid update status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	wd := s.deps.Config.Governor.Watchdog
	if wd.Mode != "heal" {
		t.Errorf("mode = %q, want heal", wd.Mode)
	}
	if wd.Enabled != nil { //nolint:staticcheck // SA1019: asserting the deprecated field was cleared is the point of this test.
		t.Errorf("legacy Enabled flag must be cleared when mode is set, got %v", *wd.Enabled) //nolint:staticcheck // SA1019: see comment above.
	}
	if wd.ProbeIntervalS != 60 {
		t.Errorf("probe interval = %d, want 60", wd.ProbeIntervalS)
	}
	if wd.Restart.CrashLoopAfter != 3 {
		t.Errorf("crash loop after = %d, want 3", wd.Restart.CrashLoopAfter)
	}
	if wd.Restart.HealthyReset != "30m" {
		t.Errorf("healthy reset = %q, want 30m", wd.Restart.HealthyReset)
	}
	if wd.AuthProbe == nil || *wd.AuthProbe {
		t.Errorf("auth probe = %v, want explicit false", wd.AuthProbe)
	}
}

func TestHandleGovernorWatchdogPartialUpdateLeavesOtherFieldsAlone(t *testing.T) {
	s := newFullServer(t)
	s.deps.Config.Governor.Watchdog.ProbeIntervalS = 120

	rec := putGovernorWatchdog(s, `{"crashLoopAfter":5}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("partial update status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	wd := s.deps.Config.Governor.Watchdog
	if wd.ProbeIntervalS != 120 {
		t.Errorf("untouched probe interval = %d, want 120", wd.ProbeIntervalS)
	}
	if wd.Restart.CrashLoopAfter != 5 {
		t.Errorf("crash loop after = %d, want 5", wd.Restart.CrashLoopAfter)
	}
	if wd.Mode != "" {
		t.Errorf("mode must stay unset on a partial update, got %q", wd.Mode)
	}
}

// --- handleProvidersHeadroom ---

func TestHandleProvidersHeadroomRejectsNonOwner(t *testing.T) {
	s := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/providers/headroom", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	rec := httptest.NewRecorder()

	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner headroom status = %d, want 403", rec.Code)
	}
}

func TestHandleProvidersHeadroomDisabledWithoutRotationManager(t *testing.T) {
	s := newFullServer(t) // RotationMgr is nil here

	rec := doOwnerGet(s, "/api/providers/headroom")

	if rec.Code != http.StatusOK {
		t.Fatalf("headroom status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Enabled   bool          `json:"enabled"`
		Providers []interface{} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body.Enabled {
		t.Fatalf("nil rotation manager must report enabled=false, got %q", rec.Body.String())
	}
	if len(body.Providers) != 0 {
		t.Fatalf("nil rotation manager must report no providers, got %v", body.Providers)
	}
}

// --- handleRepoCost ---

func TestHandleRepoCostServesJSONWithoutTokenTracker(t *testing.T) {
	s := newFullServer(t) // Tokens dep is nil: the summary must degrade, not panic

	rec := doOwnerGet(s, "/api/repo-cost")

	if rec.Code != http.StatusOK {
		t.Fatalf("repo-cost status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("repo-cost must return valid JSON: %v", err)
	}
}

func TestHandleRepoCostSurvivesEmptyServer(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/repo-cost", nil)
	rec := httptest.NewRecorder()

	s.handleRepoCost(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("repo-cost with no deps status = %d, want 200", rec.Code)
	}
}

func TestAuditPathForActivityEmptyWithoutCollector(t *testing.T) {
	if got := (&Server{}).auditPathForActivity(); got != "" {
		t.Fatalf("no activity collector must mean default audit path, got %q", got)
	}
	s := newFullServer(t) // deps set, Activity nil
	if got := s.auditPathForActivity(); got != "" {
		t.Fatalf("nil Activity dep must mean default audit path, got %q", got)
	}
}
