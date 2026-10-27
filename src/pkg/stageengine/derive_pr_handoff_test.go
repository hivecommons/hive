package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14482: the trusted-author tier used to hand the App's
// own PRs off under the contributor lane's reason, so a trusted-author
// observation read as if the contributor lane had routed the PR. Each tier
// now hands off under its own reason; both are no-opinion hand-offs and the
// self-authored lane's review-gate hold owns the stage.
func TestClassifyStageAppAuthoredHandoffsAreNoOpinion(t *testing.T) {
	for _, tc := range []struct {
		lane, reason string
	}{
		{stageLaneTrusted, trustedAuthorReasonAppAuthored},
		{stageLaneContributor, contributorLaneReasonAppAuthored},
	} {
		v := ClassifyStage(stageObservation{Lane: tc.lane, Reason: tc.reason, HeadSHA: goldenHead})
		if v.Rank != stageRankNone || v.Stage != stagelabels.None || v.Terminal {
			t.Fatalf("%s %q classified as %+v, want no opinion", tc.lane, tc.reason, v)
		}
	}
}

func TestResolveStageAppAuthoredHandoffsAttributeToSelfLane(t *testing.T) {
	list := []stageObservation{
		{Lane: stageLaneSelf, Reason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeNoBundle, HeadSHA: goldenHead, Detail: stageDetail{Role: stagelabels.RoleTrusted}},
		{Lane: stageLaneTrusted, Reason: trustedAuthorReasonAppAuthored, HeadSHA: goldenHead},
		{Lane: stageLaneContributor, Reason: contributorLaneReasonAppAuthored, HeadSHA: goldenHead},
	}
	v, role, obs := ResolveStage(list)
	if v.Stage != stagelabels.ReviewPending || v.Rank != stageRankOwned {
		t.Fatalf("stage = %v rank %d, want owned review-pending", v.Stage, v.Rank)
	}
	if role != stagelabels.RoleTrusted {
		t.Fatalf("role = %v, want trusted", role)
	}
	if obs.Lane != stageLaneSelf {
		t.Fatalf("attributed to lane %q, want %q", obs.Lane, stageLaneSelf)
	}
}
