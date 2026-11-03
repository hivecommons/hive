package dashboard

import "testing"

// TestStageTrackerMergeRefusal: GitHub's merge refusals become plain words
// and a kind token; the raw text (hivecommons/docs#512's "PUT https://…
// 405 4 of 4 required status checks are expected. []") never is the chip.
func TestStageTrackerMergeRefusal(t *testing.T) {
	for raw, want := range map[string]struct {
		kind, text string
		checks     int
	}{
		"PUT https://api.github.com/repos/hivecommons/docs/pulls/512/merge: 405 4 of 4 required status checks are expected. []": {MergeRefusalChecksPending, "4 required checks haven't finished", 4},
		"405 1 of 3 required status checks are expected.":                                                                       {MergeRefusalChecksPending, "1 required check hasn't finished", 1},
		`405 Required status check "lint" is expected.`:                                                                         {MergeRefusalChecksPending, "required check lint hasn't finished", 1},
		"PUT https://api.github.com/repos/o/r/pulls/1/merge: 405 Pull Request is not mergeable []":                              {MergeRefusalProtection, "branch protection blocks it", 0},
		"405 Base branch is protected; 2 rule violations found":                                                                 {MergeRefusalProtection, "branch protection blocks it", 0},
		"405 At least 1 approving review is required by reviewers with write access.":                                           {MergeRefusalReview, "branch protection wants an approving review hive can't give", 0},
		"405 Squash merges are not allowed on this repository.":                                                                 {MergeRefusalMethod, "the repository has this merge method switched off", 0},
		"405 Changes must be made through the merge queue":                                                                      {MergeRefusalMergeQueue, "this branch only merges through GitHub's merge queue", 0},
		"PUT https://api.github.com/repos/o/r/pulls/1/merge: 409 Head branch was modified. Review and try the merge again. []":  {MergeRefusalHeadMoved, "the branch moved under the merge call; it needs a rebase", 0},
		"PUT https://api.github.com/repos/o/r/pulls/1/merge: 403 Resource not accessible by integration []":                     {MergeRefusalForbidden, "hive isn't allowed to merge here", 0},
		"context deadline exceeded": {MergeRefusalOther, "GitHub refused the merge", 0},
	} {
		kind, text, checks := stageTrackerMergeRefusal(raw)
		if kind != want.kind || text != want.text || checks != want.checks {
			t.Errorf("%q → (%q, %q, %d), want (%q, %q, %d)", raw, kind, text, checks, want.kind, want.text, want.checks)
		}
	}
	if kind, text, checks := stageTrackerMergeRefusal("  "); kind != "" || text != "" || checks != 0 {
		t.Errorf("blank → (%q, %q, %d)", kind, text, checks)
	}
}
