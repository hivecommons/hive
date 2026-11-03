package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A PR that passes every gate in a repo whose auto-merge is off gets a
// HandMerge verdict from the merge classifier. It must not sit in the
// Merge-eligible band ("nothing needed; it merges on its own") — no sweep
// takes that repo — and it is the operator's turn to merge it, Hive's own PRs
// included: the self-authored lane skips the repo too.
func TestRepoAutoMergeOffHandMerge(t *testing.T) {
	hand := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "auto-merge is off for this repo — a person merges it", HandMerge: true}
	eligible := &github.MergeVerdict{State: github.MergeVerdictEligible}
	draft := hivePR11565()
	draft.Draft = true

	cases := []struct {
		name    string
		pr      github.PullRequest
		verdict *github.MergeVerdict
		want    want11565
	}{
		{"Hive's own PR in an off repo", hivePR11565(), hand, handMerge12378},
		{"person's PR in an off repo", personPR11565(), hand, handMerge12378},
		{"draft stays out of the inbox", draft, hand, notYours11565},
		{"Hive's own PR in an on repo is the self-authored lane's", hivePR11565(), eligible, notYours11565},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := ownershipEnv11565(5)
			env.mergeLaneTakesAuthor = func(string, string) bool { return true }
			got := decidePROwnershipGated(tc.pr, tc.verdict, false, config.DashboardIssueBandsConfig{}, env)
			checkOwnership11565(t, got, tc.want)
			if tc.want == handMerge12378 && !strings.Contains(got.Reason, hand.Reason) {
				t.Errorf("reason %q does not carry the classifier's %q", got.Reason, hand.Reason)
			}
		})
	}

	pr := hivePR11565()
	// Not eligible (no sweep takes the repo) and not in-review either: the
	// verdict is outstanding for a repo switch, not for anyone's review.
	if band := prBand(pr, hand, false, config.DashboardIssueBandsConfig{}, ownershipNow11565, github.AutoMergeQueuedLabel, "").Band; band != "open" {
		t.Errorf("hand-merge PR band = %q, want open", band)
	}
	queued := hivePR11565(github.AutoMergeQueuedLabel)
	if band := prBand(queued, hand, false, config.DashboardIssueBandsConfig{}, ownershipNow11565, github.AutoMergeQueuedLabel, "").Band; band == "eligible" {
		t.Errorf("queued hand-merge PR band = %q: the queued sweep skips an off repo too", band)
	}
	if band := prBand(pr, eligible, false, config.DashboardIssueBandsConfig{}, ownershipNow11565, github.AutoMergeQueuedLabel, "").Band; band != "eligible" {
		t.Errorf("eligible PR band = %q, want eligible (unchanged)", band)
	}
}
