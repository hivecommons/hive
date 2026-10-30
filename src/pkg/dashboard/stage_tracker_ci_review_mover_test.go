package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Owner rule (hivecommons/hive#15640): "we dont want to create a log jam
// during pr in progress". Every CI/Review row names who is moving it — CI,
// the review swarm, the hive, or the author — never a maintainer and never
// nobody, whatever lane reason the record carries.
func TestStageTrackerCIReviewCaptionAlwaysNamesAMover(t *testing.T) {
	stages := []stagelabels.Stage{stagelabels.ReviewPending, stagelabels.Reviewed, stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor, stagelabels.AdoptedByHive}
	reasons := []string{"", "contributor-lane:no-verdict", "contributor-lane:claim-tier-untrusted", "check-pending", "check-failing:unit", "conflicting", "contributor-lane:changes-requested", "something-unmapped"}
	for _, stage := range stages {
		for _, reason := range reasons {
			caption, actor := stageTrackerCaption(stage, reason, "odttlnt", "pr", false, stagelabels.Record{}, false, 0, "036bff04036bff04036bff04036bff04036bff04")
			if caption == "" || actor == "" {
				t.Errorf("%v %q: caption=%q actor=%q — a CI/Review row with no mover", stage, reason, caption, actor)
			}
			if actor == "maintainer" {
				t.Errorf("%v %q: actor is the maintainer during CI/Review (%q)", stage, reason, caption)
			}
		}
	}
}
