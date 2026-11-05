package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Danathar/tududi#512 parked "Answer on #511; the same choice applies here".
// Linked "blocked by #511", its parked row carries the open-blocker count
// and, being a decision park, which issues block it, so the dashboard can
// tell whether it waits on another decision. A park that is not a decision
// costs no blocker read; an unreadable list leaves the row its own ask.
func TestEnumerateParkedRowCarriesOpenBlockers(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	park := []wireLabel{{Name: "stage/045-needs-decision"}}
	issues := []wireIssue{
		{Number: 511, Title: "upstream: drop SQLite", User: wireUser{"hive-app[bot]"}, Labels: park, CreatedAt: hoursAgo(3)},
		{Number: 512, Title: "upstream: v2.0.0-rc.1", User: wireUser{"hive-app[bot]"}, Labels: park, CreatedAt: hoursAgo(2), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 513, Title: "a person's step", User: wireUser{"hive-app[bot]"}, Labels: []wireLabel{{Name: "needs-human"}}, CreatedAt: hoursAgo(2), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 514, Title: "unreadable", User: wireUser{"hive-app[bot]"}, Labels: park, CreatedAt: hoursAgo(2), IssueDependenciesSummary: blockedSummary(1)},
	}
	fetched := map[int]int{}
	mux := buildMux(t, org, repo, issues, []wirePR{})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var num int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo)), "%d/", &num)
		fetched[num]++
		if num == 514 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, []wireBlocker{{Number: 511, State: "open", RepositoryURL: "https://api.github.com/repos/testorg/testrepo"}}))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	rows := map[int]RepoWorkIssue{}
	for _, row := range result.WorkDetailsByRepo[repo].Issues {
		rows[row.Number] = row
	}
	if r := rows[512]; r.Bucket != workBucketFiltered || r.OpenBlockedBy != 1 || fmt.Sprint(r.OpenBlockers) != "[testrepo#511]" {
		t.Fatalf("#512 row = %+v, want a filtered park blocked by testrepo#511", r)
	}
	if r := rows[511]; r.Bucket != workBucketFiltered || r.OpenBlockedBy != 0 || len(r.OpenBlockers) != 0 {
		t.Fatalf("#511 row = %+v, want a filtered park with no blocker", r)
	}
	if r := rows[513]; r.OpenBlockedBy != 1 || len(r.OpenBlockers) != 0 || fetched[513] != 0 {
		t.Fatalf("#513 row = %+v fetched=%d, want the count but no blocker read for a needs-human park", r, fetched[513])
	}
	if r := rows[514]; r.OpenBlockedBy != 1 || len(r.OpenBlockers) != 0 {
		t.Fatalf("#514 row = %+v, want no blockers when the list is unreadable", r)
	}
}
