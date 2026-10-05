package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestOverviewTotalsUseForgeCountsAndExplainScannerGap10629(t *testing.T) {
	status := &StatusPayload{Repos: []FrontendRepo{
		{
			Name: "hive", Full: "hivecommons/hive",
			ActionableIssues: []any{github.Issue{Number: 1, Repo: "hivecommons/hive", Title: "human issue", AuthorIsHuman: true, CreatedAt: time.Now()}, github.Issue{Number: 2, Repo: "hivecommons/hive", Title: "bot issue", Author: "hive[bot]", CreatedAt: time.Now()}},
			HeldIssues:       []any{github.HoldItem{Number: 3, Repo: "hivecommons/hive", Type: "issue", Labels: []string{"hold"}}},
			OpenPrs:          []any{github.PullRequest{Number: 4, Repo: "hivecommons/hive", Title: "ready PR"}},
			HeldPrs:          []any{github.PullRequest{Number: 5, Repo: "hivecommons/hive", Title: "held PR", Labels: []string{"hold"}}},
			WorkBreakdown: &github.RepoWorkBreakdown{
				Issues: github.RepoIssueBreakdown{Actionable: 2, Hold: 1, Filtered: 2, NeedsDirection: 1, Exempt: 1},
				PRs:    github.RepoPRBreakdown{Actionable: 1, Hold: 1, Draft: 1},
			},
		},
		{
			Name: "docs", Full: "hivecommons/docs",
			ActionableIssues: []any{github.Issue{Number: 6, Repo: "hivecommons/docs", Title: "docs issue", AuthorIsHuman: true, CreatedAt: time.Now()}},
			OpenPrs:          []any{github.PullRequest{Number: 7, Repo: "hivecommons/docs", Title: "docs PR"}},
			WorkBreakdown: &github.RepoWorkBreakdown{
				Issues: github.RepoIssueBreakdown{Actionable: 1, Filtered: 1, NeedsDecision: 1},
				PRs:    github.RepoPRBreakdown{Actionable: 1},
			},
		},
	}}

	got := (&Server{}).statusWithOverviewBands(status, time.Now())
	if got.OverviewTotals.Issues.Forge != 7 || got.OverviewTotals.Issues.Tracked != 4 || got.OverviewTotals.Issues.Outside != 3 {
		t.Fatalf("issue totals = %+v, want forge=7 tracked=4 outside=3", got.OverviewTotals.Issues)
	}
	if got.OverviewTotals.Issues.Breakdown["needs_direction"] != 1 || got.OverviewTotals.Issues.Breakdown["needs_decision"] != 1 || got.OverviewTotals.Issues.Breakdown["exempt"] != 1 {
		t.Fatalf("issue breakdown = %+v, want needs-direction, needs-decision, and exempt gaps", got.OverviewTotals.Issues.Breakdown)
	}
	if got.OverviewTotals.PRs.Forge != 4 || got.OverviewTotals.PRs.Tracked != 3 || got.OverviewTotals.PRs.Outside != 1 || got.OverviewTotals.PRs.Breakdown["draft"] != 1 {
		t.Fatalf("PR totals = %+v, want multi-repo forge total with draft breakdown", got.OverviewTotals.PRs)
	}
}

func TestOverviewKPIBindsForgeTotalsAndTooltips10629(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"window._lastStatus?.overviewTotals?.issues",
		"window._lastStatus.overviewTotals",
		"const openIssues = Number(forgeTotals?.issues?.forge ?? trackedIssues);",
		"const openPRs = Number(forgeTotals?.prs?.forge ?? trackedPRs);",
		"All open GitHub issues (pull requests excluded) across the selected configured repos",
		"All open GitHub pull requests across the selected configured repos, including drafts",
		"overview-kpi-subline",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static/index.html missing %q", want)
		}
	}
}
