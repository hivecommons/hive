package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A `blocked` PR (label:blocked: held on its Depends-on targets, or by a
// person) is a gate like a hold, not a stage: every lane classifies it as
// a human hand-off on the head's own progress, never the merge queue, and
// it classifies exactly as label:hold does.
func TestClassifyStageBlockedIsAHeldGate(t *testing.T) {
	for _, lane := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor, "queued"} {
		blocked := ClassifyStage(stageObservation{Lane: lane, Reason: "label:blocked", HeadSHA: "abc1234"})
		held := ClassifyStage(stageObservation{Lane: lane, Reason: "label:hold", HeadSHA: "abc1234"})
		if blocked != held {
			t.Fatalf("%s: label:blocked = %+v, want it classified like label:hold %+v", lane, blocked, held)
		}
		if blocked.Stage == stagelabels.MergeQueue || blocked.Stage == stagelabels.AwaitingMergeAdmission || blocked.Stage == stagelabels.PreMergeSoak {
			t.Fatalf("%s: label:blocked = %+v, want it short of the merge queue", lane, blocked)
		}
	}
	if v := ClassifyStage(stageObservation{Lane: stageLaneSelf, Reason: "label:blocked", Labels: []string{"needs-rebase"}, HeadSHA: "abc1234"}); v.Stage != stagelabels.Fixing {
		t.Fatalf("needs-rebase still sends a blocked PR to its fixer first: %+v", v)
	}
}
