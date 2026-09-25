package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBoostActionableRepoPriorityStable(t *testing.T) {
	issues := []Issue{{Repo: "acme/web", Number: 1}, {Repo: "acme/api", Number: 2}, {Repo: "acme/api", Number: 3}, {Repo: "acme/ops", Number: 4}}
	BoostActionableRepoPriority(issues, "acme/api")
	got := []int{issues[0].Number, issues[1].Number, issues[2].Number, issues[3].Number}
	want := []int{2, 3, 1, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestBoostActionableRepoPriorityMatchesShortConfiguredRepo(t *testing.T) {
	issues := []Issue{{Repo: "web", Number: 1}, {Repo: "api", Number: 2}}
	BoostActionableRepoPriority(issues, "acme/api")
	if got := issues[0].Number; got != 2 {
		t.Fatalf("first issue = #%d, want swarmed short-name repo issue #2", got)
	}
}

func TestScoreSwarmCountsClosedIssuesMergedPRsAndParticipants(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		q := r.URL.Query().Get("q")
		queries = append(queries, q)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "is:issue") {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 4, "items": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"items":       []map[string]any{{"user": map[string]any{"login": "bob"}}, {"user": map[string]any{"login": "alice"}}, {"user": map[string]any{"login": "bob"}}},
		})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"api"})
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	score, err := c.ScoreSwarm(context.Background(), "acme/api", start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ScoreSwarm: %v", err)
	}
	if score.IssuesClosed != 4 || score.PRsMerged != 2 || len(score.Participants) != 2 || score.Participants[0] != "alice" || score.Participants[1] != "bob" {
		t.Fatalf("score = %+v", score)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "closed:") || !strings.Contains(queries[1], "merged:") {
		t.Fatalf("queries = %v", queries)
	}
}
