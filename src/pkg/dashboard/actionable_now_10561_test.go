package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestActionableNowSplitEqualsTotalWithSuppressedItems10561(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Dashboard: config.DashboardConfig{IssueBands: config.DashboardIssueBandsConfig{}}}}
	status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive", Issues: 4, PRs: 4,
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 1, Title: "ready"},
			github.Issue{Repo: "hivecommons/hive", Number: 2, Title: "needs human", Labels: []string{"needs-human"}},
			github.Issue{Repo: "hivecommons/hive", Number: 3, Title: "already done", Labels: []string{"hive/likely-done"}},
		},
		HeldIssues: []any{github.HoldItem{Repo: "hivecommons/hive", Number: 4, Title: "held", Type: "issue", Labels: []string{"hold"}}},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 10, Title: "open"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 11, Title: "blocked", Mergeable: github.MergeableNo}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 12, Title: "draft", Draft: true}},
		},
		HeldPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 13, Title: "held pr", Labels: []string{"hold"}}}},
	}}}

	// L5: the parked needs-human issue and the held PR (unknown hold source)
	// are your turn; the failing and draft PRs and the held issue are not
	// actionable; what is left is actionable.
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, qualityRoutedEnv11592(5))

	got := s.statusWithOverviewBands(status, now).ActionableNow
	if got.Issues != 2 || got.PRs != 1 || got.Total != 3 {
		t.Fatalf("actionableNow = %+v, want issues=2 prs=1 total=3", got)
	}
	if got.Issues+got.PRs != got.Total {
		t.Fatalf("split does not equal total: %+v", got)
	}
}

func TestOverviewAndGovernorBindSharedActionableNowField10561(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const serverActionable = window._lastStatus?.actionableNow;",
		"const sharedActionable = (data && data.actionableNow) || {};",
		"sharedActionable.issues",
		"sharedActionable.prs",
		"sharedActionable.issueEquation",
		"sharedActionable.prEquation",
		"const issueEquationHTML = issueEquationText ? renderActionableEquationSubline(issueEquation) : '';",
		"issueEquationHTML ? `<div class=\"overview-kpi-subline\">${issueEquationHTML}</div>` : ''",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard missing shared actionable binding %q", want)
		}
	}
}
