package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
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
	level, low := 5, 2
	cfg := &config.Config{ACMMLevel: &level, Project: config.ProjectConfig{Org: "o", RepoPolicies: []config.RepoPolicy{{Repo: "low", ACMMLevel: &low}}}}
	env := newOwnershipEnv(cfg, statuses, "idle", ownershipNow11565, hub)
	for lane, want := range map[string]bool{"quality": true, "scanner": false, "stopped": false, "missing": false, "": false} {
		if got := env.laneCanOpenPRFor(lane, "o/r"); got != want {
			t.Errorf("laneCanOpenPRFor(%q) = %v, want %v", lane, got, want)
		}
	}
	// A repository pinned to L2 lowers the lane's default mode to advisory
	// even though the hive is at L5 and the agent launched with push rights.
	if env.laneCanOpenPRFor("quality", "low") {
		t.Errorf("a lane must not open PRs in a repository whose effective level is L2")
	}
	// Both the full and the bare repository spelling match the lease.
	for _, repo := range []string{"o/r", "O/R", "r", "R"} {
		if !env.leasedFor(repo, 5) {
			t.Errorf("leasedFor(%q, 5) = false, want true", repo)
		}
	}
	if env.leasedFor("o/r", 6) || env.leasedFor("other/r", 5) {
		t.Fatalf("leasedFor matched an item without an unexpired lease")
	}
	expired := newOwnershipEnv(cfg, statuses, "idle", ownershipNow11565.Add(2*leaseTTL), hub)
	if expired.leasedFor("o/r", 5) {
		t.Fatalf("an expired lease must not count as a claim")
	}
}

func TestOwnershipLaneLevelGate11592(t *testing.T) {
	// L2 hive: no lane may open PRs, so a stale backlog issue shows no marker.
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("quality"), ownershipEnv11565(2)), notYours11565)
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("quality"), ownershipEnv11565(3)), safetyNet11592)

	// A repository pinned to L2 inside an L5 hive.
	env := ownershipEnv11565(5)
	env.repoLevel = func(repo string) int {
		if repo == "o/r" {
			return 2
		}
		return 5
	}
	env.laneCanOpenPR = func(lane, repo string) bool {
		return agent.DefaultAgentMode(lane, env.levelFor(repo)).CanPush()
	}
	checkOwnership11565(t, decideIssueOwnership(staleIssue11592("quality"), env), notYours11565)
	other := staleIssue11592("quality")
	other.repo = "o/other"
	checkOwnership11565(t, decideIssueOwnership(other, env), safetyNet11592)
}

func TestDecideRepoOwnershipRoutesLaneAndClaims11592(t *testing.T) {
	env := ownershipEnv11565(4)
	stale := ownershipNow11565.AddDate(0, 0, -20)
	calls := 0
	env.routeLane = func(github.Issue) string { calls++; return "quality" }
	expired, live := ownershipNow11565.Add(-time.Minute), ownershipNow11565.Add(time.Hour)
	issue := func(n int, lane string, claimedBy string, expires *time.Time) github.Issue {
		return github.Issue{Repo: "o/r", Number: n, Lane: lane, ClaimedBy: claimedBy, ClaimExpiresAt: expires, CreatedAt: stale, UpdatedAt: stale}
	}
	repos := []FrontendRepo{{Name: "o/r", ActionableIssues: []any{
		issue(1, "", "", nil),                // lane derived by the classifier
		issue(2, "scanner", "", nil),         // lane already set: not re-routed, cannot push
		issue(3, "quality", "bob", &live),    // unexpired claim
		issue(4, "quality", "bob", &expired), // expired claim
	}}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	got := map[int]Ownership{}
	for n := 1; n <= 4; n++ {
		got[n] = repos[0].Ownership[ownershipKey(ownershipKindIssue, n)]
	}
	checkOwnership11565(t, got[1], safetyNet11592)
	checkOwnership11565(t, got[2], notYours11565)
	checkOwnership11565(t, got[3], notYours11565)
	checkOwnership11565(t, got[4], safetyNet11592)
	if calls != 1 {
		t.Errorf("routeLane called %d times, want 1 (only for the issue with no lane)", calls)
	}
}
