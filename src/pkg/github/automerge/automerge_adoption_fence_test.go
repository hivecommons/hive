package automerge

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestCarriedFenceMarkerRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "tests removed", body: CarriedFenceMarker(contributorLaneReasonTestsRemoved, 13212) + "\nSupersedes #13212.", want: contributorLaneReasonTestsRemoved},
		{name: "workflow path, no origin", body: "intro\n" + CarriedFenceMarker(contributorLaneReasonWorkflowPath, 0), want: contributorLaneReasonWorkflowPath},
		{name: "no marker", body: "Supersedes #7 (adopted).", want: ""},
		{name: "unknown reason ignored", body: CarriedFenceMarker("contributor-lane:no-claim", 7), want: ""},
		{name: "deferral verdict is not a fence", body: CarriedFenceMarker(adoptionReasonHeadChanged, 7), want: ""},
		{name: "empty body", body: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriedFenceReason(tc.body); got != tc.want {
				t.Fatalf("carriedFenceReason(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
	if got := CarriedFenceMarker(contributorLaneReasonTestsRemoved, 13212); got != "<!-- hive:carried-fence reason=contributor-lane:tests-removed from=#13212 -->" {
		t.Fatalf("marker = %q", got)
	}
}

func TestAdoptionFenceDefersOnlyUnreadableVerdicts(t *testing.T) {
	for reason, want := range map[string]bool{
		adoptionReasonHeadChanged:          true,
		contributorLaneReasonFilesEvidence: true,
		contributorLaneReasonTestsRemoved:  false,
		contributorLaneReasonWorkflowPath:  false,
	} {
		if got := adoptionFenceDefers(reason); got != want {
			t.Fatalf("adoptionFenceDefers(%q) = %v, want %v", reason, got, want)
		}
	}
}

// A green App PR whose body carries a diff fence from the contributor PR it
// superseded is never merged by the self-authored lane: the reason is the
// carried contributor-lane fence (a stage-085 reason), no PUT .../merge
// happens, and a body without the marker merges as before.
func TestSelfAuthoredSweepCarriedFenceHoldsMerge(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantReason string
		wantMerges int
	}{
		{name: "tests-removed carried", body: adoptionPRBodyWithFence(contributorLaneReasonTestsRemoved), wantReason: contributorLaneReasonTestsRemoved},
		{name: "workflow-path carried", body: CarriedFenceMarker(contributorLaneReasonWorkflowPath, 7) + "\nSupersedes #7.", wantReason: contributorLaneReasonWorkflowPath},
		{name: "no marker merges", body: "Supersedes #7 (adopted after CI went red).", wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{body: tc.body, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			for tick := 0; tick < 2; tick++ {
				event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
				if err != nil {
					t.Fatalf("tick %d: err = %v", tick, err)
				}
				if reason != tc.wantReason {
					t.Fatalf("tick %d: reason = %q, want %q", tick, reason, tc.wantReason)
				}
				if tc.wantReason != "" && event.MergeSHA != "" {
					t.Fatalf("tick %d: fenced PR merged: %+v", tick, event)
				}
			}
			if tc.wantMerges == 0 && merges != 0 {
				t.Fatalf("merges = %d, want 0", merges)
			}
			if tc.wantMerges > 0 && merges == 0 {
				t.Fatal("unfenced PR did not merge")
			}
		})
	}
}

// adoptionPRBodyWithFence renders a superseding PR body carrying fence, as
// supersedeAdoptedPR would for the adopted contributor PR #7.
func adoptionPRBodyWithFence(fence string) string {
	obs := stageObservation{Owner: "acme", Repo: "widget", Display: "acme/widget", Number: 7, HeadSHA: "aaa", Author: "mallory", BaseRef: "v5", Title: "Fix the widget"}
	return AdoptionPRBody(obs, stagelabels.Record{AdoptReason: AdoptReasonOffline, AdoptDiffFence: fence}, false, "")
}
