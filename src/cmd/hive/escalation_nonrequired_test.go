package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

const advisoryCheck = "Mutation check on changed lines (advisory)"

// advisoryRedPR is a hive PR whose only failing check is outside the base
// branch's required set {"test"}, every required check having reported: the
// shape of Danathar/tududi#467, which GitHub called unstable (mergeable).
func advisoryRedPR(headSHA string) github.PullRequest {
	pr := redPR("widgets", 7, "hive-agent", headSHA)
	pr.FailingChecks = []string{advisoryCheck}
	pr.Protection = &github.ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true}
	return pr
}

func TestEscalationObservationNonRequiredFailure(t *testing.T) {
	cfg := escalationTestConfig()
	requiredRed := redPR("widgets", 7, "hive-agent", "abc")
	requiredRed.FailingChecks = []string{"test", advisoryCheck}
	requiredRed.Protection = &github.ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true, FailingRequiredChecks: []string{"test"}}
	unknown := redPR("widgets", 7, "hive-agent", "abc")
	unknown.FailingChecks = []string{advisoryCheck}

	for _, tc := range []struct {
		name        string
		pr          github.PullRequest
		wantRed     bool
		wantPending bool
	}{
		{"required {test}, failing {advisory}", advisoryRedPR("abc"), false, true},
		{"required {test}, failing {test, advisory}", requiredRed, true, false},
		{"required set unknown, failing {advisory}", unknown, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := escalationObservation(cfg, tc.pr, nil)
			if o.Red != tc.wantRed || o.Pending != tc.wantPending {
				t.Errorf("Red = %v, Pending = %v; want Red = %v, Pending = %v", o.Red, o.Pending, tc.wantRed, tc.wantPending)
			}
		})
	}
}

// The reaper must not re-dispatch a fix for a PR whose only red check is
// advisory: there is nothing to fix and the sweep can merge it as it stands.
func TestReapStuckRedPRs_NonRequiredFailureGetsNoKick(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	actionable := actionableWith(advisoryRedPR("abc"))
	kicker := &fakeKicker{}

	for i := 0; i < escalation.MaxReEngagements+2; i++ {
		recordRedStaleness(cfg, actionable)
		*clock = clock.Add(escalation.RedPRStaleAfter + escalation.ReEngageCooldown + time.Minute)
		reapStuckRedPRs(cfg, actionable, map[string]bool{}, kicker, nil, discardLogger())
	}

	if len(kicker.kicks) != 0 {
		t.Fatalf("advisory-only red PR received %d fix kicks, want 0:\n%s", len(kicker.kicks), kicker.kicks[0].message)
	}
	if got := store.ReEngagements("acme/widgets", 7); got != 0 {
		t.Errorf("advisory-only red PR spent %d re-engagements, want 0", got)
	}
}

// Nor may the escalation sweep count its heads as failed fix attempts and park
// it with needs-human.
func TestRunEscalationSweep_NonRequiredFailureNeverEscalates(t *testing.T) {
	newTestEscalationStore(t)
	cfg := escalationTestConfig()
	cfg.Escalation.Threshold = 2
	w := &fakeIssueWriter{}

	for _, sha := range []string{"sha-1", "sha-2", "sha-3"} {
		if got := runEscalationSweep(context.Background(), cfg, w, actionableWith(advisoryRedPR(sha)), nil, nil, discardLogger()); len(got) != 0 {
			t.Fatalf("head %s: escalated = %v, want none", sha, got)
		}
	}
	if len(w.comments) != 0 || len(w.labels) != 0 {
		t.Fatalf("advisory-only red PR was escalated: comments=%v labels=%v", w.comments, w.labels)
	}
}

// The other HasFailingRequiredCheck callers follow the same rule: a claiming
// PR red only on an advisory check keeps suppressing its issue, and a
// dependency-bot PR is not handed off for red CI. The head was red on its
// required check first, then passed it on a re-run while the advisory job
// stayed red: the advisory-only pass is Pending, which keeps the staleness
// clock, so only the predicate stands between the PR and a release.
func TestNonRequiredFailureIsNotRedForOtherCallers(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	requiredRed := advisoryRedPR("abc")
	requiredRed.FailingChecks = []string{"test", advisoryCheck}
	requiredRed.Protection = &github.ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true, FailingRequiredChecks: []string{"test"}}
	recordRedStaleness(cfg, actionableWith(requiredRed))
	*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)

	actionable := actionableWith(advisoryRedPR("abc"))
	recordRedStaleness(cfg, actionable)
	if !store.StaleRed("acme/widgets", 7, "abc") {
		t.Fatal("setup: the head's staleness clock should survive the advisory-only pass")
	}
	if redStale := claimingPRRedStale(cfg, actionable); redStale("widgets", 7) {
		t.Error("claimingPRRedStale released the claim of a PR red only on an advisory check")
	}
	if dependencyBotRedCI(advisoryRedPR("abc")) {
		t.Error("dependencyBotRedCI handed off a PR red only on an advisory check")
	}
}
