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
		ClaimContext: &github.IssueClaimContext{PRNumber: 103, Reference: true},
		LinkedPRs:    []github.IssueLinkedPR{{Number: 103, State: "open", Reference: true}},
	}}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	issue := trackerItem(t, resp, "issue", 9)
	if issue.Stage != trackerNumberCoveredByPR || !issue.ClaimReference || !issue.IsTracker || issue.Children == nil || issue.Children.Completed != 2 || issue.Children.Total != 4 {
		t.Fatalf("tracker = %+v", issue)
	}
	items := []StageTrackerItem{
		{Repo: "acme/console", Kind: "issue", Number: 9, LinkedPR: 103},
		{Repo: "acme/console", Kind: "pr", Number: 103, BaseRef: "v6"},
	}
	stageTrackerResolveLinkedPRs(items)
	if len(items[0].LinkedPRs) != 1 || items[0].LinkedPRs[0].BaseRef != "v6" {
		t.Fatalf("covering PR base lost: %+v", items[0].LinkedPRs)
	}
}
