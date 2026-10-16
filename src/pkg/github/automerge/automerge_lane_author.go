package automerge

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
func (c *Engine) MergeLaneTakesAuthor(repo, author string) bool {
	if _, ok := c.sweepLaneForAuthor(author); ok {
		return true
	}
	if policy := c.currentTrustedAuthorPolicy(); policy.repoAllowed(repo) && c.trustedAuthorDecision(author, policy.RequireRole).Allowed {
		return true
	}
	return c.currentContributorLanePolicy().repoAllowed(repo)
}
