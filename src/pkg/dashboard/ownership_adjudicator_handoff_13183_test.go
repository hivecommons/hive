package dashboard

import (
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#13183: the adjudicator's HAND-OFF verdict makes an
// escalated Hive PR your turn at once, the wait it ends says who it waits on
// and until when, and the timeout reason no longer claims the adjudicator
// never acted.

func TestAdjudicatorHandoffIsYourTurnImmediately13183(t *testing.T) {
	escalated := time.Date(2026, 10, 12, 11, 40, 0, 0, time.UTC)
	env := ownershipEnv11565(6)
	env.escalatedAt = func(string, int) time.Time { return escalated }

	got := decidePROwnership(hivePR11565("needs-human", ownershipLabelReviewerHandoff), false, env)
	checkOwnership11565(t, got, want11565{OwnershipVerbReview, OwnershipReasonAdjudicatorHandedOff})
	if got.WaitingElsewhere {
		t.Fatalf("handed-off PR = %+v, must not also be waiting elsewhere", got)
	}
	if !strings.Contains(got.Reason, "Next step for a maintainer") {
		t.Fatalf("reason = %q, want it to point at the adjudicator's next-step line", got.Reason)
	}

	// A re-escalation after a pass still belongs to a true human.
	checkOwnership11565(t, decidePROwnership(hivePR11565("needs-human", ownershipLabelReviewerPassed, ownershipLabelReviewerHandoff), false, env),
		want11565{OwnershipVerbReview, OwnershipReasonAdjudicatorReescalated})

	// Without needs-human the hand-off is done: the label alone is no claim.
	checkOwnership11565(t, decidePROwnership(hivePR11565(ownershipLabelReviewerHandoff), false, env), notYours11565)

	// Below the adjudicator levels the plain needs-human rule still applies.
	checkOwnership11565(t, decidePROwnership(hivePR11565("needs-human", ownershipLabelReviewerHandoff), false, ownershipEnv11565(4)),
		want11565{OwnershipVerbReview, OwnershipReasonHivePRNeedsHuman})
}

func TestAdjudicatorWaitCarriesAReason13183(t *testing.T) {
	escalated := time.Date(2026, 10, 12, 11, 32, 0, 0, time.UTC)
	env := ownershipEnv11565(6)
	env.escalatedAt = func(string, int) time.Time { return escalated }

	got := decidePROwnership(hivePR11565("needs-human"), false, env)
	if got.YourTurn || !got.WaitingElsewhere {
		t.Fatalf("within the adjudicator wait = %+v, want waiting elsewhere", got)
	}
	want := "Waiting for the adjudicator (escalated 11:32Z); your turn at 15:32Z if it doesn't act"
	if got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}

	unknown := env
	unknown.escalatedAt = nil
	if got := decidePROwnership(hivePR11565("needs-human"), false, unknown); !got.WaitingElsewhere || got.Reason != "Waiting for the adjudicator" {
		t.Fatalf("unknown escalation time = %+v, want waiting for the adjudicator", got)
	}
}

func TestAdjudicatorWaitElapsedReason13183(t *testing.T) {
	env := ownershipEnv11565(6)
	env.escalatedAt = func(string, int) time.Time { return ownershipNow11565.Add(-ownershipAdjudicatorWait - time.Hour) }

	got := decidePROwnership(hivePR11565("needs-human"), false, env)
	checkOwnership11565(t, got, want11565{OwnershipVerbReview, OwnershipReasonAdjudicatorWaitElapsed})
	if want := "Escalated over 4 hours ago and no verdict Hive recognises was recorded"; got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}
}
