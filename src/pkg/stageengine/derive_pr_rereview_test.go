package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/chairlift#799: a change request left on an older commit
// the author pushed past waits on the reviewer. It is a person's decision
// (085), never the author's 071 / 072, so no nudge goes out and no adoption
// clock runs against an author who did the work. A needs-rebase label is
// still the author's.
func TestClassifyStageReReviewRequestedWaitsOnTheReviewer(t *testing.T) {
	reason := prstage.ReReviewRequestedReason([]string{"Danathar"})
	for _, tc := range []struct {
		name   string
		lane   string
		kind   authorkind.Kind
		labels []string
		want   stagelabels.Stage
		human  bool
	}{
		{name: "clanker PR on the contributor lane", lane: stageLaneContributor, kind: authorkind.Clanker, want: stagelabels.NeedsDecision, human: true},
		{name: "human PR on the trusted-author lane", lane: stageLaneTrusted, kind: authorkind.Human, want: stagelabels.NeedsDecision, human: true},
		{name: "needs-rebase is still the author's", lane: stageLaneContributor, kind: authorkind.Clanker, labels: []string{"needs-rebase"}, want: stagelabels.ReturnedToContributor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyStage(stageObservation{Lane: tc.lane, Reason: reason, HeadSHA: goldenHead, AuthorKind: tc.kind, Labels: tc.labels})
			if v.Stage != tc.want || v.Human != tc.human {
				t.Fatalf("classified as %+v, want stage=%s human=%v", v, tc.want, tc.human)
			}
		})
	}
	// The head-scoped request keeps today's route: the author's turn.
	if v := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonChangesRequested, HeadSHA: goldenHead, AuthorKind: authorkind.Clanker}); v.Stage != stagelabels.ReturnedToContributor {
		t.Fatalf("changes requested on the head classified as %+v, want 072", v)
	}
}
