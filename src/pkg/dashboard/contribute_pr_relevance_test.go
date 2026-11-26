package dashboard

import (
	"encoding/json"
	"strings"
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
	if prTextReferencesIssue(ghpkg.PRVerification{Body: "Fixes #420 and #4", HeadRef: "fix-420"}, "o/r", 42) {
		t.Fatal("#420 / fix-420 must not reference #42")
	}
	if !prTextReferencesIssue(ghpkg.PRVerification{Body: "Fixes #42"}, "o/r", 42) {
		t.Fatal("#42 at end of text must reference #42")
	}
	if !prTextReferencesIssue(ghpkg.PRVerification{HeadRef: "42-fix"}, "o/r", 42) {
		t.Fatal("branch starting with the number must reference it")
	}
}

// A reference to the same number in another repository is not this issue.
func TestPRTextReferencesIssue_OtherRepoDoesNotCount(t *testing.T) {
	for _, body := range []string{"Fixes other/repo#42", "see https://github.com/other/repo/issues/42"} {
		if prTextReferencesIssue(ghpkg.PRVerification{Body: body}, "o/r", 42) {
			t.Errorf("%q must not reference o/r#42", body)
		}
	}
	for _, body := range []string{"Fixes o/r#42", "Fixes O/R#42", "https://github.com/o/r/issues/42", "(#42)", "#42"} {
		if !prTextReferencesIssue(ghpkg.PRVerification{Body: body}, "o/r", 42) {
			t.Errorf("%q must reference o/r#42", body)
		}
	}
	if prTextReferencesIssue(ghpkg.PRVerification{Body: "path/to#42"}, "o/r", 42) {
		t.Error("an owner/repo#N that is not the task repo must not count")
	}
}

// The issue time survives a re-record of the same task and a persist/load
// round trip, so a resumed task still gets the relevance check.
func TestTaskLease_IssuedAtSurvivesRerecordAndReload(t *testing.T) {
	h := &ContributeWSHub{logger: covBLogger()}
	first := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	if err := h.recordLease("alice", "t1", "o/r", 42, "trusted", 1, first); err != nil {
		t.Fatal(err)
	}
	if err := h.recordLease("alice", "t1", "o/r", 42, "trusted", 2, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.leaseMu.Lock()
	got := h.leases[leaseKey("alice", "t1")].issuedAt
	h.leaseMu.Unlock()
	if !got.Equal(first) {
		t.Fatalf("issuedAt after re-record = %v, want %v", got, first)
	}
	if err := h.recordLease("alice", "t2", "o/r", 43, "trusted", 1, first.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.leaseMu.Lock()
	got = h.leases[leaseKey("alice", "t2")].issuedAt
	h.leaseMu.Unlock()
	if !got.Equal(first.Add(2 * time.Hour)) {
		t.Fatalf("a new task must get its own issuedAt, got %v", got)
	}
	raw, err := json.Marshal(persistedLease{TaskID: "t1", IssuedAt: first})
	if err != nil || !strings.Contains(string(raw), `"issued_at"`) {
		t.Fatalf("persisted lease = %s, %v", raw, err)
	}
	if raw, _ := json.Marshal(persistedLease{TaskID: "t1"}); strings.Contains(string(raw), "issued_at") {
		t.Fatalf("zero issuedAt must be omitted: %s", raw)
	}
}
