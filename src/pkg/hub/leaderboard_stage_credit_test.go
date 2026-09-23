package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMergeLeaderboardsSumsStageCreditWithWeight: stage completions reported by
// several public hives are summed per contributor and weighted by
// LeaderboardStageCreditWeight into stage_credit; a contributor no hive
// reported stages for stays unknown (nil), and the ranking input
// (tasks_completed) is untouched.
func TestMergeLeaderboardsSumsStageCreditWithWeight(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Leaderboard: []LeaderboardEntry{
			{GitHubUsername: "user1", TasksCompleted: 3, StagesCompleted: intPtr(2)},
			{GitHubUsername: "legacy", TasksCompleted: 1},
			{GitHubUsername: "stages-only", StagesCompleted: intPtr(4)},
		}},
		{ID: "h2", IsPublic: true, Leaderboard: []LeaderboardEntry{
			{GitHubUsername: "user1", TasksCompleted: 1, StagesCompleted: intPtr(3)},
			{GitHubUsername: "legacy", TasksCompleted: 2},
		}},
		{ID: "private", IsPublic: false, Leaderboard: []LeaderboardEntry{
			{GitHubUsername: "user1", StagesCompleted: intPtr(100)},
		}},
	}
	srv.mu.Unlock()

	srv.mu.RLock()
	merged := srv.mergeLeaderboards()
	srv.mu.RUnlock()
	byUser := map[string]LeaderboardEntry{}
	for _, e := range merged {
		byUser[e.GitHubUsername] = e
	}

	u1 := byUser["user1"]
	if u1.StagesCompleted == nil || *u1.StagesCompleted != 5 {
		t.Fatalf("user1 stages_completed = %v, want 5 (public hives only)", u1.StagesCompleted)
	}
	if u1.StageCredit == nil || *u1.StageCredit != 5*LeaderboardStageCreditWeight {
		t.Fatalf("user1 stage_credit = %v, want %d", u1.StageCredit, 5*LeaderboardStageCreditWeight)
	}
	if u1.TasksCompleted != 4 {
		t.Fatalf("user1 tasks_completed = %d, want 4 (ranking formula unchanged)", u1.TasksCompleted)
	}
	legacy := byUser["legacy"]
	if legacy.StagesCompleted != nil || legacy.StageCredit != nil {
		t.Fatalf("legacy stages = %v credit = %v, want unknown (nil)", legacy.StagesCompleted, legacy.StageCredit)
	}
	so, ok := byUser["stages-only"]
	if !ok || so.StageCredit == nil || *so.StageCredit != 4*LeaderboardStageCreditWeight {
		t.Fatalf("stages-only entry = %+v (found %v), want kept with credit", so, ok)
	}

	raw, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stages_completed":5`) || !strings.Contains(string(raw), `"stage_credit":`) {
		t.Fatalf("merged json = %s", raw)
	}
}

// TestTaskStatusStagesAbsentIsUnknownNotZero: a v5 spoke's payload has no
// stages_completed; after the hub handler stores it the field is nil, a
// reported count survives, and a negative count is dropped to unknown.
func TestTaskStatusStagesAbsentIsUnknownNotZero(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.setHubSecret("secret")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: "task-hive", Online: true, Name: "org/repo", IsPublic: true}}
	srv.mu.Unlock()

	payload := `{"hive_id":"task-hive","leaderboard":[` +
		`{"github_username":"v5user","tasks_completed":3},` +
		`{"github_username":"v6user","tasks_completed":1,"stages_completed":2},` +
		`{"github_username":"baduser","tasks_completed":1,"stages_completed":-7}` +
		`],"contributors":{"registered":3,"active":0}}`
	req := httptest.NewRequest(http.MethodPost, "/api/task-status", strings.NewReader(payload))
	req.Header.Set("Authorization", heartbeatBearer("secret", "task-hive"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("task-status = %d (body %s)", w.Code, w.Body.String())
	}

	srv.mu.RLock()
	stored := srv.registry.Hives[0].Leaderboard
	srv.mu.RUnlock()
	byUser := map[string]LeaderboardEntry{}
	for _, e := range stored {
		byUser[e.GitHubUsername] = e
	}
	if e := byUser["v5user"]; e.StagesCompleted != nil {
		t.Fatalf("v5 entry stages = %d, want nil (unknown)", *e.StagesCompleted)
	}
	if e := byUser["v6user"]; e.StagesCompleted == nil || *e.StagesCompleted != 2 {
		t.Fatalf("v6 entry stages = %v, want 2", e.StagesCompleted)
	}
	if e := byUser["baduser"]; e.StagesCompleted != nil {
		t.Fatalf("negative stages = %d, want dropped to nil", *e.StagesCompleted)
	}

	lbReq := httptest.NewRequest(http.MethodGet, "/api/hub/leaderboard", nil)
	lbW := httptest.NewRecorder()
	srv.mux.ServeHTTP(lbW, lbReq)
	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(lbW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode leaderboard: %v body=%s", err, lbW.Body.String())
	}
	for _, e := range resp.Leaderboard {
		switch e.GitHubUsername {
		case "v5user":
			if e.StageCredit != nil {
				t.Fatalf("v5user stage_credit = %d, want absent", *e.StageCredit)
			}
		case "v6user":
			if e.StageCredit == nil || *e.StageCredit != 2*LeaderboardStageCreditWeight {
				t.Fatalf("v6user stage_credit = %v", e.StageCredit)
			}
		}
	}
}

// TestHeartbeatLeaderboardStagesRoundTripOptional mirrors the runs round trip:
// the field is omitted when unset and preserved when set.
func TestHeartbeatLeaderboardStagesRoundTripOptional(t *testing.T) {
	raw, err := json.Marshal(HeartbeatPayload{HiveID: "legacy", Leaderboard: []LeaderboardEntry{{GitHubUsername: "u"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stages_completed") {
		t.Fatalf("legacy payload carries stages_completed: %s", raw)
	}
	var legacy HeartbeatPayload
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Leaderboard[0].StagesCompleted != nil {
		t.Fatalf("legacy stages = %v, want nil", legacy.Leaderboard[0].StagesCompleted)
	}
	var got HeartbeatPayload
	if err := json.Unmarshal([]byte(`{"hive_id":"v6","leaderboard":[{"github_username":"u","stages_completed":2}]}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Leaderboard[0].StagesCompleted == nil || *got.Leaderboard[0].StagesCompleted != 2 {
		t.Fatalf("stages round trip = %v", got.Leaderboard[0].StagesCompleted)
	}
}

// TestHubLandingRendersStageCreditAsUnknownWhenAbsent is the render smoke: the
// landing page's leaderboard has a Stages column that reads stage_credit and
// falls back to "unknown", never a zero, when the field is missing.
func TestHubLandingRendersStageCreditAsUnknownWhenAbsent(t *testing.T) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, want := range []string{
		"function stageCreditText(e)",
		"e.stage_credit === null || e.stage_credit === undefined",
		">unknown</span>",
		`<th style="text-align:center">Stages</th>`,
		"stageCreditText(e)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("hub index.html lacks %q", want)
		}
	}
}
