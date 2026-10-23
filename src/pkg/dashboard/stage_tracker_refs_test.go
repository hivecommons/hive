package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerRefsAndTrackerMetadata(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.statusMu.Lock()
	s.status.Repos[0].NonActionableIssues = []any{github.RepoWorkIssue{
		Repo: "acme/console", Number: 9, Title: "Tracker", Bucket: "claimed_by_pr",
		IsTracker: true, Children: &github.IssueChildren{Total: 4, Completed: 2},
		ClaimContext: &github.IssueClaimContext{PRNumber: 203, Reference: true},
		LinkedPRs:    []github.IssueLinkedPR{{Number: 203, State: "open", Reference: true}},
	}}
	s.status.Repos[0].ActionableIssues = append(s.status.Repos[0].ActionableIssues, github.Issue{
		Repo: "acme/console", Number: 10, Title: "Partial work", Labels: []string{" Hive/Partially-Addressed "},
	})
	s.status.Repos[0].NonActionablePrs = []any{github.RepoWorkPR{
		Repo: "acme/console", Number: 203, Title: "Draft coverage", Draft: true,
		Bucket: "draft", BaseRef: "v6",
	}}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	issue := trackerItem(t, resp, "issue", 9)
	if issue.Stage != trackerNumberCoveredByPR || !issue.ClaimReference || !issue.IsTracker || issue.Children == nil || issue.Children.Completed != 2 || issue.Children.Total != 4 {
		t.Fatalf("tracker = %+v", issue)
	}
	partial := trackerItem(t, resp, "issue", 10)
	if !partial.IsTracker || partial.Children != nil {
		t.Fatalf("partial label classification differs from work details: %+v", partial)
	}
	pr := trackerItem(t, resp, "pr", 203)
	if pr.BaseRef != "v6" {
		t.Fatalf("draft PR base lost: %+v", pr)
	}
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].BaseRef != "v6" {
		t.Fatalf("covering draft PR base lost: %+v", issue.LinkedPRs)
	}
}
