package review

import "testing"

func TestSameHeadSHA(t *testing.T) {
	const full = "98489e7ba141fc60c30c9aaf47774dc42f9b1c76"
	cases := []struct {
		a, b string
		want bool
	}{
		{full, full, true},
		{"98489e7", full, true},
		{full, "98489e7", true},
		{" 98489E7B ", full, true},
		{"98489e7", "98489e7", true},
		{"", "", true},
		{"", full, false},
		{"98489e", full, false},  // six digits is too short to trust
		{"98489e8", full, false}, // not a prefix
		{"deadbee", full, false}, // different head
		{"not-hex", "not-hex-abc", false},
		{"8eeb2602", "8eeb2602aa32918faf9869613a7a57c65c776d30", true},
	}
	for _, tc := range cases {
		if got := SameHeadSHA(tc.a, tc.b); got != tc.want {
			t.Errorf("SameHeadSHA(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCanonicalHeadSHA(t *testing.T) {
	const full = "98489e7ba141fc60c30c9aaf47774dc42f9b1c76"
	if got := CanonicalHeadSHA("98489e7", full); got != full {
		t.Fatalf("abbreviation not widened: %q", got)
	}
	if got := CanonicalHeadSHA("deadbee", full); got != "deadbee" {
		t.Fatalf("non-prefix must stay as written: %q", got)
	}
	if got := CanonicalHeadSHA(full, "98489e7"); got != full {
		t.Fatalf("full head must not shrink: %q", got)
	}
	if got := CanonicalHeadSHA("98489e7", ""); got != "98489e7" {
		t.Fatalf("blank full keeps the short head: %q", got)
	}
	if !IsAbbreviatedSHA("98489e7") || IsAbbreviatedSHA(full) || IsAbbreviatedSHA("abc") {
		t.Fatal("IsAbbreviatedSHA")
	}
}

// The live failure (hivecommons/hive#13205, 2026-10-20): the reviewer wrote
// head_sha "98489e7", the aggregate was keyed by it, and every lookup by the
// 40-character head returned nothing.
func TestHasAggregateApprovalMatchesAbbreviatedHead(t *testing.T) {
	const full = "98489e7ba141fc60c30c9aaf47774dc42f9b1c76"
	a := Artifact{Items: []Aggregate{{Repo: "hivecommons/hive", Number: 13205, HeadSHA: "98489e7", Verdict: VerdictApprove, MergeEligible: true}}}
	if !a.HasAggregateApproval("hivecommons/hive", 13205, full) {
		t.Fatal("approval recorded under the abbreviated head was not found by the full head")
	}
	if _, ok := a.AggregateForBody("hivecommons/hive", 13205, full, ""); !ok {
		t.Fatal("AggregateForBody did not find the abbreviated head")
	}
	if a.HasAggregateApproval("hivecommons/hive", 13205, "deadbeef00000000000000000000000000000000") {
		t.Fatal("a different head must not inherit the approval")
	}
}

// A verdict recorded under an abbreviated head must suppress a duplicate
// dispatch for that head, and a PR that moves to a new head with no verdict
// must be re-dispatched exactly once: the second plan sees the pending rows
// and adds nothing.
func TestPlanDispatchAbbreviatedHeadAndNewHeadRekick(t *testing.T) {
	const oldHead = "98489e7ba141fc60c30c9aaf47774dc42f9b1c76"
	const newHead = "8eeb2602aa32918faf9869613a7a57c65c776d30"
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}
	artifact := Artifact{Items: []Aggregate{{Repo: "acme/hive", Number: 7, HeadSHA: "98489e7", Verdict: VerdictApprove}}}

	plan := PlanDispatch([]PullRequest{dispatchPR(oldHead)}, artifact, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("verdict under the abbreviated head did not suppress re-review: %+v", plan.ReviewKicks)
	}

	plan = PlanDispatch([]PullRequest{dispatchPR(newHead)}, artifact, DispatchState{}, opts)
	if len(plan.ReviewKicks) == 0 {
		t.Fatal("new head without a verdict was not re-dispatched")
	}
	for _, k := range plan.ReviewKicks {
		if k.HeadSHA != newHead {
			t.Fatalf("kick for the wrong head: %+v", k)
		}
	}
	again := PlanDispatch([]PullRequest{dispatchPR(newHead)}, artifact, plan.State, opts)
	if len(again.ReviewKicks) != 0 {
		t.Fatalf("second plan re-kicked the same head: %+v", again.ReviewKicks)
	}
}
