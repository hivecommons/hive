package automerge

import "strings"

// MergeLaneTakesAuthor reports whether one of Hive's merge lanes admits pull
// requests by author in repo, using the author gates the sweeps apply: the
// App's own PRs and allow-listed bots (the self-authored lane), an author the
// trusted-author tier authorizes, or any author on a repository the
// contributor lane serves (that lane's per-PR claim and tier checks need
// GitHub, so they stay with the sweep and the lane counts as taking the
// author). False is the gate the sweeps log as not-app-authored or
// untrusted-author-role: a merge-ready PR waits for a person to merge it by
// hand, which the dashboard's your-turn decision reads
// (hivecommons/hive#12378).
//
// The sweeps key their repo allow-lists by the configured repository name,
// which may be bare (same-org) or owner/repo; the dashboard passes a PR's
// owner/repo. The repo is checked under both spellings so a restricted
// allow-list matches the same repositories here as in the sweep.
func (c *Engine) MergeLaneTakesAuthor(repo, author string) bool {
	if _, ok := c.sweepLaneForAuthor(author); ok {
		return true
	}
	allowed := func(repoAllowed func(string) bool) bool {
		if repoAllowed(repo) {
			return true
		}
		if i := strings.LastIndex(repo, "/"); i >= 0 && i+1 < len(repo) {
			return repoAllowed(repo[i+1:])
		}
		return false
	}
	if policy := c.currentTrustedAuthorPolicy(); allowed(policy.repoAllowed) && c.trustedAuthorDecision(author, policy.RequireRole).Allowed {
		return true
	}
	return allowed(c.currentContributorLanePolicy().repoAllowed)
}
