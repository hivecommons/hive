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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 4,
				"items": []map[string]any{
					{"closed_by": map[string]any{"login": "alice"}},
					{"closed_by": map[string]any{"login": "alice"}},
					{"closed_by": map[string]any{"login": "carol"}},
				},
			})
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
	if score.PRsByAuthor["bob"] != 2 || score.PRsByAuthor["alice"] != 1 || score.IssuesClosedBy["alice"] != 2 || score.IssuesClosedBy["carol"] != 1 {
		t.Fatalf("attribution = prs %#v issues %#v", score.PRsByAuthor, score.IssuesClosedBy)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "closed:") || !strings.Contains(queries[1], "merged:") {
		t.Fatalf("queries = %v", queries)
	}
}

func TestCountUnlabeledOpenIssues(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		query = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 7, "items": []any{}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"api"})
	got, err := c.CountUnlabeledOpenIssues(context.Background(), "acme/api")
	if err != nil {
		t.Fatalf("CountUnlabeledOpenIssues: %v", err)
	}
	if got != 7 || !strings.Contains(query, "repo:acme/api") || !strings.Contains(query, "no:label") {
		t.Fatalf("got %d query %q", got, query)
	}
}
