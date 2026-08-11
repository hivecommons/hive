package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- heartbeatBearerOK tests ---

func TestHeartbeatBearerOK_LegacyRawSecret(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	secret := srv.hubSecret

	if !srv.heartbeatBearerOK("Bearer "+secret, "") {
		t.Error("heartbeatBearerOK must accept raw master secret")
	}
}

func TestHeartbeatBearerOK_DerivedKey(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	derived := srv.heartbeatKey()
	if derived == "" {
		t.Fatal("heartbeatKey() returned empty — test cannot proceed")
	}

	if !srv.heartbeatBearerOK("Bearer "+derived, "") {
		t.Error("heartbeatBearerOK must accept derived heartbeat key")
	}
}

func TestHeartbeatBearerOK_InvalidTokenRejected(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	cases := []struct {
		name string
		auth string
	}{
		{"empty", ""},
		{"no bearer prefix", srv.hubSecret},
		{"wrong token", "Bearer totally-wrong-token"},
		{"basic auth", "Basic dXNlcjpwYXNz"},
		{"bearer prefix only", "Bearer "},
		{"bearer with space", "Bearer  " + srv.hubSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if srv.heartbeatBearerOK(tc.auth, "") {
				t.Errorf("heartbeatBearerOK(%q) = true, want false", tc.auth)
			}
		})
	}
}

func TestHeartbeatBearerOK_DerivedDiffersFromRaw(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	if srv.hubSecret == srv.heartbeatKey() {
		t.Error("derived heartbeat key must differ from raw master secret")
	}
}

// --- storePendingGitHubAppConfig / consumePendingGitHubAppConfig tests ---

func TestPendingGitHubAppConfig_StoreAndConsume(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	cfg := &HeartbeatGitHubAppConfig{AppID: 42, InstallationID: 99}

	srv.storePendingGitHubAppConfig("hive-1", cfg)

	got := srv.consumePendingGitHubAppConfig("hive-1")
	if got == nil {
		t.Fatal("consumePendingGitHubAppConfig returned nil after store")
	}
	if got.AppID != 42 || got.InstallationID != 99 {
		t.Errorf("got AppID=%d InstallationID=%d, want 42/99", got.AppID, got.InstallationID)
	}

	// Second consume must return nil (consumed)
	if got2 := srv.consumePendingGitHubAppConfig("hive-1"); got2 != nil {
		t.Error("second consumePendingGitHubAppConfig should return nil")
	}
}

func TestPendingGitHubAppConfig_ConsumeNonExistent(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	if got := srv.consumePendingGitHubAppConfig("no-such-hive"); got != nil {
		t.Errorf("consume on empty map returned %v, want nil", got)
	}
}

func TestPendingGitHubAppConfig_OverwritesPrevious(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.storePendingGitHubAppConfig("hive-1", &HeartbeatGitHubAppConfig{AppID: 1})
	srv.storePendingGitHubAppConfig("hive-1", &HeartbeatGitHubAppConfig{AppID: 2})

	got := srv.consumePendingGitHubAppConfig("hive-1")
	if got == nil || got.AppID != 2 {
		t.Errorf("store should overwrite: got AppID=%v, want 2", got)
	}
}

// --- handleLeaderboard tests ---

func TestHandleLeaderboard_EmptyRegistry(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/leaderboard", nil)
	w := httptest.NewRecorder()

	srv.handleLeaderboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	lb, ok := resp["leaderboard"]
	if !ok {
		t.Fatal("response missing 'leaderboard' key")
	}
	arr, ok := lb.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("expected empty leaderboard array, got %v", lb)
	}
}

func TestHandleLeaderboard_WithEntries(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{
			ID:       "hive-1",
			Name:     "Alpha",
			Online:   true,
			IsPublic: true,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "alice", HiveName: "Alpha", TasksCompleted: 5},
				{GitHubUsername: "bob", HiveName: "Alpha", TasksCompleted: 3},
			},
		},
		{
			ID:       "hive-2",
			Name:     "Beta",
			Online:   true,
			IsPublic: true,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "charlie", HiveName: "Beta", TasksCompleted: 2},
			},
		},
	}

	req := httptest.NewRequest("GET", "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.handleLeaderboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Leaderboard) != 3 {
		t.Errorf("expected 3 leaderboard entries (alice, bob, charlie), got %d", len(resp.Leaderboard))
	}
}

func TestHandleLeaderboard_PrivateHivesExcluded(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{
			ID:       "priv",
			Name:     "Private",
			Online:   true,
			IsPublic: false,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "secret-user", TasksCompleted: 99},
			},
		},
	}

	req := httptest.NewRequest("GET", "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.handleLeaderboard(w, req)

	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Leaderboard) != 0 {
		t.Errorf("private hive entries should not appear in leaderboard, got %d entries", len(resp.Leaderboard))
	}
}

// --- handleStats tests ---

func TestHandleStats_EmptyRegistry(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	srv.handleStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"hives", "online", "agents", "contributors", "issues", "prs"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("response missing key %q", key)
		}
	}
}

func TestHandleStats_OnlyCountsPublicHives(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "pub1", IsPublic: true, Online: true, AgentCount: 3, ActiveContributors: 2, ActionableIssues: 5, ActionablePRs: 4},
		{ID: "priv1", IsPublic: false, Online: true, AgentCount: 10, ActiveContributors: 10, ActionableIssues: 10, ActionablePRs: 10},
		{ID: "pub2", IsPublic: true, Online: false, AgentCount: 1, ActiveContributors: 1, ActionableIssues: 1, ActionablePRs: 1},
	}

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	srv.handleStats(w, req)

	var resp map[string]float64
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Only public hives counted
	if resp["hives"] != 2 {
		t.Errorf("hives = %v, want 2 (only public)", resp["hives"])
	}
	if resp["online"] != 1 {
		t.Errorf("online = %v, want 1 (only pub1 is online+public)", resp["online"])
	}
	if resp["agents"] != 4 {
		t.Errorf("agents = %v, want 4 (3+1 from public hives)", resp["agents"])
	}
	if resp["contributors"] != 3 {
		t.Errorf("contributors = %v, want 3 (2+1 from public hives)", resp["contributors"])
	}
	if resp["issues"] != 6 {
		t.Errorf("issues = %v, want 6 (5+1 from public hives)", resp["issues"])
	}
	if resp["prs"] != 5 {
		t.Errorf("prs = %v, want 5 (4+1 from public hives)", resp["prs"])
	}
}

// --- handleTaskStatus tests ---

func TestHandleTaskStatus_Unauthorized(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"hive-1","leaderboard":[],"contributors":{"registered":0,"active":0}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleTaskStatus_ValidPayload(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Name: "TestHive", Online: true},
	}

	body := `{"hive_id":"hive-1","leaderboard":[{"github_username":"alice"}],"contributors":{"registered":5,"active":3}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+srv.hubSecret)
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify contributors were updated
	srv.mu.RLock()
	h := srv.registry.Hives[0]
	srv.mu.RUnlock()
	if h.ContributorCount != 5 {
		t.Errorf("ContributorCount = %d, want 5", h.ContributorCount)
	}
	if h.ActiveContributors != 3 {
		t.Errorf("ActiveContributors = %d, want 3", h.ActiveContributors)
	}
}

func TestHandleTaskStatus_OfflineHiveRejected(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Name: "TestHive", Online: false},
	}

	body := `{"hive_id":"hive-1","leaderboard":[],"contributors":{"registered":1,"active":0}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+srv.hubSecret)
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for offline hive", w.Code)
	}
}

func TestHandleTaskStatus_InvalidJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+srv.hubSecret)
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleTaskStatus_EmptyHiveID(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"","leaderboard":[],"contributors":{}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+srv.hubSecret)
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty hive_id", w.Code)
	}
}

func TestHandleTaskStatus_AcceptsDerivedKey(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Name: "TestHive", Online: true},
	}

	body := `{"hive_id":"hive-1","leaderboard":[],"contributors":{"registered":1,"active":1}}`
	req := httptest.NewRequest("POST", "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+srv.heartbeatKey())
	w := httptest.NewRecorder()
	srv.handleTaskStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with derived key; body: %s", w.Code, w.Body.String())
	}
}

// --- regPath tests ---

func TestRegPath_ReturnsNonEmpty(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	p := srv.regPath()
	if p == "" {
		t.Error("regPath() returned empty string")
	}
	if !strings.HasSuffix(p, ".json") {
		t.Errorf("regPath() = %q, expected .json suffix", p)
	}
}
