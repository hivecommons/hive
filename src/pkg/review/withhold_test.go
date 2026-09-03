package review

// The withhold-only proof.
//
// The governing safety claim for the reviewer role is that a review verdict may
// only ever WITHHOLD a merge, never cause one. This mirrors the rule the
// approval desk already follows after the sweep's eligibility checks, and it is
// what makes the role safe to enable on an evidence base of six PRs: at worst a
// wrong verdict withholds a good merge for a human to clear from the batch
// queue; it can never wave a bad one through.
//
// The property holds structurally at the consumption site. Merge eligibility
// (cmd/hive/main.go) consults review approval as:
//
//	if requireReviewApproval && (!reviewLoaded || !reviewArtifact.HasAggregateApproval(...)) {
//	        continue
//	}
//
// The only effect available to the verdict is `continue` — dropping a PR from
// the eligible set. There is no branch on which a verdict ADDS a PR, and the
// check is reached only after every prior gate (draft, CI, mergeability, DCO)
// has already passed. These tests pin the half of that argument that lives in
// this package: HasAggregateApproval is a predicate that can only ever answer
// "approved" for a verdict that genuinely is a SHA-matched unanimous approval.

import "testing"

func approvedAggregate(repo string, number int, sha string) Aggregate {
	return Aggregate{
		Repo: repo, Number: number, HeadSHA: sha,
		Verdict: VerdictApprove, MergeEligible: true,
	}
}

// TestApprovalRequiresMatchingHeadSHA pins that a verdict is attributable to a
// TREE, not to "the PR". A verdict produced against an older head must not
// authorize a merge of code the reviewer never read — the same reasoning that
// makes the merge sweep re-verify the head SHA immediately before merging.
func TestApprovalRequiresMatchingHeadSHA(t *testing.T) {
	art := Artifact{Items: []Aggregate{approvedAggregate("hivecommons/hive", 42, "oldsha")}}

	if art.HasAggregateApproval("hivecommons/hive", 42, "newsha") {
		t.Fatal("an approval recorded against a different head SHA must not authorize the merge: " +
			"the reviewer never read this code")
	}
	// Positive control: the same approval DOES apply to the SHA it was made
	// against, proving the test above measures SHA matching rather than a
	// predicate that always returns false.
	if !art.HasAggregateApproval("hivecommons/hive", 42, "oldsha") {
		t.Fatal("an approval must apply to the head SHA it was produced against")
	}
}

// TestNonApproveVerdictsNeverAuthorizeMerge pins that every verdict OTHER than
// a unanimous approve withholds. Written as an exhaustive sweep rather than a
// spot check because the claim is about all verdicts.
func TestNonApproveVerdictsNeverAuthorizeMerge(t *testing.T) {
	for _, v := range []Verdict{VerdictChangesRequested, VerdictRequiresHuman, VerdictReject} {
		art := Artifact{Items: []Aggregate{{
			Repo: "hivecommons/hive", Number: 42, HeadSHA: "abc",
			Verdict: v,
			// Deliberately contradictory: MergeEligible is set true while the
			// verdict is non-approve. Even this inconsistent record must not
			// authorize a merge — the predicate requires BOTH.
			MergeEligible: true,
		}}}
		if art.HasAggregateApproval("hivecommons/hive", 42, "abc") {
			t.Errorf("verdict %q must not authorize a merge even when MergeEligible is set", v)
		}
	}
}

// TestMergeIneligibleApprovalDoesNotAuthorize pins the other half of the
// conjunction: an approve verdict that was not marked merge-eligible (e.g. it
// did not cover all required perspectives) must not authorize a merge either.
func TestMergeIneligibleApprovalDoesNotAuthorize(t *testing.T) {
	art := Artifact{Items: []Aggregate{{
		Repo: "hivecommons/hive", Number: 42, HeadSHA: "abc",
		Verdict: VerdictApprove, MergeEligible: false,
	}}}
	if art.HasAggregateApproval("hivecommons/hive", 42, "abc") {
		t.Fatal("an approve verdict that is not merge-eligible must not authorize the merge")
	}
}

// TestAbsentVerdictDoesNotAuthorizeMerge pins the fail-closed default: a PR
// with no verdict at all is not approved. Combined with the caller's
// `!reviewLoaded || !HasAggregateApproval(...) -> continue`, a missing or
// unreadable artifact withholds every merge rather than waving them through.
func TestAbsentVerdictDoesNotAuthorizeMerge(t *testing.T) {
	if (Artifact{}).HasAggregateApproval("hivecommons/hive", 42, "abc") {
		t.Fatal("an empty review artifact must not authorize any merge (fail closed)")
	}
}

// TestRejectCannotBeOverriddenByAnotherPerspective pins that a reject is
// terminal in aggregation: no number of approving perspectives can outvote it.
// This is the review-side analogue of the desk's ceiling ordering — an
// auto-approve cannot override a reject.
func TestRejectCannotBeOverriddenByAnotherPerspective(t *testing.T) {
	reports := []PerspectiveReport{
		{Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, Repo: "o/r", Number: 1},
		{Perspective: PerspectiveSecurity, Verdict: VerdictApprove, Repo: "o/r", Number: 1},
		{Perspective: PerspectiveStyle, Verdict: VerdictApprove, Repo: "o/r", Number: 1},
		{Perspective: PerspectiveDocsCurrency, Verdict: VerdictApprove, Repo: "o/r", Number: 1},
		{Perspective: PerspectiveIntentAlignment, Verdict: VerdictReject, Repo: "o/r", Number: 1},
	}
	agg := AggregateReports(reports, AggregateOptions{})
	if agg.Verdict != VerdictReject {
		t.Fatalf("a single reject must survive four approvals, got %q", agg.Verdict)
	}
	if agg.MergeEligible {
		t.Fatal("a rejected aggregate must never be merge-eligible")
	}
}

// TestUnanimousApprovalIsTheOnlyMergePath is the positive control for this
// whole file. If it failed, every test above would pass vacuously against an
// aggregator that never approves anything, and the withhold-only property would
// be meaningless because nothing would ever merge.
func TestUnanimousApprovalIsTheOnlyMergePath(t *testing.T) {
	var reports []PerspectiveReport
	for _, p := range DefaultPerspectives {
		reports = append(reports, PerspectiveReport{
			Perspective: p, Verdict: VerdictApprove, Repo: "o/r", Number: 1, HeadSHA: "abc",
		})
	}
	agg := AggregateReports(reports, AggregateOptions{})
	if agg.Verdict != VerdictApprove || !agg.MergeEligible {
		t.Fatalf("unanimous approval across all perspectives must be merge-eligible, got verdict=%q eligible=%v",
			agg.Verdict, agg.MergeEligible)
	}
}

// TestPartialPerspectiveCoverageIsNotApproval pins that silence is not consent:
// approving on a subset of perspectives does not produce a merge-eligible
// aggregate. A reviewer that simply failed to run on the security perspective
// must not thereby approve the change.
func TestPartialPerspectiveCoverageIsNotApproval(t *testing.T) {
	reports := []PerspectiveReport{
		{Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, Repo: "o/r", Number: 1},
	}
	agg := AggregateReports(reports, AggregateOptions{})
	if agg.MergeEligible {
		t.Fatal("approval on one perspective must not authorize a merge; missing perspectives are not approvals")
	}
}
