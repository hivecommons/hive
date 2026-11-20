package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// GitHub reports mergeable_state "unknown" while it recomputes a merge, and
// the contributor lane stops there before admission, review or CI. That pass
// says nothing about the stage: projectbluefin/utah-packages#1222 (no linked
// issue, approved, green) read it as a weak 080 and dropped 090 → 080 → 090.
func approvedMergeableUnknownObservation(lane string) stageObservation {
	return stageObservation{Lane: lane, HeadSHA: "1bb4e6bf", Reason: prstage.StageReasonMergeableUnknown, Detail: stageDetail{
		VerdictKnown: true,
		Verdict:      ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"},
	}}
}

func TestClassifyStageMergeableUnknownIsNoOpinion(t *testing.T) {
	for _, o := range []stageObservation{
		{Lane: stageLaneContributor, HeadSHA: "1bb4e6bf", Reason: prstage.StageReasonMergeableUnknown},
		approvedMergeableUnknownObservation(stageLaneContributor),
	} {
		if v := ClassifyStage(o); v.Rank != stageRankNone || v.Stage != stagelabels.None || v.Terminal {
			t.Fatalf("ClassifyStage(%q, verdict known=%v) = %+v, want no opinion", o.Reason, o.Detail.VerdictKnown, v)
		}
	}
}

// With the contributor lane silent, the trusted-author lane's weak
// untrusted-author-role 090 candidate does not stand in for it either (a
// conflicting PR at 072 would read 090 for one pass). The reconciler's
// DerivePR path agrees and abstains. An owned opinion from another lane
// still applies.
func TestResolveStageMergeableUnknownKeepsTheStage(t *testing.T) {
	unknown := approvedMergeableUnknownObservation(stageLaneContributor)
	weak := stageObservation{Lane: stageLaneTrusted, HeadSHA: unknown.HeadSHA, Reason: "untrusted-author-role", Detail: unknown.Detail}
	if v := ClassifyStage(weak); v.Rank == stageRankNone || v.Rank >= stageRankOwned {
		t.Fatalf("precondition: untrusted-author-role classified %+v, want a weak opinion", v)
	}
	for name, list := range map[string][]stageObservation{
		"contributor lane only":     {unknown},
		"with a weak trust opinion": {weak, unknown},
	} {
		t.Run(name, func(t *testing.T) {
			if v, _, _ := ResolveStage(list); v.Rank != stageRankNone {
				t.Fatalf("ResolveStage = %+v, want no opinion", v)
			}
			if d := derivePR(FactsFromPRObservations(list)); !d.Abstain || d.Reason != prstage.StageReasonMergeableUnknown {
				t.Fatalf("derivePR = %+v, want abstain on %q", d, prstage.StageReasonMergeableUnknown)
			}
		})
	}
	owned := stageObservation{Lane: stageLaneTrusted, HeadSHA: unknown.HeadSHA, Reason: "required-check-failing:build", Detail: unknown.Detail}
	if v, _, _ := ResolveStage([]stageObservation{owned, unknown}); v.Rank != stageRankOwned || v.Stage != FixingStage(owned) {
		t.Fatalf("owned opinion with an unknown pass = %+v, want owned %v", v, FixingStage(owned))
	}
}
