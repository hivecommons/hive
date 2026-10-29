package dashboard

import (
	"testing"
	"time"
)

// hivecommons/hive#15469: the owner filed an issue, the hive dispatched its
// own fixer nine seconds later, and the owner's PR (`Fixes #N`) arrived 45 s
// after that. The ledger stopped offering the issue but the in-flight task
// was never told, so the fixer opened a second PR. An open PR by someone
// else covering the issue now releases the task (and reassigns the
// session); a sibling on other work and the author's own PR are untouched.
func TestPreemptIssueCoveredByPR_ReleasesInFlightTaskByOtherAuthor(t *testing.T) {
	hub, s, _ := claimsHub(t)
	setStatusIssues(s,
		intgIssue(15469, "merged counter", "clubanderson", nil),
		intgIssue(2, "other work", "someone", nil),
		intgIssue(3, "sibling's work", "someone", nil),
	)
	profile := &ContributorProfile{GitHubUsername: "odttlnt", ContributorID: "c-odttlnt", TrustTier: "contributor"}
	fixer := &ContributorConnection{
		profile: profile, session: "a",
		currentTask:    &WSTaskAssign{TaskID: "ct-15469", Repo: "myorg/repo1", Number: 15469},
		currentTaskGen: 7, lastLeaseRenew: time.Now(), lastPong: time.Now(),
	}
	sibling := &ContributorConnection{
		profile: profile, session: "b",
		currentTask:    &WSTaskAssign{TaskID: "ct-3", Repo: "myorg/repo1", Number: 3},
		currentTaskGen: 9, lastLeaseRenew: time.Now(), lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-a"] = fixer
	hub.connections["conn-b"] = sibling
	hub.mu.Unlock()

	if n := hub.PreemptIssueCoveredByPR("MyOrg/Repo1", 15469, 15470, "clubanderson"); n != 1 {
		t.Fatalf("released %d sessions, want 1", n)
	}
	fixer.mu.Lock()
	cur, gen := fixer.currentTask, fixer.currentTaskGen
	fixer.mu.Unlock()
	if cur == nil || cur.Number == 15469 {
		t.Fatalf("fixer not moved off #15469: %+v", cur)
	}
	if cur.Number != 2 {
		t.Fatalf("fixer reassigned to #%d, want #2", cur.Number)
	}
	if gen == 7 {
		t.Fatal("assignment generation not bumped — a stale completion would not be fenced")
	}
	sibling.mu.Lock()
	sib := sibling.currentTask
	sibling.mu.Unlock()
	if sib == nil || sib.Number != 3 {
		t.Fatalf("sibling disturbed: %+v", sib)
	}
}

func TestPreemptIssueCoveredByPR_LeavesAuthorsOwnTaskAlone(t *testing.T) {
	hub, s, _ := claimsHub(t)
	setStatusIssues(s, intgIssue(15469, "merged counter", "clubanderson", nil))
	fixer := &ContributorConnection{
		profile: &ContributorProfile{GitHubUsername: "odttlnt", ContributorID: "c-odttlnt", TrustTier: "contributor"}, session: "a",
		currentTask:    &WSTaskAssign{TaskID: "ct-15469", Repo: "myorg/repo1", Number: 15469},
		currentTaskGen: 7, lastLeaseRenew: time.Now(), lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-a"] = fixer
	hub.mu.Unlock()

	// The fixer's own PR arrives through the same webhook.
	if n := hub.PreemptIssueCoveredByPR("myorg/repo1", 15469, 15473, "ODTTLNT"); n != 0 {
		t.Fatalf("released %d sessions for the author's own PR, want 0", n)
	}
	// A PR on a different issue is not this task's business either.
	if n := hub.PreemptIssueCoveredByPR("myorg/repo1", 99, 15474, "clubanderson"); n != 0 {
		t.Fatalf("released %d sessions for another issue, want 0", n)
	}
	fixer.mu.Lock()
	cur, gen := fixer.currentTask, fixer.currentTaskGen
	fixer.mu.Unlock()
	if cur == nil || cur.Number != 15469 || gen != 7 {
		t.Fatalf("own task disturbed: %+v gen=%d", cur, gen)
	}
	if hub.PreemptIssueCoveredByPR("myorg/repo1", 0, 15470, "x") != 0 || (*ContributeWSHub)(nil).PreemptIssueCoveredByPR("myorg/repo1", 1, 2, "x") != 0 {
		t.Fatal("degenerate inputs must release nothing")
	}
}
