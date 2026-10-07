package github

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestEnumerateActionable_WorkBreakdown pins classification at the existing
// enumeration choice points. It covers every current exclusion path and the
// positive controls that guard against broad dashboard-title heuristics.
func TestEnumerateActionable_WorkBreakdown(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 1, Title: "normal work", User: wireUser{"alice"}, Labels: []wireLabel{{Name: "approved"}}, CreatedAt: hoursAgo(7)},
		{Number: 2, Title: advisoryTitle, User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: advisoryLabelName}}, CreatedAt: hoursAgo(6)},
		{Number: 3, Title: "Dependency Dashboard", User: wireUser{"renovate[bot]"}, CreatedAt: hoursAgo(5)},
		{Number: 4, Title: "Dependency Dashboard is confusing", User: wireUser{"bob"}, Labels: []wireLabel{{Name: "approved"}}, CreatedAt: hoursAgo(4)},
		{Number: 5, Title: "held work", User: wireUser{"carol"}, Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(3)},
		{Number: 6, Title: "exempt work", User: wireUser{"dave"}, Labels: []wireLabel{{Name: "approved"}, {Name: "no-ai"}}, CreatedAt: hoursAgo(2)},
		{Number: 7, Title: "missing required label", User: wireUser{"erin"}, CreatedAt: hoursAgo(1)},
	}
	prs := []wirePR{
		{Number: 10, Title: "normal PR", User: wireUser{"alice"}, CreatedAt: hoursAgo(4)},
		{Number: 11, Title: "held PR", User: wireUser{"bob"}, Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(3)},
		{Number: 12, Title: "draft PR", User: wireUser{"carol"}, Draft: true, CreatedAt: hoursAgo(2)},
		{Number: 13, Title: "exempt PR", User: wireUser{"dave"}, Labels: []wireLabel{{Name: "no-ai"}}, CreatedAt: hoursAgo(1)},
	}

	server := httptest.NewServer(buildMux(t, org, repo, issues, prs))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetExemptLabels([]string{"no-ai"})
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"approved"}})

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}

	if got, want := result.TotalByRepo[repo], (RepoCounts{Issues: 7, PRs: 4}); got != want {
		t.Fatalf("raw totals = %+v, want %+v", got, want)
	}
	got := result.WorkBreakdownByRepo[repo]
	wantIssues := RepoIssueBreakdown{Actionable: 2, Hold: 1, HiveAdvisory: 1, DependencyDashboard: 1, Filtered: 2, Exempt: 1}
	wantPRs := RepoPRBreakdown{Actionable: 1, Hold: 1, Draft: 1, Filtered: 1}
	if got.Issues != wantIssues {
		t.Errorf("issue breakdown = %+v, want %+v", got.Issues, wantIssues)
	}
	if got.PRs != wantPRs {
		t.Errorf("PR breakdown = %+v, want %+v", got.PRs, wantPRs)
	}
	if got.Issues.Total() != result.TotalByRepo[repo].Issues {
		t.Errorf("issue breakdown total = %d, raw total = %d", got.Issues.Total(), result.TotalByRepo[repo].Issues)
	}
	if got.PRs.Total() != result.TotalByRepo[repo].PRs {
		t.Errorf("PR breakdown total = %d, raw total = %d", got.PRs.Total(), result.TotalByRepo[repo].PRs)
	}
	if result.Issues.Count != 2 || result.PRs.Count != 1 || result.Hold.Total != 2 {
		t.Errorf("existing queue semantics changed: issues=%d PRs=%d hold=%d", result.Issues.Count, result.PRs.Count, result.Hold.Total)
	}
	details := result.WorkDetailsByRepo[repo].Issues
	if len(details) != 4 {
		t.Fatalf("work details = %d, want 4 non-actionable issue rows: %+v", len(details), details)
	}
	gotDetails := map[int]RepoWorkIssue{}
	for _, detail := range details {
		gotDetails[detail.Number] = detail
	}
	for number, want := range map[int]struct {
		bucket string
		reason string
	}{
		2: {"hive_advisory", "Hive advisory report"},
		3: {"dependency_dashboard", "Dependency dashboard control panel"},
		6: {"filtered", "Exempt label"},
		7: {"filtered", "Project issue filter"},
	} {
		detail, ok := gotDetails[number]
		if !ok {
			t.Fatalf("missing work detail for issue #%d in %+v", number, details)
		}
		if detail.Bucket != want.bucket || detail.Reason != want.reason || detail.Title == "" || detail.URL == "" {
			t.Errorf("detail #%d = %+v, want bucket=%q reason=%q and display fields", number, detail, want.bucket, want.reason)
		}
	}
}

func TestStandingMetaIssueReason(t *testing.T) {
	cases := []struct {
		name, title, author string
		labels              []string
		want                string
	}{
		{"advisory title", advisoryTitle, "anyone", nil, "hive's own advisory report"},
		{"advisory label", "renamed report", "anyone", []string{"HIVE/Advisory"}, "hive's own advisory report"},
		{"renovate dashboard", "Dependency Dashboard", "renovate[bot]", nil, "bot dependency dashboard (control panel, not work)"},
		{"dependabot dashboard", "Dependency Dashboard", "dependabot[bot]", nil, "bot dependency dashboard (control panel, not work)"},
		{"human dashboard", "Dependency Dashboard", "alice", nil, ""},
		{"bot ordinary work", "Update lockfile parser", "renovate[bot]", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := standingMetaIssueReason(tc.title, tc.author, tc.labels); got != tc.want {
				t.Errorf("standingMetaIssueReason() = %q, want %q", got, tc.want)
			}
		})
	}
}
