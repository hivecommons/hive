package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// prs_by_band on the projectbluefin hive, 2026-11-05 20:53Z: PRs that need a
// person sat in Merge-eligible ("nothing needed; it merges on its own") or
// in-review with no reason. The shapes below are those rows; the verdicts
// are what the classifier writes for them.
func TestBandAndOwnershipForPRsThatNeedAPerson(t *testing.T) {
	const queued = "lgtm"
	offReason := "auto-merge is off for this repo (its auto-merge setting, or the hive is below ACMM L6) — a person merges it"
	person := func(n int, labels ...string) github.PullRequest {
		pr := personPR11565(labels...)
		pr.Number = n
		pr.Mergeable = github.MergeableYes
		return pr
	}
	// projectbluefin/server#583: queued, a non-required check red, verdict
	// outstanding; in an auto-merge-off repo the classifier marks it a
	// hand merge.
	pr583 := person(583, queued)
	pr583.CIStatus, pr583.FailingChecks = "failing", []string{"prow / prow"}
	v583 := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "CI failing: prow / prow; " + offReason, HandMerge: true}
	// projectbluefin/server#613: queued, GitHub refuses the merge (a
	// required check red, mergeable no).
	pr613 := person(613, queued)
	pr613.Mergeable, pr613.MergeableState = github.MergeableNo, "blocked"
	pr613.CIStatus, pr613.FailingChecks = "failing", []string{"zizmor"}
	v613 := &github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: "blocked — CI failing: zizmor"}
	// projectbluefin/documentation#1668: green, the contributor lane
	// refuses the relay, auto-merge off.
	pr1668 := person(1668)
	v1668 := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "contributor-lane policy refuses the merge relay: contributor-lane:no-linked-issue; " + offReason, HandMerge: true}
	// projectbluefin/actions#697: approved, a non-required check red,
	// auto-merge off.
	pr697 := person(697)
	pr697.CIStatus, pr697.FailingChecks = "failing", []string{"Check consumer validation evidence"}
	v697 := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "CI failing: Check consumer validation evidence; " + offReason, HandMerge: true}
	// projectbluefin/actions#723: every gate passed, auto-merge off (already
	// right before this change).
	pr723 := person(723)
	v723 := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: offReason, HandMerge: true}
	// Queued with no verdict yet, and a plain eligible verdict: unchanged.
	prQueuedOnly := person(1, queued)
	prEligible := person(2)
	vEligible := &github.MergeVerdict{State: github.MergeVerdictEligible, Reason: "the sweep would merge this now"}
	// Queued, outstanding for a Hive-side reason, auto-merge on: amber, not
	// eligible.
	prQueuedOutstanding := person(3, queued)
	vOutstanding := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "soaking"}

	// Queued with no verdict yet, but CI red: blocked, not eligible. An
	// eligible verdict on a PR GitHub reports unmergeable stays eligible:
	// the sweep merges past a non-required GitHub blocker, and its verdict
	// is the authority.
	prQueuedRed := person(4, queued)
	prQueuedRed.CIStatus, prQueuedRed.FailingChecks = "failing", []string{"build"}
	prStaleEligible := person(5)
	prStaleEligible.Mergeable, prStaleEligible.MergeableState = github.MergeableNo, "blocked"

	cases := []struct {
		name     string
		pr       github.PullRequest
		verdict  *github.MergeVerdict
		band     string
		want     want11565
		reasonIn []string
	}{
		{"server#583 queued, red optional check, auto-merge off", pr583, v583, "blocked", handMerge12378, []string{"prow / prow", "auto-merge is off"}},
		{"server#613 queued, GitHub blocks the merge", pr613, v613, "blocked", notYours11565, nil},
		{"documentation#1668 lane refusal, auto-merge off", pr1668, v1668, "open", handMerge12378, []string{"no-linked-issue", "auto-merge is off"}},
		{"actions#697 red optional check, auto-merge off", pr697, v697, "blocked", handMerge12378, []string{"Check consumer validation evidence", "auto-merge is off"}},
		{"actions#723 every gate passed, auto-merge off", pr723, v723, "open", handMerge12378, []string{"auto-merge is off"}},
		{"queued with no verdict yet", prQueuedOnly, nil, "eligible", notYours11565, nil},
		{"eligible verdict", prEligible, vEligible, "eligible", notYours11565, nil},
		{"queued but outstanding, auto-merge on", prQueuedOutstanding, vOutstanding, "in-review", notYours11565, nil},
		{"queued with no verdict yet, CI red", prQueuedRed, nil, "blocked", notYours11565, nil},
		{"eligible verdict, GitHub says not mergeable", prStaleEligible, vEligible, "eligible", notYours11565, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := prBand(tc.pr, tc.verdict, false, config.DashboardIssueBandsConfig{}, ownershipNow11565, queued, "")
			if info.Band != tc.band {
				t.Errorf("band = %q, want %q", info.Band, tc.band)
			}
			if tc.verdict != nil && tc.verdict.State != github.MergeVerdictEligible && info.Band == "eligible" {
				t.Errorf("a %s verdict must never be Merge-eligible", tc.verdict.State)
			}
			env := ownershipEnv11565(5)
			env.mergeLaneTakesAuthor = func(string, string) bool { return true }
			got := decidePROwnershipGated(tc.pr, tc.verdict, false, config.DashboardIssueBandsConfig{}, env)
			checkOwnership11565(t, got, tc.want)
			for _, w := range tc.reasonIn {
				if !strings.Contains(got.Reason, w) {
					t.Errorf("reason %q lacks %q", got.Reason, w)
				}
			}
		})
	}
}
