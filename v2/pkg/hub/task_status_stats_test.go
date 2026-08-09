package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestHubServer creates a minimal HubServer for handler tests.
func newTestHubServer(secret string) *HubServer {
	return &HubServer{
		logger:    slog.Default(),
		hubSecret: secret,
		registry: Registry{
			Hives: []RegistryEntry{},
		},
	}
}

// --- handleTaskStatus tests ---

func TestHandleTaskStatus_Unauthorized(t *testing.T) {
	s := newTestHubServer("secret-abc")

	body := `{"hive_id":"h1","leaderboard":[],"contributors":{"active":1,"registered":5}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(body))
	// No Authorization header
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestHandleTaskStatus_WrongBearer(t *testing.T) {
	s := newTestHubServer("secret-abc")

	body := `{"hive_id":"h1","leaderboard":[],"contributors":{"active":1,"registered":5}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestHandleTaskStatus_NoSecretSkipsAuth(t *testing.T) {
	s := newTestHubServer("") // no hub secret → auth bypassed
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Name: "Hive One", Online: true},
	}

	body := `{"hive_id":"h1","leaderboard":[],"contributors":{"active":2,"registered":10}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleTaskStatus_InvalidJSON(t *testing.T) {
	s := newTestHubServer("secret-abc")

	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleTaskStatus_EmptyHiveID(t *testing.T) {
	s := newTestHubServer("secret-abc")

	body := `{"hive_id":"","leaderboard":[],"contributors":{"active":1,"registered":5}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleTaskStatus_HiveOffline(t *testing.T) {
	s := newTestHubServer("secret-abc")
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Name: "Hive One", Online: false},
	}

	body := `{"hive_id":"h1","leaderboard":[],"contributors":{"active":1,"registered":5}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "offline") {
		t.Fatalf("expected 'offline' in body, got: %s", rr.Body.String())
	}
}

func TestHandleTaskStatus_HiveNotFound(t *testing.T) {
	s := newTestHubServer("secret-abc")
	s.registry.Hives = []RegistryEntry{
		{ID: "other", Name: "Other", Online: true},
	}

	body := `{"hive_id":"h1","leaderboard":[],"contributors":{"active":1,"registered":5}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	// Unknown hive still returns OK (no match found, but no error either)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleTaskStatus_Success(t *testing.T) {
	s := newTestHubServer("secret-abc")
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Name: "Hive One", Online: true},
	}

	payload := map[string]any{
		"hive_id": "h1",
		"leaderboard": []map[string]any{
			{"github_username": "alice", "tasks_completed": 5},
			{"github_username": "bob", "tasks_completed": 3},
		},
		"contributors": map[string]any{"active": 2, "registered": 10},
	}
	data, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify registry was updated
	h := s.registry.Hives[0]
	if h.ActiveContributors != 2 {
		t.Errorf("expected ActiveContributors=2, got %d", h.ActiveContributors)
	}
	if h.ContributorCount != 10 {
		t.Errorf("expected ContributorCount=10, got %d", h.ContributorCount)
	}
	if len(h.Leaderboard) != 2 {
		t.Fatalf("expected 2 leaderboard entries, got %d", len(h.Leaderboard))
	}
	if h.Leaderboard[0].HiveName != "Hive One" {
		t.Errorf("expected HiveName='Hive One', got %q", h.Leaderboard[0].HiveName)
	}
	if h.Leaderboard[1].GitHubUsername != "bob" {
		t.Errorf("expected username 'bob', got %q", h.Leaderboard[1].GitHubUsername)
	}
}

func TestHandleTaskStatus_SanitizesLeaderboardUsername(t *testing.T) {
	s := newTestHubServer("secret-abc")
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Name: "Hive", Online: true},
	}

	// Inject a username with invalid characters that sanitizeField should strip
	payload := map[string]any{
		"hive_id": "h1",
		"leaderboard": []map[string]any{
			{"github_username": "alice<script>", "tasks_completed": 1},
		},
		"contributors": map[string]any{"active": 1, "registered": 1},
	}
	data, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rr := httptest.NewRecorder()
	s.handleTaskStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// sanitizeField removes disallowed chars
	username := s.registry.Hives[0].Leaderboard[0].GitHubUsername
	if strings.Contains(username, "<") || strings.Contains(username, ">") {
		t.Errorf("username should be sanitized, got %q", username)
	}
}

// --- handleStats tests ---

func TestHandleStats_EmptyRegistry(t *testing.T) {
	s := newTestHubServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rr := httptest.NewRecorder()
	s.handleStats(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["hives"] != 0 {
		t.Errorf("expected hives=0, got %d", resp["hives"])
	}
}

func TestHandleStats_OnlyPublicHivesCounted(t *testing.T) {
	s := newTestHubServer("")
	s.registry.Hives = []RegistryEntry{
		{ID: "pub1", IsPublic: true, Online: true, AgentCount: 3, ActiveContributors: 2, ActionableIssues: 10, ActionablePRs: 5},
		{ID: "priv1", IsPublic: false, Online: true, AgentCount: 7, ActiveContributors: 4, ActionableIssues: 20, ActionablePRs: 15},
		{ID: "pub2", IsPublic: true, Online: false, AgentCount: 1, ActiveContributors: 1, ActionableIssues: 3, ActionablePRs: 2},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rr := httptest.NewRecorder()
	s.handleStats(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Only public hives: pub1 + pub2
	if resp["hives"] != 2 {
		t.Errorf("expected hives=2, got %d", resp["hives"])
	}
	// Only pub1 is online
	if resp["online"] != 1 {
		t.Errorf("expected online=1, got %d", resp["online"])
	}
	// agents: 3 + 1 = 4
	if resp["agents"] != 4 {
		t.Errorf("expected agents=4, got %d", resp["agents"])
	}
	// contributors: 2 + 1 = 3
	if resp["contributors"] != 3 {
		t.Errorf("expected contributors=3, got %d", resp["contributors"])
	}
	// issues: 10 + 3 = 13
	if resp["issues"] != 13 {
		t.Errorf("expected issues=13, got %d", resp["issues"])
	}
	// prs: 5 + 2 = 7
	if resp["prs"] != 7 {
		t.Errorf("expected prs=7, got %d", resp["prs"])
	}
}

func TestHandleStats_ConcurrentAccess(t *testing.T) {
	s := newTestHubServer("")
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, AgentCount: 5},
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
			rr := httptest.NewRecorder()
			s.handleStats(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rr.Code)
			}
		}()
	}
	wg.Wait()
}
