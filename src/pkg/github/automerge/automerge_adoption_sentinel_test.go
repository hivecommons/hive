package automerge

import (
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The Sentinel sweep judges a superseding PR by the adopted PR's author,
// which it reads from this body (hgithub.AdoptedFrom). Every supersede path
// must keep writing a line it can read, or the App's copy of a flagged head
// goes back to a trusted-author notice.
func TestAdoptionPRBodyNamesTheAdoptedAuthorForSentinel(t *testing.T) {
	obs := stageObservation{Number: 930, HeadSHA: "44428f31f051", Author: "kylerankin", BaseRef: "main", Display: "o/r", Body: "Closes #918", Reason: "check-failing:unit"}
	for _, reason := range []string{AdoptReasonGrace, AdoptReasonOffline, AdoptReasonUnregistered, AdoptReasonDCORehome, AdoptReasonSignoffRehome, AdoptReasonInPlaceStalled} {
		for _, fence := range []string{"", contributorLaneReasonWorkflowPath} {
			for _, dco := range []bool{false, true} {
				body := AdoptionPRBody(obs, stagelabels.Record{AdoptReason: reason, AdoptDiffFence: fence}, dco, "")
				n, author, ok := hgithub.AdoptedFrom(body)
				if !ok || n != 930 || author != "kylerankin" {
					t.Fatalf("reason=%s fence=%q dco=%v: AdoptedFrom = (%d, %q, %v), want (930, kylerankin):\n%s", reason, fence, dco, n, author, ok, body)
				}
			}
		}
	}
}
