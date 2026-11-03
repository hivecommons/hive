package github

// SetScheduledBaseRefresh installs the live current-base scheduling policy.
// Automatic client-side updates (follow-up, merge relay, supersession) defer
// to the automerge engine's shared preparation owner on these repositories.
// The engine uses its own head-pinned REST mutation after admission.
func (c *Client) SetScheduledBaseRefresh(policy func(repo string) bool) {
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.scheduledBaseRefresh = policy
}

func (c *Client) baseRefreshScheduled(repo string) bool {
	c.reposMu.RLock()
	policy := c.scheduledBaseRefresh
	c.reposMu.RUnlock()
	return policy != nil && policy(repo)
}
