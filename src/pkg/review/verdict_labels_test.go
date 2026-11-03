package review

import (
	"reflect"
	"testing"
)

func verdictLabelTestSet() VerdictLabelSet {
	return VerdictLabelSet{
		Reviewing:        "hive-review/in-progress",
		Approve:          "hive-review/approved",
		ChangesRequested: "hive-review/changes-requested",
		RequiresHuman:    "hive-review/needs-human",
	}
}

func TestPlanVerdictLabelsFollowsTheCurrentHead(t *testing.T) {
	set := verdictLabelTestSet()
	artifact := Artifact{Items: []Aggregate{
		{Repo: "Org/Repo", Number: 1, HeadSHA: "aaa111", Verdict: VerdictApprove},
		{Repo: "org/repo", Number: 2, HeadSHA: "bbb222", Verdict: VerdictChangesRequested},
		{Repo: "org/repo", Number: 3, HeadSHA: "ccc333", Verdict: VerdictReject},
		{Repo: "org/repo", Number: 4, HeadSHA: "old444", Verdict: VerdictApprove},
		{Repo: "org/repo", Number: 6, HeadSHA: "fff666", Verdict: VerdictIncomplete},
	}}
	state := DispatchState{Pending: []PendingReview{
		{Repo: "repo", Number: 5, HeadSHA: "eee555", Perspective: PerspectiveCorrectness, Agent: "r"},
	}}
	prs := []PullRequest{
		// Bare repo name matches the aggregate's owner/name, case-insensitively.
		{Repo: "repo", Number: 1, HeadSHA: "aaa111"},
		{Repo: "org/repo", Number: 2, HeadSHA: "bbb222", Labels: []string{"hive-review/in-progress"}},
		{Repo: "org/repo", Number: 3, HeadSHA: "ccc333"},
		// New head pushed: the previous head's approve label goes.
		{Repo: "org/repo", Number: 4, HeadSHA: "new444", Labels: []string{"hive-review/approved", "bug"}},
		{Repo: "repo", Number: 5, HeadSHA: "eee555"},
		{Repo: "org/repo", Number: 6, HeadSHA: "fff666"},
		// Already correct: no change.
		{Repo: "org/repo", Number: 7, HeadSHA: "ggg777"},
	}
	got := PlanVerdictLabels(set, prs, artifact, state, 0)
	want := []VerdictLabelChange{
		{Repo: "repo", Number: 1, Add: "hive-review/approved"},
		{Repo: "org/repo", Number: 2, Add: "hive-review/changes-requested", Remove: []string{"hive-review/in-progress"}},
		{Repo: "org/repo", Number: 3, Add: "hive-review/needs-human"},
		{Repo: "org/repo", Number: 4, Remove: []string{"hive-review/approved"}},
		{Repo: "repo", Number: 5, Add: "hive-review/in-progress"},
		{Repo: "org/repo", Number: 6, Add: "hive-review/in-progress"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes =\n%+v\nwant\n%+v", got, want)
	}
}

func TestPlanVerdictLabelsIsIdempotentAndCapped(t *testing.T) {
	set := verdictLabelTestSet()
	artifact := Artifact{Items: []Aggregate{{Repo: "o/r", Number: 1, HeadSHA: "a", Verdict: VerdictApprove}}}
	settled := []PullRequest{{Repo: "o/r", Number: 1, HeadSHA: "a", Labels: []string{"Hive-Review/Approved"}}}
	if got := PlanVerdictLabels(set, settled, artifact, DispatchState{}, 0); len(got) != 0 {
		t.Fatalf("already-correct PR planned %+v", got)
	}
	state := DispatchState{Pending: []PendingReview{
		{Repo: "o/r", Number: 2, HeadSHA: "b"}, {Repo: "o/r", Number: 3, HeadSHA: "c"},
	}}
	prs := []PullRequest{{Repo: "o/r", Number: 2, HeadSHA: "b"}, {Repo: "o/r", Number: 3, HeadSHA: "c"}}
	if got := PlanVerdictLabels(set, prs, Artifact{}, state, 1); len(got) != 1 {
		t.Fatalf("cap 1 planned %d changes", len(got))
	}
}

func TestPlanVerdictLabelsOffAndPartialSets(t *testing.T) {
	prs := []PullRequest{{Repo: "o/r", Number: 1, HeadSHA: "a", Labels: []string{"hive-review/approved"}}}
	artifact := Artifact{Items: []Aggregate{{Repo: "o/r", Number: 1, HeadSHA: "a", Verdict: VerdictApprove}}}
	if got := PlanVerdictLabels(VerdictLabelSet{}, prs, artifact, DispatchState{}, 0); got != nil {
		t.Fatalf("disabled set planned %+v", got)
	}
	// Only requires_human configured: an approved head carries nothing, and
	// labels the hive does not manage are never touched.
	only := VerdictLabelSet{RequiresHuman: "needs-human"}
	if got := PlanVerdictLabels(only, prs, artifact, DispatchState{}, 0); len(got) != 0 {
		t.Fatalf("unmanaged label touched: %+v", got)
	}
}
