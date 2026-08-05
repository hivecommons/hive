package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCooldownCounts_ReflectsHubState seeds the hub's completedTasks (cooldown set)
// and live connections (in-flight set) and asserts CooldownCounts() reports the
// counts the fleet payload surfaces (#2649 companion). Expired-but-not-swept
// completions must NOT inflate the cooldown count.
func TestCooldownCounts_ReflectsHubState(t *testing.T) {
	hub, s := covK2Hub(t)

	enabled := true
	s.deps.Config.Hub.ContributeCooldownEnabled = &enabled
	const customHours = 24
	s.deps.Config.Hub.ContributeCooldownHours = customHours

	// Two issues STILL within the 24h window (completed recently).
	hub.markTaskCompleted("myorg/repo1", 1, "https://github.com/myorg/repo1/pull/1")
	hub.markTaskCompleted("myorg/repo1", 2, "https://github.com/myorg/repo1/pull/2")

	// One issue completed 25h ago — EXPIRED, must not count even though its entry is
	// still present in the map (not yet swept by isTaskInCooldown).
	hub.markTaskCompleted("myorg/repo1", 3, "https://github.com/myorg/repo1/pull/3")
	hub.completedMu.Lock()
	hub.completedTasks["myorg/repo1#3"] = time.Now().Add(-(customHours + 1) * time.Hour)
	hub.completedMu.Unlock()

	// Two distinct issues held in flight by live connections.
	hub.mu.Lock()
	hub.connections["c1"] = &ContributorConnection{
		currentTask: &WSTaskAssign{Repo: "myorg/repo1", Number: 100},
	}
	hub.connections["c2"] = &ContributorConnection{
		currentTask: &WSTaskAssign{Repo: "myorg/repo1", Number: 101},
	}
	hub.mu.Unlock()

	cooldown, inFlight := hub.CooldownCounts()
	if cooldown != 2 {
		t.Fatalf("cooldown count = %d, want 2 (two non-expired completions; the 25h-old one must not count)", cooldown)
	}
	if inFlight != 2 {
		t.Fatalf("in-flight count = %d, want 2 (two connections each holding a distinct issue)", inFlight)
	}
}

// TestCooldownCounts_DisabledYieldsZero verifies the operator kill-switch: with
// cooldown disabled, nothing is gated, so the cooldown count is 0 even though the
// completion history is recorded. In-flight is unaffected.
func TestCooldownCounts_DisabledYieldsZero(t *testing.T) {
	hub, s := covK2Hub(t)

	disabled := false
	s.deps.Config.Hub.ContributeCooldownEnabled = &disabled

	hub.markTaskCompleted("myorg/repo1", 10, "https://github.com/myorg/repo1/pull/10")

	hub.mu.Lock()
	hub.connections["c1"] = &ContributorConnection{
		currentTask: &WSTaskAssign{Repo: "myorg/repo1", Number: 200},
	}
	hub.mu.Unlock()

	cooldown, inFlight := hub.CooldownCounts()
	if cooldown != 0 {
		t.Fatalf("cooldown count = %d, want 0 when cooldown is disabled", cooldown)
	}
	if inFlight != 1 {
		t.Fatalf("in-flight count = %d, want 1", inFlight)
	}
}

// TestContributeFleet_IncludesCooldownCounts asserts the /api/contribute/fleet
// JSON payload carries the cooldown_count / in_flight_count fields reflecting the
// hub state, so the Operations tab can render them.
func TestContributeFleet_IncludesCooldownCounts(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t)) // also registers the contribute routes + hub
	hub := s.contributeHub

	enabled := true
	s.deps.Config.Hub.ContributeCooldownEnabled = &enabled

	hub.markTaskCompleted("myorg/repo1", 1, "https://github.com/myorg/repo1/pull/1")
	hub.mu.Lock()
	hub.connections["c1"] = &ContributorConnection{
		currentTask: &WSTaskAssign{Repo: "myorg/repo1", Number: 100},
	}
	hub.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		CooldownCount *int `json:"cooldown_count"`
		InFlightCount *int `json:"in_flight_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.CooldownCount == nil {
		t.Fatalf("fleet payload missing cooldown_count: %s", w.Body.String())
	}
	if resp.InFlightCount == nil {
		t.Fatalf("fleet payload missing in_flight_count: %s", w.Body.String())
	}
	// The seeded completion is in cooldown (default period) and one issue is in
	// flight, so both counts are 1.
	if *resp.CooldownCount != 1 {
		t.Fatalf("cooldown_count = %d, want 1", *resp.CooldownCount)
	}
	if *resp.InFlightCount != 1 {
		t.Fatalf("in_flight_count = %d, want 1", *resp.InFlightCount)
	}
}
