package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The Draft band read 0 hive-wide while projectbluefin/common#1532, #1626 and
// server#585 were open drafts: the enumeration sidelines drafts instead of
// listing them with the open PRs, and the bands only walked the open and
// held lists.
func TestOverviewDraftsLandInDraftBand(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	now := mustTime9102(t, "2026-11-05T21:00:00Z")
	open := github.PullRequest{Number: 7, BaseRef: "main", Title: "fix", Author: "someone", Labels: []string{}, Mergeable: github.MergeableYes, MergeableState: "clean", UpdatedAt: now, CreatedAt: now}
	status := &StatusPayload{HiveID: "h1", Repos: []FrontendRepo{{
		Full:    "projectbluefin/common",
		OpenPrs: []any{FrontendPR{PullRequest: open}},
		NonActionablePrs: []any{
			github.RepoWorkPR{Repo: "projectbluefin/common", Number: 1532, Title: "wip", Draft: true, Bucket: "draft", Reason: "Draft", CreatedAt: now, UpdatedAt: now},
			github.RepoWorkPR{Repo: "projectbluefin/common", Number: 1626, Title: "wip", Labels: []string{"approved"}, Draft: true, Bucket: "draft", Reason: "Draft", CreatedAt: now, UpdatedAt: now},
			github.RepoWorkPR{Repo: "projectbluefin/common", Number: 9, Title: "excluded", Bucket: "exempt", Reason: "Exempt label"},
		},
	}}}

	rows, specs := s.overviewPRRows(status, s.issueBandsConfig(), overviewFilters{}, now)
	counts := map[string]int{}
	for _, spec := range specs {
		counts[spec.Key] = spec.Count
	}
	if counts["draft"] != 2 {
		t.Fatalf("Draft count = %d, want 2; counts = %v", counts["draft"], counts)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the open PR and the two drafts", len(rows))
	}
	for _, item := range s.overviewPRItems(status, s.issueBandsConfig(), overviewFilters{}, now) {
		if item.pr.Number == 9 {
			t.Fatal("a sidelined non-draft PR is not a draft row")
		}
	}

	out := s.statusWithOverviewBands(status, now)
	if out.OverviewBands == nil {
		t.Fatal("status payload carries no overview bands")
	}
	for _, spec := range out.OverviewBands.PRs {
		if spec.Key == "draft" && spec.Count != 2 {
			t.Fatalf("status payload Draft band = %d, want 2 (%+v)", spec.Count, out.OverviewBands.PRs)
		}
	}
}
