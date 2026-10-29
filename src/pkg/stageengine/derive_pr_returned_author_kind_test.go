package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15159: "Needs author" (071) is for a person's PR only.
// A clanker-authored PR that a human or the review swarm sends back is 072
// returned-to-contributor — the owning contributor's hive picks it up —
// whichever lane carried the verdict.
func TestClassifyStageChangesRequestedSplitsOnAuthorKind(t *testing.T) {
	cases := []struct {
		name       string
		lane       string
		reason     string
		detail     stageDetail
		kind       authorkind.Kind
		wantStage  stagelabels.Stage
		wantReason string
	}{
		{
			name:       "human CHANGES_REQUESTED on a human's PR → 071",
			lane:       stageLaneTrusted,
			reason:     autoMergeReasonHumanChangesRequested,
			detail:     stageDetail{HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead},
			kind:       authorkind.Human,
			wantStage:  stagelabels.NeedsAuthor,
			wantReason: autoMergeReasonHumanChangesRequested,
		},
		{
			name:       "human CHANGES_REQUESTED on a clanker's PR → 072",
			lane:       stageLaneTrusted,
			reason:     autoMergeReasonHumanChangesRequested,
			detail:     stageDetail{HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead},
			kind:       authorkind.Clanker,
			wantStage:  stagelabels.ReturnedToContributor,
			wantReason: autoMergeReasonHumanChangesRequested,
		},
		{
			name:       "swarm changes_requested on a human's PR → 071",
			lane:       stageLaneTrusted,
			reason:     autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested,
			kind:       authorkind.Human,
			wantStage:  stagelabels.NeedsAuthor,
			wantReason: stagelabels.LaneReasonChangesRequested,
		},
		{
			name:       "swarm changes_requested on a clanker's PR → 072",
			lane:       stageLaneContributor,
			reason:     autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested,
			kind:       authorkind.Clanker,
			wantStage:  stagelabels.ReturnedToContributor,
			wantReason: stagelabels.LaneReasonChangesRequested,
		},
		{
			name:       "unknown kind still reads as a person's PR",
			lane:       stageLaneTrusted,
			reason:     autoMergeReasonHumanChangesRequested,
			detail:     stageDetail{HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead},
			kind:       authorkind.Unknown,
			wantStage:  stagelabels.NeedsAuthor,
			wantReason: autoMergeReasonHumanChangesRequested,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyStage(stageObservation{Lane: tc.lane, Reason: tc.reason, HeadSHA: goldenHead, Detail: tc.detail, AuthorKind: tc.kind})
			if v.Stage != tc.wantStage || v.Reason != tc.wantReason {
				t.Fatalf("classified as %+v, want stage=%s reason=%q", v, tc.wantStage, tc.wantReason)
			}
		})
	}
}

// Through Derive: the same two facts differ only in the author kind the
// sink resolved, and land on 071 vs 072.
func TestDerivePRChangesRequestedRoutesByAuthorKind(t *testing.T) {
	for _, tc := range []struct {
		kind authorkind.Kind
		want stagelabels.Stage
	}{
		{authorkind.Human, stagelabels.NeedsAuthor},
		{authorkind.Clanker, stagelabels.ReturnedToContributor},
	} {
		f := Facts{
			Ref:    Ref{Owner: "o", Repo: "r", Number: 15159, Kind: stagelabels.PRItem},
			Open:   true,
			Author: AuthorFacts{Login: "thatguywob", Kind: tc.kind},
			PR: &PRFacts{
				HeadSHA: goldenHead,
				Review:  ReviewFacts{HumanChangeRequester: "clubanderson"},
				Lanes:   []LaneFact{{Lane: stageLaneTrusted, Reason: autoMergeReasonHumanChangesRequested}},
			},
		}
		d := derivePR(f)
		if d.Abstain || d.Stage != tc.want {
			t.Fatalf("kind %s: derived %+v, want %s", tc.kind, d, tc.want)
		}
	}
}

// hivecommons/hive#15159: the contributor lane parked a clanker PR on its
// sentinel alert (085) and kept re-asserting it over the owner's 072 after
// the owner's CHANGES_REQUESTED review. A trusted human's standing change
// request on the current head is the human the park waited for: the PR is
// the author's to fix. On another head it is not (per-head, like #15216's
// sentinel release), and the park stands.
func TestClassifyStageHumanChangeRequestWinsOverSentinelPark(t *testing.T) {
	reviewed := stageDetail{HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead}
	for _, reason := range []string{contributorLaneReasonSentinelBlocked, "sentinel", contributorLaneReasonWorkflowPath} {
		t.Run(reason, func(t *testing.T) {
			parked := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: reason, HeadSHA: goldenHead, Labels: []string{"sentinel-alert"}, AuthorKind: authorkind.Clanker})
			if parked.Stage != stagelabels.NeedsDecision || !parked.Human {
				t.Fatalf("no review: %+v, want 085", parked)
			}
			v := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: reason, HeadSHA: goldenHead, Labels: []string{"sentinel-alert"}, Detail: reviewed, AuthorKind: authorkind.Clanker})
			if v.Stage != stagelabels.ReturnedToContributor || v.Reason != autoMergeReasonHumanChangesRequested || v.Human {
				t.Fatalf("human changes_requested on the head: %+v, want 072 %s", v, autoMergeReasonHumanChangesRequested)
			}
			human := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: reason, HeadSHA: goldenHead, Labels: []string{"sentinel-alert"}, Detail: reviewed, AuthorKind: authorkind.Human})
			if human.Stage != stagelabels.NeedsAuthor {
				t.Fatalf("a person's PR goes to 071: %+v", human)
			}
			stale := ClassifyStage(stageObservation{Lane: stageLaneContributor, Reason: reason, HeadSHA: "newhead0000", Labels: []string{"sentinel-alert"}, Detail: reviewed, AuthorKind: authorkind.Clanker})
			if stale.Stage != stagelabels.NeedsDecision {
				t.Fatalf("a new head re-arms the park: %+v", stale)
			}
		})
	}
}
