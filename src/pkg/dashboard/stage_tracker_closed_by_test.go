package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15232: a PR whose closing link names an issue that was
// closed before the tracker ever listed it (the issue is not in the
// snapshot at all) keeps the issue in its linked_issues with its closed
// state — the SPA draws it in the issue slot — and, when the merged ledger
// knows another PR merged under that issue, names it as the closer on both
// the link and closed_issue. A PR never reads as the closer of its own
// issue.
func TestStageTrackerClosedIssueNamesClosingPR(t *testing.T) {
	now := time.Date(2026, 10, 28, 21, 0, 0, 0, time.UTC)
	const issueNum, prNum, otherPR = 15173, 15191, 15181
	tests := []struct {
		name   string
		ledger []MergedLedgerEntry
		wantBy int
	}{
		{name: "closed by another merged PR", ledger: []MergedLedgerEntry{{Repo: "hivecommons/hive", Number: otherPR, Issue: issueNum, MergedAt: now.Add(-time.Hour)}}, wantBy: otherPR},
		{name: "closer unknown", wantBy: 0},
		{name: "ledger names only this PR", ledger: []MergedLedgerEntry{{Repo: "hivecommons/hive", Number: prNum, Issue: issueNum, MergedAt: now.Add(-time.Hour)}}, wantBy: 0},
		{name: "same issue number in another repo", ledger: []MergedLedgerEntry{{Repo: "hivecommons/other", Number: otherPR, Issue: issueNum, MergedAt: now.Add(-time.Hour)}}, wantBy: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer()
			store := stagelabels.NewStore("", func() time.Time { return now })
			s.stageStore = store
			store.Observe(stagelabels.Key("hivecommons/hive", prNum), stagelabels.ReadyForHumanMerge, "contributor-lane:workflow-path:human-approved@clubanderson")
			for _, e := range tt.ledger {
				s.mergedLedger().Record(e, now)
			}

			status := minimalPayload()
			status.HiveID = "hv-test"
			// The issue is already closed and absent from the snapshot: only
			// the PR's closing reference knows it.
			status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive",
				OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: prNum, Title: "Let hive decide on a ready-for-human-merge PR", Author: "clubanderson",
					Labels: []string{"stage/090-ready-for-human-merge"}, CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-time.Hour),
					LinkedIssues: []github.PRLinkedIssue{{Number: issueNum, Repo: "hivecommons/hive", Title: "Sweep merges approved PRs", State: "CLOSED", StateReason: "COMPLETED"}}}}}}}
			s.UpdateStatus(status)
			resp := s.buildStageTracker(status, store, now)

			pr := trackerItem(t, resp, "pr", prNum)
			if len(pr.LinkedIssues) != 1 || pr.LinkedIssues[0].Number != issueNum || pr.LinkedIssues[0].State != "closed" || pr.LinkedIssues[0].Title == "" {
				t.Fatalf("linked_issues = %+v, want the closed issue #%d with its title so the issue slot can name it", pr.LinkedIssues, issueNum)
			}
			if pr.LinkedIssues[0].ClosedBy != tt.wantBy {
				t.Fatalf("linked_issues[0].closed_by = %d, want %d", pr.LinkedIssues[0].ClosedBy, tt.wantBy)
			}
			if pr.ClosedIssue == nil || pr.ClosedIssue.Number != issueNum || pr.ClosedIssue.ClosedBy != tt.wantBy {
				t.Fatalf("closed_issue = %+v, want #%d closed by %d", pr.ClosedIssue, issueNum, tt.wantBy)
			}
		})
	}
}

func TestStageTrackerMarkClosedBy(t *testing.T) {
	closers := map[string]int{"acme/console#496": 500, "acme/console#7": 9}
	lookup := func(repo string, issue, exclude int) int {
		if n := closers[repo+"#"+itoa(issue)]; n != exclude {
			return n
		}
		return 0
	}
	items := []StageTrackerItem{
		// An issue row is never touched.
		{Kind: "issue", Repo: "acme/console", Number: 496},
		// Closed link, closed by another PR: named on the link and the anchor.
		{Kind: "pr", Repo: "acme/console", Number: 501, LinkedIssues: []StageTrackerLinkedIssue{{Number: 496, State: "closed"}}, ClosedIssue: &StageTrackerLinkedIssue{Number: 496, State: "closed"}},
		// The row's own PR is the recorded closer: left at 0.
		{Kind: "pr", Repo: "acme/console", Number: 500, LinkedIssues: []StageTrackerLinkedIssue{{Number: 496, State: "closed"}}},
		// An open link and an already-named closer are left alone.
		{Kind: "pr", Repo: "acme/console", Number: 502, LinkedIssues: []StageTrackerLinkedIssue{{Number: 7, State: "open"}, {Number: 496, State: "closed", ClosedBy: 42}}},
	}
	stageTrackerMarkClosedBy(items, lookup)
	if items[1].LinkedIssues[0].ClosedBy != 500 || items[1].ClosedIssue.ClosedBy != 500 {
		t.Fatalf("closed by another PR: link %+v anchor %+v", items[1].LinkedIssues[0], items[1].ClosedIssue)
	}
	if items[2].LinkedIssues[0].ClosedBy != 0 {
		t.Fatalf("a PR must not be its own issue's closer: %+v", items[2].LinkedIssues[0])
	}
	if items[3].LinkedIssues[0].ClosedBy != 0 || items[3].LinkedIssues[1].ClosedBy != 42 {
		t.Fatalf("open link / pre-named closer changed: %+v", items[3].LinkedIssues)
	}
	// A nil lookup is a no-op, not a panic.
	stageTrackerMarkClosedBy(items, nil)
}

func TestMergedLedgerClosedBy(t *testing.T) {
	l := newMergedLedger("")
	l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 500, Issue: 496, MergedAt: mergedTestNow.Add(-2 * time.Hour)}, mergedTestNow)
	l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 503, Issue: 496, MergedAt: mergedTestNow.Add(-time.Hour)}, mergedTestNow)
	l.Record(MergedLedgerEntry{Repo: "acme/console", Number: 504, MergedAt: mergedTestNow.Add(-time.Minute)}, mergedTestNow)
	if got := l.ClosedBy("ACME/Console", 496, 0); got != 503 {
		t.Fatalf("newest closer = %d, want 503", got)
	}
	if got := l.ClosedBy("acme/console", 496, 503); got != 500 {
		t.Fatalf("excluding the newest falls back to the earlier closer: %d, want 500", got)
	}
	if got := l.ClosedBy("acme/console", 497, 0); got != 0 {
		t.Fatalf("unknown issue = %d, want 0", got)
	}
	if got := l.ClosedBy("acme/other", 496, 0); got != 0 {
		t.Fatalf("other repo = %d, want 0", got)
	}
	var nilLedger *MergedLedger
	if got := nilLedger.ClosedBy("acme/console", 496, 0); got != 0 {
		t.Fatalf("nil ledger = %d, want 0", got)
	}
}
