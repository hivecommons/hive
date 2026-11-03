package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A lane that reports `unauthorized:<step>` — the hive's GitHub App lacks
// the permission the next step needs (hivecommons/hive#14589) — parks the
// PR at 085 with the step kept in the reason, so the entry comment and the
// tracker's gate cell can name the human step owed.
func TestClassifyStageUnauthorizedParksForMaintainer(t *testing.T) {
	reason := prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns
	cases := []struct {
		name  string
		obs   stageObservation
		want  stagelabels.Stage
		human bool
	}{
		{name: "contributor lane", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: reason, AuthorKind: authorkind.Clanker}, want: stagelabels.NeedsDecision, human: true},
		{name: "contributor lane human author", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: reason, AuthorKind: authorkind.Human}, want: stagelabels.NeedsDecision, human: true},
		{name: "other step", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: prstage.StageReasonUnauthorizedPrefix + "update-branch", AuthorKind: authorkind.Clanker}, want: stagelabels.NeedsDecision, human: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyStage(tc.obs)
			if got.Stage != tc.want || got.Rank != stageRankOwned || got.Human != tc.human {
				t.Fatalf("ClassifyStage=%+v want owned stage %v human=%v", got, tc.want, tc.human)
			}
			if got.Reason != "" && got.Reason != tc.obs.Reason {
				t.Fatalf("ClassifyStage reason = %q, want the lane's %q kept", got.Reason, tc.obs.Reason)
			}
		})
	}
}
