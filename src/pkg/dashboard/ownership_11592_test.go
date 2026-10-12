package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/github"
)

func inLane11592(in issueOwnershipInput, lane string) issueOwnershipInput {
	in.lane = lane
	return in
}

func staleIssue11592(lane string, labels ...string) issueOwnershipInput {
	in := inLane11592(freshIssue11565(labels...), lane)
	stale := ownershipNow11565.AddDate(0, 0, -20)
	in.createdAt, in.updatedAt = stale, stale
	return in
}

var safetyNet11592 = want11565{OwnershipVerbReview, OwnershipReasonStaleSafetyNet}

func TestOwnershipSafetyNetUsesRoutedLane11592(t *testing.T) {
	env := ownershipEnv11565(4)
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("scanner"), env), notYours11565)
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("quality"), env), safetyNet11592)
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592(""), env), notYours11565)
}

func TestOwnershipLikelyDoneUsesRoutedLane11592(t *testing.T) {
	env := ownershipEnv11565(4)
	closeDone := want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone}
	fresh := func(lane string) issueOwnershipInput {
		return inLane11592(freshIssue11565("hive/likely-done"), lane)
	}
	checkOwnership11565(t, decideIssueOwnership(fresh("scanner"), env), closeDone)
	checkOwnership11565(t, decideIssueOwnership(fresh(""), env), closeDone)
	checkOwnership11565(t, decideIssueOwnership(fresh("quality"), env), notYours11565)
}

func TestOwnershipSafetyNetSkipsLiveClaims11592(t *testing.T) {
	env := ownershipEnv11565(4)
	in := staleIssue11592("quality")
	in.liveClaim = true
	checkOwnership11565(t, decideIssueOwnership(in, env), notYours11565)

	env.leased = func(repo string, number int) bool { return repo == "o/r" && number == 1 }
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("quality"), env), notYours11565)

	env.leased = func(string, int) bool { return false }
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("quality"), env), safetyNet11592)

	pr := hivePR11565()
	pr.CreatedAt, pr.UpdatedAt = ownershipNow11565.AddDate(0, 0, -20), ownershipNow11565.AddDate(0, 0, -20)
	pr.LinkedIssues = []github.PRLinkedIssue{{Number: 7}}
	checkOwnership11565(t, decidePROwnership(pr, false, env), safetyNet11592)
	env.leased = func(repo string, number int) bool { return repo == "o/r" && number == 7 }
	checkOwnership11565(t, decidePROwnership(pr, false, env), notYours11565)
	env.leased = func(repo string, number int) bool { return repo == "o/r" && number == pr.Number }
	checkOwnership11565(t, decidePROwnership(pr, false, env), notYours11565)
}

func TestOwnershipReporterConfirmationGate11592(t *testing.T) {
	env := ownershipEnv11565(4)
	gated := func(updated time.Time) issueOwnershipInput {
		in := inLane11592(freshIssue11565("hive/likely-done"), "scanner")
		in.reporterGate = true
		in.createdAt, in.updatedAt = updated, updated
		return in
	}
	checkOwnership11565(t, decideIssueOwnership(gated(ownershipNow11565.Add(-time.Hour)), env), notYours11565)
	checkOwnership11565(t, decideIssueOwnership(gated(ownershipNow11565.AddDate(0, 0, -20)), env), want11565{OwnershipVerbClose, OwnershipReasonReporterConfirmationStale})

	// The gate applies only to likely-done issues.
	plain := freshIssue11565()
	plain.reporterGate = true
	checkOwnership11565(t, decideIssueOwnership(plain, env), notYours11565)
}

func TestNewOwnershipEnvLaneAndLeases11592(t *testing.T) {
	statuses := map[string]*agent.AgentProcess{
		"quality": {State: agent.StateRunning, LaunchedMode: agent.ModeIssuesAndPRs},
		"scanner": {State: agent.StateRunning, LaunchedMode: agent.ModeIssuesOnly},
		"stopped": {State: agent.StateStopped, LaunchedMode: agent.ModeIssuesAndPRs},
	}
	hub := NewContributeWSHub(nil, nil)
	hub.taskLeasesFile = filepath.Join(t.TempDir(), "leases.json")
	if err := hub.recordLease("alice", "t1", "O/R", 5, "", 1, ownershipNow11565); err != nil {
		t.Fatalf("recordLease: %v", err)
	}
	env := newOwnershipEnv(nil, statuses, "idle", ownershipNow11565, hub)
	for lane, want := range map[string]bool{"quality": true, "scanner": false, "stopped": false, "missing": false, "": false} {
		if got := env.laneCanOpenPRFor(lane); got != want {
			t.Errorf("laneCanOpenPRFor(%q) = %v, want %v", lane, got, want)
		}
	}
	if !env.leasedFor("o/r", 5) || env.leasedFor("o/r", 6) {
		t.Fatalf("leasedFor did not match the unexpired lease on o/r#5 only")
	}
	expired := newOwnershipEnv(nil, statuses, "idle", ownershipNow11565.Add(2*leaseTTL), hub)
	if expired.leasedFor("o/r", 5) {
		t.Fatalf("an expired lease must not count as a claim")
	}
}
