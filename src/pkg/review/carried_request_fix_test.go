package review

import "testing"

// A maintainer's change request carried over the hive's own base merges
// (A to B to C) is one request: the planner kicks the fixer once for it, not
// once per head, while that fix is pending. A request on a head the fixer
// pushed (no carry) is planned as before.
func TestPlanDispatchKicksOncePerCarriedRequest(t *testing.T) {
	opts := humanRequestOpts()
	at := func(head, carriedFrom string) PullRequest {
		pr := liveCasePR()
		pr.HeadSHA = head
		req := liveCaseRequest()
		req.Source, req.SourceID, req.HeadSHA, req.CarriedFromHead = HumanChangeRequestSourceReview, 77, head, carriedFrom
		pr.HumanChangeRequest = &req
		return pr
	}
	first := PlanDispatch([]PullRequest{at("aaaa", "")}, Artifact{}, DispatchState{}, opts)
	if len(first.FixKicks) != 1 {
		t.Fatalf("first fix kicks = %+v, want one", first.FixKicks)
	}
	state := first.State
	for _, head := range []string{"bbbb", "cccc"} {
		plan := PlanDispatch([]PullRequest{at(head, "aaaa")}, Artifact{}, state, opts)
		if len(plan.FixKicks) != 0 {
			t.Fatalf("head %s: fix kicks = %+v, want none for the same carried request", head, plan.FixKicks)
		}
		state = plan.State
	}
	// The same review id with no carry (a head the fixer pushed, the
	// reviewer still asking) follows the ordinary rules.
	if plan := PlanDispatch([]PullRequest{at("dddd", "")}, Artifact{}, state, opts); len(plan.FixKicks) != 1 {
		t.Fatalf("uncarried request fix kicks = %+v, want one", plan.FixKicks)
	}
}

// The fixer was kicked for the request on head A and its attempt is
// recorded (ConfirmDelivered) without a push; then the App merges the base
// into the branch. The carried request on the new head is kicked at once,
// under the ordinary rules, rather than waiting out PendingFixTTL.
func TestPlanDispatchCarriedRequestKicksAfterTheFixAttemptIsRecorded(t *testing.T) {
	opts := humanRequestOpts()
	at := func(head, carriedFrom string) PullRequest {
		pr := liveCasePR()
		pr.HeadSHA = head
		req := liveCaseRequest()
		req.Source, req.SourceID, req.HeadSHA, req.CarriedFromHead = HumanChangeRequestSourceReview, 77, head, carriedFrom
		pr.HumanChangeRequest = &req
		return pr
	}
	first := PlanDispatch([]PullRequest{at("aaaa", "")}, Artifact{}, DispatchState{}, opts)
	if len(first.FixKicks) != 1 {
		t.Fatalf("first fix kicks = %+v, want one", first.FixKicks)
	}
	state := ConfirmDelivered(first.State, first.FixKicks, first.FixKicks)
	if len(state.FixAttempts) != 1 {
		t.Fatalf("fix attempts = %+v, want the delivered fix recorded", state.FixAttempts)
	}
	plan := PlanDispatch([]PullRequest{at("bbbb", "aaaa")}, Artifact{}, state, opts)
	if len(plan.FixKicks) != 1 {
		t.Fatalf("carried request after the recorded attempt: fix kicks = %+v, want one at once", plan.FixKicks)
	}
}
