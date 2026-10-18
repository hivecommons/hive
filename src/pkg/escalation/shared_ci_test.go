package escalation

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// redObs builds a red observation with a named failing check set.
func redObs(number int, sha string, checks ...string) Observation {
	return Observation{
		Repo: "org/repo", Number: number, HeadSHA: sha, Red: true,
		FailingChecks: checks,
	}
}

func controlObs(number int, sha string, checks ...string) Observation {
	o := redObs(number, sha, checks...)
	o.IndependentControl = true
	return o
}

// A shared failure needs independent evidence. One human-authored control
// failing the same check is enough; requiring four agent PRs let a repo-wide
// outage consume the first three PRs' fix budgets.
func TestMarkSharedFailures_IndependentControlConfirmsFailure(t *testing.T) {
	obs := []Observation{
		redObs(1, "agent", "build-and-test"),
		controlObs(2, "human", "build-and-test"),
	}

	MarkSharedFailures(obs)

	if !obs[0].Shared {
		t.Fatalf("agent PR was not held despite a red independent control: %+v", obs[0])
	}
}

// A mixed PR keeps its PR-local failure countable while recording the shared
// subset — #9258's changelog-fragment-guard was a real defect.
func TestMarkSharedFailures_MixedFailureKeepsLocalCheck(t *testing.T) {
	obs := []Observation{
		redObs(1, "agent", "build-and-test", "changelog-fragment-guard"),
		controlObs(2, "human", "build-and-test"),
	}

	MarkSharedFailures(obs)

	if obs[0].Shared {
		t.Fatalf("PR-local failure was suppressed: %+v", obs[0])
	}
	if len(obs[0].SharedChecks) != 1 || obs[0].SharedChecks[0] != "build-and-test" {
		t.Fatalf("shared checks = %v, want [build-and-test]", obs[0].SharedChecks)
	}
}

// Agent PRs are not independent controls for one another. They may all carry
// the same bad generated change.
func TestMarkSharedFailures_AgentPRsAloneAreLocal(t *testing.T) {
	obs := []Observation{
		redObs(1, "a", "test"),
		redObs(2, "b", "test"),
	}

	MarkSharedFailures(obs)

	for _, o := range obs {
		if o.Shared || len(o.SharedChecks) != 0 {
			t.Errorf("PR %d used another agent PR as a control: %+v", o.Number, o)
		}
	}
}

// A green PR is direct counter-evidence to a repo-wide outage. Counting only
// red PRs suppresses genuine PR-local failures whenever enough unrelated PRs
// happen to fail the same check.
func TestMarkSharedFailures_GreenPRDisprovesRepoWideFailure(t *testing.T) {
	obs := []Observation{
		redObs(1, "agent", "test"),
		controlObs(2, "human", "test"),
		{Repo: "org/repo", Number: 3, HeadSHA: "green"},
	}

	MarkSharedFailures(obs)

	if obs[0].Shared || len(obs[0].SharedChecks) != 0 {
		t.Errorf("agent PR read as shared despite a green PR: %+v", obs[0])
	}
}

// Pending PRs provide no evidence about whether a failure is repository-wide.
func TestMarkSharedFailures_PendingPRDoesNotDisproveFailure(t *testing.T) {
	obs := []Observation{
		redObs(1, "agent", "test"),
		controlObs(2, "human", "test"),
		{Repo: "org/repo", Number: 3, HeadSHA: "pending", Pending: true},
	}

	MarkSharedFailures(obs)

	if !obs[0].Shared {
		t.Errorf("pending PR incorrectly disproved shared failure: %+v", obs[0])
	}
}

// Identically named checks in different repositories are independent. A
// failure in one repository must never suppress escalation in another.
func TestMarkSharedFailures_DoesNotCombineRepos(t *testing.T) {
	otherRepo := controlObs(2, "human", "test")
	otherRepo.Repo = "org/other"
	obs := []Observation{
		redObs(1, "agent", "test"),
		otherRepo,
	}

	MarkSharedFailures(obs)

	for _, o := range obs {
		if o.Shared || len(o.SharedChecks) != 0 {
			t.Errorf("%s#%d borrowed failures from another repository: %+v", o.Repo, o.Number, o)
		}
	}
}

// A wholly-shared red pass carries no information about THIS PR's fix loop, so
// it must behave exactly like a pending pass: no attempt counted, ever.
func TestSweep_SharedFailuresNeverAdvanceAttempts(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	for i, sha := range []string{"a", "b", "c", "d"} {
		obs := []Observation{
			redObs(1, sha, "build-and-test"),
			controlObs(2, "human", "build-and-test"),
		}
		MarkSharedFailures(obs)
		r := s.Sweep(obs[:1], DefaultThreshold)[Key("org/repo", 1)]
		if r.NewlyEscala {
			t.Fatalf("pass %d: shared breakage must never escalate: %+v", i+1, r)
		}
		if r.Attempts != 0 {
			t.Fatalf("pass %d: shared breakage counted %d attempts, want 0", i+1, r.Attempts)
		}
	}
}

// An empty `ci: retrigger` commit is a new head SHA over an IDENTICAL tree. It
// is not a fix attempt: #9285 escalated as "3 distinct fix attempts" whose
// three SHAs all carried tree fdac5ef23.
func TestSweep_RetriggerCommitsShareATreeAndCountOnce(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	var last Result
	for _, sha := range []string{"7bc0bc147", "5054e6d8c", "8b6b79e61"} {
		o := redObs(7, sha, "test")
		o.HeadTree = "fdac5ef23"
		last = s.Sweep([]Observation{o}, DefaultThreshold)[Key("org/repo", 7)]
	}
	if last.Attempts != 1 {
		t.Fatalf("three retriggers over one tree = %d attempts, want 1", last.Attempts)
	}
	if last.NewlyEscala {
		t.Fatal("retriggers over one tree must not reach the escalation threshold")
	}

	// A real fix — a new tree — does count.
	o := redObs(7, "469ede237", "test")
	o.HeadTree = "aaaa1111"
	got := s.Sweep([]Observation{o}, DefaultThreshold)[Key("org/repo", 7)]
	if got.Attempts != 2 {
		t.Fatalf("a new tree must count as an attempt: got %d, want 2", got.Attempts)
	}
}

// A labeled PR that goes green must be un-parked by the sweep. Before this the
// labeled branch returned BEFORE the green branch, so the entry stayed
// escalated and the label stayed on until a human removed it.
func TestSweep_LabeledGreenPRIsUnparked(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	s.Sweep([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "a", Red: true, Labeled: true}}, DefaultThreshold)

	r := s.Sweep([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "a", Labeled: true}}, DefaultThreshold)[Key("org/repo", 7)]
	if !r.Unparked || r.Escalated {
		t.Fatalf("green labeled PR must be un-parked: %+v", r)
	}
	if !strings.Contains(r.UnparkReason, "green") {
		t.Fatalf("un-park reason must say why: %q", r.UnparkReason)
	}

	// The label removal has not landed yet: retry it, but never comment twice
	// and never re-escalate.
	again := s.Sweep([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "a", Labeled: true}}, DefaultThreshold)[Key("org/repo", 7)]
	if !again.Unparked || again.UnparkReason != "" || again.Escalated {
		t.Fatalf("second pass must retry the label removal silently: %+v", again)
	}
}

// An escalation raised over shared breakage must clear itself when the shared
// incident does — #9258 sat needs-human for 31 hours waiting for a human after
// the base branch and the runners recovered.
func TestSweep_SharedIncidentRecoveryUnparks(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	// Escalate on a mixed head: one PR-local failure plus a shared one.
	for i := range DefaultThreshold {
		obs := []Observation{
			redObs(7, string(rune('a'+i)), "changelog-fragment-guard", "build-and-test"),
			controlObs(8, "human", "build-and-test"),
		}
		MarkSharedFailures(obs)
		s.Sweep(obs[:1], DefaultThreshold)
	}
	s.MarkEscalated("org/repo", 7)
	s.MarkLabelApplied("org/repo", 7)

	// The outage clears: build-and-test is red on this PR alone now.
	obs := []Observation{redObs(7, "z", "build-and-test")}
	obs[0].Labeled = true
	MarkSharedFailures(obs)
	r := s.Sweep(obs, DefaultThreshold)[Key("org/repo", 7)]
	if !r.Unparked {
		t.Fatalf("recovered shared incident must un-park the PR: %+v", r)
	}
	if !strings.Contains(r.UnparkReason, "build-and-test") {
		t.Fatalf("un-park reason must name the cleared checks: %q", r.UnparkReason)
	}
}

// The staleness clock must start when CI SETTLES. #9452 had its first failure
// twelve minutes before its last shard finished, so it could read as "stale"
// while CI was still running.
func TestObserveRed_ClockStartsWhenCISettles(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Date(2026, 9, 29, 7, 54, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	running := redObs(48, "abc", "overlayfs-exec-guard")
	running.CIRunning = true
	s.ObserveRed([]Observation{running})

	now = now.Add(RedPRStaleAfter + time.Minute)
	if s.StaleRed("org/repo", 48, "abc") {
		t.Fatal("a head with checks still running must never read as stale")
	}

	// CI settles: the clock starts NOW, not retroactively.
	settled := redObs(48, "abc", "overlayfs-exec-guard", "test")
	s.ObserveRed([]Observation{settled})
	if s.StaleRed("org/repo", 48, "abc") {
		t.Fatal("the clock must start at settlement, not at the first failure")
	}
	now = now.Add(RedPRStaleAfter + time.Minute)
	if !s.StaleRed("org/repo", 48, "abc") {
		t.Fatal("a settled red head unchanged past the threshold must read stale")
	}
}

// The exhausted escalation comment must quote the kicks actually DELIVERED.
// "6 automated fix re-dispatches" on a PR nobody was ever kicked about sent
// maintainers looking for a bug in the PR instead of in the delivery path.
func TestCommentBody_QuotesDeliveredKicksAndSharedChecks(t *testing.T) {
	body := CommentBody(Evidence{
		Attempts:      1,
		FailingChecks: []string{"test", "build-and-test"},
		SharedChecks:  []string{"build-and-test"},
		Exhausted:     true,
		ReEngagements: 2,
	})
	if !strings.Contains(body, "2 delivered fix re-dispatches") {
		t.Fatalf("comment must quote delivered kicks:\n%s", body)
	}
	if strings.Contains(body, "**Failing checks:** build-and-test, test") {
		t.Fatalf("shared checks must be listed separately:\n%s", body)
	}
	if !strings.Contains(body, "shared breakage, not this PR") {
		t.Fatalf("comment must name the shared breakage:\n%s", body)
	}
}
