package dashboard

import (
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The #19985 case: the contributor was dispatched #19985, reported blocked,
// and the relay scraped the URL of their own older PR #19984 (for #19979).
// That PR must not count as #19985's work.
func TestUnrelatedReportedPR_OlderPRForAnotherIssueIsDropped(t *testing.T) {
	assigned := time.Date(2026, 11, 26, 4, 13, 47, 0, time.UTC)
	task := &WSTaskAssign{Kind: "issue", Repo: "hivecommons/hive", Number: 19985}
	pr := ghpkg.PRVerification{
		Verified:  true,
		Title:     "feat: chat config assist phase 1",
		Body:      "Refs #19979\n\nPhase 1 of RFC #19788. Not #199850 either.",
		HeadRef:   "feat/chat-config-assist-phase-1",
		CreatedAt: assigned.Add(-6 * time.Hour),
	}
	if !unrelatedReportedPR(pr, task, assigned) {
		t.Fatal("an older PR that never names the issue must be unrelated")
	}
	hub := &ContributeWSHub{logger: covBLogger()}
	got := hub.dropUnrelatedReportedPR(task, pr, "https://github.com/hivecommons/hive/pull/19984", "hanthor", assigned)
	if got.Verified {
		t.Fatal("dropUnrelatedReportedPR must downgrade the PR to unverified")
	}
	if got.Reason != reportedPRUnrelatedReason {
		t.Fatalf("reason = %q, want %q", got.Reason, reportedPRUnrelatedReason)
	}
	// A nil hub (no logger) still downgrades.
	var nilHub *ContributeWSHub
	if nilHub.dropUnrelatedReportedPR(task, pr, "u", "hanthor", assigned).Verified {
		t.Fatal("nil hub must still downgrade an unrelated PR")
	}
}

func TestUnrelatedReportedPR_KeepsRelatedOrUnknown(t *testing.T) {
	assigned := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	older := assigned.Add(-time.Hour)
	task := &WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 42}
	base := ghpkg.PRVerification{Verified: true, CreatedAt: older}

	cases := []struct {
		name       string
		pr         func() ghpkg.PRVerification
		task       *WSTaskAssign
		assignedAt time.Time
	}{
		{"opened during the task", func() ghpkg.PRVerification { p := base; p.CreatedAt = assigned.Add(time.Minute); return p }, task, assigned},
		{"opened within clock skew", func() ghpkg.PRVerification {
			p := base
			p.CreatedAt = assigned.Add(-reportedPRAssignmentSkew / 2)
			return p
		}, task, assigned},
		{"older PR whose body fixes the issue", func() ghpkg.PRVerification { p := base; p.Body = "Fixes #42"; return p }, task, assigned},
		{"older PR whose title names the issue", func() ghpkg.PRVerification { p := base; p.Title = "fix: thing (#42)"; return p }, task, assigned},
		{"older PR linking the issue URL", func() ghpkg.PRVerification {
			p := base
			p.Body = "see https://github.com/o/r/issues/42"
			return p
		}, task, assigned},
		{"older PR on the issue's branch", func() ghpkg.PRVerification { p := base; p.HeadRef = "scanner/fix-42"; return p }, task, assigned},
		{"not verified", func() ghpkg.PRVerification { p := base; p.Verified = false; return p }, task, assigned},
		{"adopted task, no assignment time", func() ghpkg.PRVerification { return base }, task, time.Time{}},
		{"no creation time", func() ghpkg.PRVerification { p := base; p.CreatedAt = time.Time{}; return p }, task, assigned},
		{"nil task", func() ghpkg.PRVerification { return base }, nil, assigned},
		{"not an issue task", func() ghpkg.PRVerification { return base }, &WSTaskAssign{Kind: "fix-pr", Repo: "o/r", Number: 42}, assigned},
		{"no issue number", func() ghpkg.PRVerification { return base }, &WSTaskAssign{Kind: "issue", Repo: "o/r"}, assigned},
		{"external source", func() ghpkg.PRVerification { return base }, &WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 42, SourceType: "jira"}, assigned},
	}
	hub := &ContributeWSHub{logger: covBLogger()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := tc.pr()
			if unrelatedReportedPR(pr, tc.task, tc.assignedAt) {
				t.Fatal("must not be treated as unrelated")
			}
			got := hub.dropUnrelatedReportedPR(tc.task, pr, "u", "c", tc.assignedAt)
			if got.Verified != pr.Verified || got.Reason != pr.Reason {
				t.Fatalf("PR changed: got %+v, want %+v", got, pr)
			}
		})
	}
}

// Issue numbers are matched whole: #4 is not #42, and branch fix-420 is not 42.
func TestPRTextReferencesIssue_WholeNumber(t *testing.T) {
	if prTextReferencesIssue(ghpkg.PRVerification{Body: "Fixes #420 and #4", HeadRef: "fix-420"}, 42) {
		t.Fatal("#420 / fix-420 must not reference #42")
	}
	if !prTextReferencesIssue(ghpkg.PRVerification{Body: "Fixes #42"}, 42) {
		t.Fatal("#42 at end of text must reference #42")
	}
	if !prTextReferencesIssue(ghpkg.PRVerification{HeadRef: "42-fix"}, 42) {
		t.Fatal("branch starting with the number must reference it")
	}
}
