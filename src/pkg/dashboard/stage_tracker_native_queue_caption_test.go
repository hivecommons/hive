package dashboard

import (
	"strings"
	"testing"

	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// A head the merge queue failed more than once on the same base tip parks
// at 085; the caption says why in plain words instead of the reason slug,
// with the slow-retry hours from the shared constants.
func TestHumanizeNativeQueueRejectedRepeatedly(t *testing.T) {
	got := humanizeStageReason(stageengineprstage.AutoMergeReasonNativeQueueRejectedRepeatedly)
	for _, want := range []string{"failed this head repeatedly on the same base", "retries it every 6 hours (first after 2 hours)", "Retry now"} {
		if !strings.Contains(got, want) {
			t.Fatalf("humanizeStageReason = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "native-merge-queue") || strings.Contains(got, "twice") {
		t.Fatalf("humanizeStageReason = %q: no slug, and no count that goes stale", got)
	}
	if strings.ContainsAny(got, "\u2013\u2014@") {
		t.Fatalf("humanizeStageReason = %q: no en/em dashes or @ in emitted text", got)
	}
}

// The 085 row for a queue-rejected head says what to fix and offers a real
// retry: Approve leads as "Retry now (the cause is fixed)" (an approval of
// the head after the failure re-arms the lane's mark), even when someone
// approved the head before; Give direction and Let hive decide, which can
// send a fixer at the PR, are off.
func TestDecisionForPRNativeQueueRejectedRepeatedly(t *testing.T) {
	it := StageTrackerItem{Repo: "acme/widget", Number: 7, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "4b9cac9c10ec", Reason: stageengineprstage.AutoMergeReasonNativeQueueRejectedRepeatedly}
	for _, approver := range []string{"", "octocat"} {
		it.HumanApprover = approver
		d := decisionForPR(it, nil, "", false)
		if !strings.Contains(d.Question, "merge queue failed this head repeatedly on the same base") || !strings.Contains(d.Question, "every 6 hours") {
			t.Fatalf("approver %q: question = %q", approver, d.Question)
		}
		if !strings.Contains(d.Blocker, "2 hours after the second failure and then every 6 hours") {
			t.Fatalf("approver %q: blocker = %q", approver, d.Blocker)
		}
		if !strings.Contains(d.Question, "stopped queueing it every 30 minutes") {
			t.Fatalf("approver %q: question = %q, want the 30-minute recheck named", approver, d.Question)
		}
		var retry, direction, delegate *StageTrackerDecisionOption
		for i := range d.Options {
			switch d.Options[i].Action {
			case decisionActionApprove:
				retry = &d.Options[i]
			case decisionActionDirection:
				direction = &d.Options[i]
			case decisionActionDelegate:
				delegate = &d.Options[i]
			}
		}
		if retry == nil || !retry.Enabled || !retry.Lead || retry.Label != decisionRetryNowLabel || retry.Consequence == "" {
			t.Fatalf("approver %q: retry option = %+v, want the enabled lead %q", approver, retry, decisionRetryNowLabel)
		}
		if direction == nil || direction.Enabled || direction.ReasonIfDisabled == "" {
			t.Fatalf("approver %q: direction option = %+v, want it disabled with a reason", approver, direction)
		}
		if delegate == nil || delegate.Enabled || delegate.ReasonIfDisabled == "" {
			t.Fatalf("approver %q: delegate option = %+v, want it disabled with a reason", approver, delegate)
		}
		for _, text := range []string{d.Question, d.Blocker, retry.Label, retry.Consequence, direction.ReasonIfDisabled, delegate.ReasonIfDisabled} {
			if strings.ContainsAny(text, "\u2013\u2014@") {
				t.Fatalf("approver %q: %q: no en/em dashes or @ in emitted text", approver, text)
			}
		}
	}
}
