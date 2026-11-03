package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func abandonedLeaderboardServer(t *testing.T) *HubServer {
	t.Helper()
	// Decode the wire payloads, including an older spoke without abandoned.
	payloads := []string{
		`{"leaderboard":[{"github_username":"alice","tasks_completed":5,"tasks_failed":1,"abandoned":2},{"github_username":"only-abandoned","abandoned":4}]}`,
		`{"leaderboard":[{"github_username":"alice","tasks_completed":3,"abandoned":3}]}`,
		`{"leaderboard":[{"github_username":"alice","tasks_completed":1},{"github_username":"legacy","tasks_completed":2}]}`,
	}
	s := newTestHubServer("")
	for _, raw := range payloads {
		var p HeartbeatPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		s.registry.Hives = append(s.registry.Hives, RegistryEntry{IsPublic: true, Leaderboard: p.Leaderboard})
	}
	s.registry.Hives = append(s.registry.Hives,
		RegistryEntry{IsPublic: false, Leaderboard: []LeaderboardEntry{{GitHubUsername: "alice", Abandoned: 100}}},
		RegistryEntry{IsPublic: true, Leaderboard: []LeaderboardEntry{
			{GitHubUsername: "agent", TrustTier: "agent", Abandoned: 100},
			{GitHubUsername: "", Abandoned: 100},
			{GitHubUsername: "empty"},
		}},
	)
	return s
}

func TestMergeLeaderboardsAbandoned(t *testing.T) {
	s := abandonedLeaderboardServer(t)
	rows := s.mergeLeaderboards()
	if len(rows) != 3 {
		t.Fatalf("merged rows = %+v, want alice, legacy, only-abandoned", rows)
	}
	want := map[string]int{"alice": 5, "legacy": 0, "only-abandoned": 4}
	for _, row := range rows {
		count, ok := want[row.GitHubUsername]
		if !ok || row.Abandoned != count {
			t.Errorf("unexpected merged row: %+v", row)
		}
		if row.GitHubUsername == "alice" && (row.TasksCompleted != 9 || row.TasksFailed != 1) {
			t.Errorf("existing counts changed: %+v", row)
		}
	}
}

func TestLeaderboardEndpointAbandonedJSON(t *testing.T) {
	s := abandonedLeaderboardServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hub/leaderboard", s.handleLeaderboard)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/hub/leaderboard", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Leaderboard []struct {
			Login     string `json:"github_username"`
			Abandoned *int   `json:"abandoned"`
		} `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Leaderboard) != 3 {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	want := map[string]int{"alice": 5, "legacy": 0, "only-abandoned": 4}
	for _, row := range response.Leaderboard {
		count, ok := want[row.Login]
		if !ok || row.Abandoned == nil || *row.Abandoned != count {
			t.Errorf("incorrect abandoned count for %q: %s", row.Login, w.Body.String())
		}
	}
}
