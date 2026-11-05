package github

import (
	"context"
	"net/http/httptest"
	"testing"
)

// Danathar/tududi#512 parked "Answer on #511; the same choice applies here".
// Linked "blocked by #511", its parked row carries the open-blocker count, so
// the dashboard can count one owner ask for the pair, not two.
func TestEnumerateParkedRowCarriesOpenBlockedBy(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 511, Title: "upstream: drop SQLite", User: wireUser{"hive-app[bot]"}, Labels: []wireLabel{{Name: "stage/045-needs-decision"}}, CreatedAt: hoursAgo(3)},
		{Number: 512, Title: "upstream: v2.0.0-rc.1", User: wireUser{"hive-app[bot]"}, Labels: []wireLabel{{Name: "stage/045-needs-decision"}}, CreatedAt: hoursAgo(2),
			IssueDependenciesSummary: &wireDepsSummary{BlockedBy: 1, TotalBlockedBy: 1}},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, []wirePR{}))
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
	if r := rows[512]; r.Bucket != workBucketFiltered || r.OpenBlockedBy != 1 {
		t.Fatalf("#512 row = %+v, want a filtered park with OpenBlockedBy=1", r)
	}
	if r := rows[511]; r.Bucket != workBucketFiltered || r.OpenBlockedBy != 0 {
		t.Fatalf("#511 row = %+v, want a filtered park with no blocker", r)
	}
}
