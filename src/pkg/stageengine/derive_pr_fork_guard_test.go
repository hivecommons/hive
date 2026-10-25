package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// 2026-10-25: a fork PR whose only red check was a fork-publication guard
// ("a fork pull request cannot publish the canary image; a maintainer must
// push this branch to the repository") went to 072 and its contributor was
// nudged to fix what only a maintainer can (hivecommons/hive#14273). A red
// PR whose every failing check is such a guard parks at 085 with the checks
// named; any other red stays author-facing.
func TestClassifyStageForkGuardParksForMaintainer(t *testing.T) {
	approve := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	reject := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}, VerdictKnown: true}
	redGuard := approve
	redGuard.RedChecks, redGuard.RedChecksHead = []string{"Canary"}, "sha"
	redOther := approve
	redOther.RedChecks, redOther.RedChecksHead = []string{"Canary", "unit"}, "sha"
	guard := []string{"Canary"}
	const want = "fork-guard:Canary"
	cases := []struct {
		name   string
		obs    stageObservation
		want   stagelabels.Stage
		reason string
		human  bool
	}{
		{name: "fork required guard check", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:Canary", AuthorKind: authorkind.Clanker, ForkGuardChecks: guard}, want: stagelabels.NeedsDecision, reason: want, human: true},
		{name: "fork optional guard check while soaking", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h)", AuthorKind: authorkind.Clanker, Detail: redGuard, ForkGuardChecks: guard}, want: stagelabels.NeedsDecision, reason: want, human: true},
		{name: "fork guard on lane error", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, AuthorKind: authorkind.Clanker, Detail: redGuard, ForkGuardChecks: guard}, want: stagelabels.NeedsDecision, reason: want, human: true},
		{name: "same-repo PR with the guard message is red CI", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:Canary", AuthorKind: authorkind.Clanker}, want: stagelabels.ReturnedToContributor},
		{name: "fork with unrelated red check is red CI", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:unit", AuthorKind: authorkind.Clanker}, want: stagelabels.ReturnedToContributor},
		{name: "fork with another red optional check is red CI", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonSoaking + "(1h)", AuthorKind: authorkind.Clanker, Detail: redOther, ForkGuardChecks: guard}, want: stagelabels.ReturnedToContributor, reason: "optional-check-failing:Canary"},
		{name: "changes requested stays author-facing", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-failing:Canary", AuthorKind: authorkind.Clanker, Detail: reject, ForkGuardChecks: guard}, want: stagelabels.ReturnedToContributor, reason: "reviewer-verdict:changes_requested"},
		{name: "pending CI is unchanged", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "required-check-pending:build", AuthorKind: authorkind.Clanker, ForkGuardChecks: guard}, want: stagelabels.ReviewPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyStage(tc.obs)
			if got.Stage != tc.want || got.Reason != tc.reason || got.Rank != stageRankOwned || got.Human != tc.human {
				t.Fatalf("ClassifyStage=%+v want owned stage %v reason %q human=%v", got, tc.want, tc.reason, tc.human)
			}
		})
	}
}

// The fork-guard evidence survives the Facts round trip, so DerivePR parks
// the PR exactly as the legacy pass does.
func TestForkGuardChecksRoundTripFacts(t *testing.T) {
	obs := stageObservation{Lane: stageLaneContributor, Owner: "o", Repo: "r", Number: 609, HeadSHA: "sha", Reason: "required-check-failing:Canary", AuthorKind: authorkind.Clanker, ForkGuardChecks: []string{"Canary"}}
	f := FactsFromPRObservations([]stageObservation{obs})
	if got := f.PR.ForkGuardChecks; len(got) != 1 || got[0] != "Canary" {
		t.Fatalf("PRFacts.ForkGuardChecks = %v", got)
	}
	back := ObservationsFromPRFacts(f)
	if len(back) != 1 {
		t.Fatalf("observations = %+v", back)
	}
	if v := ClassifyStage(back[0]); v.Stage != stagelabels.NeedsDecision || v.Reason != "fork-guard:Canary" {
		t.Fatalf("ClassifyStage after round trip = %+v", v)
	}
}
