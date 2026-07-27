package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func covK2ContribServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

// covK2SeedProfiles writes several contributor profiles into a temp dir and
// points HIVE_CONTRIBUTORS_DIR at it, so leaderboard/summary aggregation loops run.
func covK2SeedProfiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	profiles := []ContributorProfile{
		{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted", TasksCompleted: 12},
		{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "contributor", TasksCompleted: 5},
		{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "newcomer", TasksCompleted: 1},
	}
	for _, p := range profiles {
		data, _ := json.MarshalIndent(p, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, p.GitHubUsername+".json"), data, 0o644); err != nil {
			t.Fatalf("seed profile: %v", err)
		}
	}
	return dir
}

func TestCovK2_ContributorSummaryAndLeaderboard(t *testing.T) {
	covK2SeedProfiles(t)
	s := covK2ContribServer(t)

	registered, active := s.ContributorSummary()
	if registered != 3 {
		t.Fatalf("ContributorSummary registered = %d, want 3", registered)
	}
	_ = active

	entries := s.LeaderboardForHub()
	if len(entries) < 3 {
		t.Fatalf("expected >=3 leaderboard entries, got %d", len(entries))
	}
	// Ranks are assigned 1..N.
	for i, e := range entries {
		if e.Rank != i+1 {
			t.Fatalf("entry %d has rank %d", i, e.Rank)
		}
	}
}

func TestCovK2_LeaderboardRoutes(t *testing.T) {
	covK2SeedProfiles(t)
	s := covK2ContribServer(t)

	if rec := doGet(s, "/leaderboard"); rec.Code != http.StatusOK {
		t.Fatalf("leaderboard page: %d", rec.Code)
	}
	if rec := doGet(s, "/api/leaderboard"); rec.Code != http.StatusOK {
		t.Fatalf("leaderboard API: %d", rec.Code)
	}
}

func TestCovK2_ContributeLandingAndGet(t *testing.T) {
	covK2SeedProfiles(t)
	s := covK2ContribServer(t)

	if rec := doGet(s, "/contribute"); rec.Code != http.StatusOK {
		t.Fatalf("contribute landing: %d", rec.Code)
	}
	// Known contributor by id.
	if rec := doGet(s, "/api/contributors/c-alice"); rec.Code != http.StatusOK {
		t.Fatalf("contributor get: %d", rec.Code)
	}
}

func TestCovK2_TrustTierBadgeCSS(t *testing.T) {
	for _, tier := range []string{"newcomer", "contributor", "trusted", "advisor", "revoked", agentTierLabel, "unknown"} {
		bg, text, border := trustTierBadgeCSS(tier)
		if bg == "" || text == "" || border == "" {
			t.Fatalf("trustTierBadgeCSS(%q) returned empty component", tier)
		}
	}
	// trustTierColor also has a per-tier switch.
	for _, tier := range []string{"newcomer", "contributor", "trusted", "advisor", "revoked", "other"} {
		if trustTierColor(tier) == "" {
			t.Fatalf("trustTierColor(%q) empty", tier)
		}
	}
	// rankDisplay medal + numeric branches.
	for _, r := range []int{1, 2, 3, 4} {
		if rankDisplay(r) == "" {
			t.Fatalf("rankDisplay(%d) empty", r)
		}
	}
}

func TestCovK2_ValidateGitHubToken(t *testing.T) {
	// Empty token → "".
	if got := validateGitHubToken("", ""); got != "" {
		t.Fatalf("empty token should yield empty username")
	}
	// A bogus token hits api.github.com/user offline → non-200/err → "".
	if got := validateGitHubToken("ghp_bogus", ""); got != "" {
		t.Fatalf("bogus token offline should yield empty username, got %q", got)
	}
}

func TestCovK2_HandleAPIv1(t *testing.T) {
	s := covK2ContribServer(t)

	// No auth header → 401.
	if rec := doGet(s, "/api/v1/status"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("apiv1 no-auth: expected 401, got %d", rec.Code)
	}

	// Bearer with a bogus token (validated offline → "") → 401. Exercises the
	// Bearer-prefix parsing branch.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer ghp_bogus")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("apiv1 bearer bogus: expected 401, got %d", rec.Code)
	}

	// "token " prefix parsing branch.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "token ghp_bogus")
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("apiv1 token-prefix: expected 401, got %d", rec.Code)
	}
}

// --- Federation registry hives handlers ---

func TestCovK2_HivesHandlers(t *testing.T) {
	regPath := filepath.Join(t.TempDir(), "fed.json")
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", regPath)
	s := covK2ContribServer(t)

	// List (empty).
	if rec := doGet(s, "/api/hives"); rec.Code != http.StatusOK {
		t.Fatalf("hives list: %d", rec.Code)
	}

	// Register a hive with a public URL (registers or reports private/valid).
	rec := doPost(s, "/api/hives/register", map[string]any{
		"project_name": "proj", "org": "org", "hub_url": "https://hub.example.com",
	})
	if rec.Code >= 500 {
		t.Fatalf("hives register unexpected 5xx: %d", rec.Code)
	}

	// Onboard: missing fields → 400; valid → 200.
	if rec := doPost(s, "/api/hives/onboard", map[string]any{"project_name": "p"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("onboard missing fields: expected 400, got %d", rec.Code)
	}
	if rec := doPost(s, "/api/hives/onboard", map[string]any{"project_name": "p", "org": "o", "repos": []string{"r"}}); rec.Code != http.StatusOK {
		t.Fatalf("onboard valid: expected 200, got %d", rec.Code)
	}

	// Heartbeat + delete for an unknown id → 404.
	if rec := doPost(s, "/api/hives/hive-nope/heartbeat", map[string]any{"active_agents": 1}); rec.Code != http.StatusNotFound {
		t.Fatalf("heartbeat unknown: expected 404, got %d", rec.Code)
	}
	if rec := doDelete(s, "/api/hives/hive-nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: expected 404, got %d", rec.Code)
	}
}

func TestCovK2_HivesRegisterThenHeartbeatDelete(t *testing.T) {
	regPath := filepath.Join(t.TempDir(), "fed.json")
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", regPath)
	s := covK2ContribServer(t)

	// Seed a registry with a known hive directly.
	reg := &FederationRegistry{Hives: []FederationHive{
		{ID: "hive-org-proj", ProjectName: "proj", Org: "org", HubURL: "https://h"},
	}}
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// Heartbeat a known hive → 200 (exercises the found + field-update path).
	if rec := doPost(s, "/api/hives/hive-org-proj/heartbeat", map[string]any{
		"active_contributors": 2, "active_agents": 3, "actionable_items": 5,
	}); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat known: expected 200, got %d", rec.Code)
	}

	// Delete a known hive → 200.
	if rec := doDelete(s, "/api/hives/hive-org-proj", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete known: expected 200, got %d", rec.Code)
	}
}
