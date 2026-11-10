package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#17565: on a repo whose auto-merge is off nothing merges
// the head, so a lane observation under "repo-auto-merge-disabled" is
// classified from the head's evidence instead of being dropped as a
// hand-off: never 080, 095 or 100, and approved and green is the
// "090 · auto-merge off" human merge.
func TestClassifyStageRepoAutoMergeDisabledFromHeadEvidence(t *testing.T) {
	const reason = "repo-auto-merge-disabled"
	approve := ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	for _, tc := range []struct {
		name      string
		labels    []string
		detail    stageDetail
		want      stagelabels.Stage
		wantHuman bool
	}{
		{name: "no verdict", want: stagelabels.ReviewPending},
		{name: "approve and green", detail: stageDetail{VerdictKnown: true, Verdict: approve}, want: stagelabels.ReadyForHumanMerge, wantHuman: true},
		{name: "approve and red", detail: stageDetail{VerdictKnown: true, Verdict: approve, RedChecks: []string{"unit"}, RedChecksHead: goldenHead}, want: stagelabels.Fixing},
		{name: "changes requested", detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}, want: stagelabels.Fixing},
		{name: "requires human", detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}}, want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "decision requested", detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve", DecisionRequested: true}}, want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "needs-rebase label", labels: []string{"needs-rebase"}, detail: stageDetail{VerdictKnown: true, Verdict: approve}, want: stagelabels.Fixing},
		{name: "sentinel alert", detail: stageDetail{VerdictKnown: true, Verdict: approve, SentinelAlert: true}, want: stagelabels.NeedsDecision, wantHuman: true},
		{name: "hold label", labels: []string{"hold"}, detail: stageDetail{VerdictKnown: true, Verdict: approve}, want: stagelabels.NeedsDecision, wantHuman: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, lane := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor} {
				obs := stageObservation{Lane: lane, HeadSHA: goldenHead, Reason: reason, Labels: tc.labels, Detail: tc.detail}
				got := ClassifyStage(obs)
				want := tc.want
				if want == stagelabels.Fixing {
					want = FixingStage(stageObservation{Lane: lane})
				}
				if got.Stage != want || got.Rank != stageRankOwned || got.Human != tc.wantHuman || got.Terminal {
					t.Fatalf("%s lane: got %+v, want stage %v rank owned human=%v", lane, got, want, tc.wantHuman)
				}
				if want == stagelabels.ReadyForHumanMerge {
					recorded := got.Reason
					if recorded == "" {
						recorded = obs.Reason
					}
					if hm := prstage.HumanMergeReason(recorded); hm != prstage.HumanMergeReasonAutoMergeOff {
						t.Fatalf("%s lane: 090 human-merge reason %q (recorded %q), want %q", lane, hm, recorded, prstage.HumanMergeReasonAutoMergeOff)
					}
				}
			}
		})
	}
}
