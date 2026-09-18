package review

import "testing"

func approveReport(p Perspective) PerspectiveReport {
	r := PerspectiveReport{Perspective: p, Verdict: VerdictApprove, Repo: "o/r", Number: 1}
	r.Kind = "review"
	return r
}

// max_perspectives_per_pr caps how many perspectives a PR is ever given. A
// perspective that is never dispatched cannot approve, so requiring all five
// made approve unreachable: at a cap of 1 every PR aggregated to
// requires_human with "did not unanimously approve", merge_eligible was never
// true, and a hive running require_approval could never clear anything.
//
// Observed live on a bluefin spoke with max_perspectives_per_pr: 1 — all 47
// pending reviews were the correctness perspective, and both recorded verdicts
// came back requires_human off a clean approve.
func TestCappedHiveCanReachUnanimousApprove(t *testing.T) {
	reports := []PerspectiveReport{approveReport(PerspectiveCorrectness)}

	capped := AggregateReports(reports, AggregateOptions{MaxPerspectivesPerPR: 1})
	if capped.Verdict != VerdictApprove || !capped.MergeEligible {
		t.Fatalf("capped hive cannot reach approve: verdict=%s merge_eligible=%v reasons=%v",
			capped.Verdict, capped.MergeEligible, capped.Reasons)
	}

	// With no cap the full set is still required: a single approve out of five
	// is not unanimity, and must not become one.
	uncapped := AggregateReports(reports, AggregateOptions{})
	if uncapped.Verdict != VerdictRequiresHuman || uncapped.MergeEligible {
		t.Fatalf("uncapped hive approved on one perspective of %d: %+v", len(DefaultPerspectives), uncapped)
	}
}

// The cap raises no PR above its own evidence: a non-approving perspective
// still blocks, and a cap larger than the reports collected still waits.
func TestCappedUnanimityStillRequiresEveryReportToApprove(t *testing.T) {
	t.Run("dissent blocks", func(t *testing.T) {
		reports := []PerspectiveReport{approveReport(PerspectiveCorrectness), {
			Perspective: PerspectiveSecurity, Verdict: VerdictRequiresHuman, Repo: "o/r", Number: 1,
		}}
		got := AggregateReports(reports, AggregateOptions{MaxPerspectivesPerPR: 2})
		if got.Verdict != VerdictRequiresHuman || got.MergeEligible {
			t.Fatalf("a dissenting perspective did not block: %+v", got)
		}
	})

	t.Run("fewer reports than the cap waits", func(t *testing.T) {
		reports := []PerspectiveReport{approveReport(PerspectiveCorrectness)}
		got := AggregateReports(reports, AggregateOptions{MaxPerspectivesPerPR: 3})
		if got.Verdict == VerdictApprove || got.MergeEligible {
			t.Fatalf("approved before the capped perspectives had all reported: %+v", got)
		}
	})

	t.Run("cap above the default set requires the default set", func(t *testing.T) {
		reports := []PerspectiveReport{approveReport(PerspectiveCorrectness)}
		got := AggregateReports(reports, AggregateOptions{MaxPerspectivesPerPR: 99})
		if got.Verdict == VerdictApprove {
			t.Fatalf("an oversized cap lowered the bar below DefaultPerspectives: %+v", got)
		}
	})

	t.Run("full set still approves", func(t *testing.T) {
		var reports []PerspectiveReport
		for _, p := range DefaultPerspectives {
			reports = append(reports, approveReport(p))
		}
		got := AggregateReports(reports, AggregateOptions{})
		if got.Verdict != VerdictApprove || !got.MergeEligible {
			t.Fatalf("full unanimous approval regressed: %+v", got)
		}
	})
}

// hasAllRequiredPerspectives is currently only reached when the caller has
// already established that every verdict is approve, so its per-entry check is
// defensive. Test it directly: otherwise the defense is dead code that could be
// deleted without any test noticing, and merge eligibility is the last place to
// rely on an invariant held somewhere else.
func TestHasAllRequiredPerspectivesRejectsNonApprove(t *testing.T) {
	for name, tc := range map[string]struct {
		got    map[Perspective]Verdict
		maxPer int
		want   bool
	}{
		"single approve under cap 1": {map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove}, 1, true},
		"single reject under cap 1":  {map[Perspective]Verdict{PerspectiveCorrectness: VerdictReject}, 1, false},
		"one of two approves":        {map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove, PerspectiveSecurity: VerdictChangesRequested}, 2, false},
		"empty never approves":       {map[Perspective]Verdict{}, 1, false},
		"empty with no cap":          {map[Perspective]Verdict{}, 0, false},
	} {
		if got := hasAllRequiredPerspectives(tc.got, tc.maxPer); got != tc.want {
			t.Errorf("%s: hasAllRequiredPerspectives = %v, want %v", name, got, tc.want)
		}
	}
}
