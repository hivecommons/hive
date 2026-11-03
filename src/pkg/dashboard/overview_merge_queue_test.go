package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A PR the classifier marks as a merge-queue-required hand merge must leave
// the Overview's Merge-eligible count and land in the person's inbox, like a
// PR in a repo whose auto-merge is off (projectbluefin/bluefin-bling#93 and
// #113, 2026-10-25).
func TestOverviewMergeQueueRequiredLeavesEligibleBand(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	now := mustTime9102(t, "2026-10-25T18:00:00Z")
	queue := &github.MergeVerdict{
		State:     github.MergeVerdictOutstanding,
		Reason:    "merge-queue-required: main only accepts merges through GitHub's merge queue, and Hive merges over REST and cannot enqueue — a person merges it through the merge queue, or the queue rule is removed",
		HandMerge: true,
	}
	eligible := &github.MergeVerdict{State: github.MergeVerdictEligible, Reason: "the sweep would merge this now"}
	pr := func(n int) github.PullRequest {
		return github.PullRequest{Number: n, BaseRef: "main", Title: "bump", Author: "renovate[bot]", Labels: []string{}, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success", UpdatedAt: now, CreatedAt: now}
	}
	status := &StatusPayload{HiveID: "h1", Repos: []FrontendRepo{{
		Full: "org/bling",
		OpenPrs: []any{
			FrontendPR{PullRequest: pr(93), MergeVerdict: queue},
			FrontendPR{PullRequest: pr(113), MergeVerdict: queue},
			FrontendPR{PullRequest: pr(120), MergeVerdict: eligible},
		},
	}}}

	_, specs := s.overviewPRRows(status, s.issueBandsConfig(), overviewFilters{}, now)
	counts := map[string]int{}
	for _, spec := range specs {
		counts[spec.Key] = spec.Count
	}
	if counts["eligible"] != 1 {
		t.Errorf("Merge-eligible count = %d, want 1 (only #120); counts = %v", counts["eligible"], counts)
	}
	for _, item := range s.overviewPRItems(status, s.issueBandsConfig(), overviewFilters{}, now) {
		if item.pr.Number == 120 {
			continue
		}
		if item.info.Band == "eligible" {
			t.Errorf("#%d band = eligible, want it out of Merge-eligible", item.pr.Number)
		}
	}

	env := ownershipEnv11565(5)
	env.mergeLaneTakesAuthor = func(string, string) bool { return true }
	got := decidePROwnershipGated(pr(93), queue, false, config.DashboardIssueBandsConfig{}, env)
	if got.Reason == "" || !strings.Contains(got.Reason, "merge-queue-required") {
		t.Errorf("ownership = %+v, want the person's turn with the merge-queue-required reason", got)
	}
	checkOwnership11565(t, got, handMerge12378)
}
