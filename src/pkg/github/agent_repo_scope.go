package github

// Per-repo custom agents (#6204): the client-side half.
//
// The MITM proxy refuses an out-of-scope agent write, but three write paths do
// not traverse it. `hive-open-pr`, `hive-merge` and `hive-open-issue` are relays:
// the agent drops a request file, the HIVE performs the GitHub call, and the
// hive's own traffic is exempt from the forced-egress redirect by design. For
// PR creation and merge that is not incidental — the proxy HARD-DENIES direct
// POST /pulls and PUT /pulls/{n}/merge for every agent mode precisely so those
// operations route through here.
//
// So a scope enforced only at the proxy would have left a specialist able to
// open PRs, merge them, and file issues on repositories it was never defined
// for — the exact activity the scope exists to prevent. Each relay asks the same
// predicate before it acts.

// SetAgentRepoScopeFunc installs the per-repo agent-scope predicate (#6204).
// The hive passes config's AgentServesRepo, so a scope edited in the dashboard
// is in force on the very next relay request with nothing to re-wire; passing
// nil clears it, which is the unscoped behaviour every hive had before.
func (c *Client) SetAgentRepoScopeFunc(fn func(agent, repo string) bool) {
	if c == nil {
		return
	}
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.agentServesRepo = fn
}

// AgentServesRepo reports whether agent is scoped to repo. repo may be bare or
// "owner/repo" — the configured predicate normalizes both.
//
// Fails OPEN when no predicate is configured or the agent is unnamed: the scope
// can only ever NARROW an agent that would otherwise serve everything, so an
// unknown answer must not invent a refusal.
func (c *Client) AgentServesRepo(agent, repo string) bool {
	if c == nil {
		return true
	}
	c.reposMu.RLock()
	serves := c.agentServesRepo
	c.reposMu.RUnlock()
	if serves == nil || agent == "" {
		return true
	}
	return serves(agent, repo)
}

// AgentRepoScopeReason is the operator- and agent-facing explanation written
// into a relay request's result file when the agent is not scoped to the target
// repository.
func AgentRepoScopeReason(agent, repo string) string {
	return "agent " + agent + " is not scoped to " + repo + " — this hive defines it for a named set of repositories and " + repo + " is not one of them. Widen the agent's repos in its config to allow this."
}

// FilterActionableForRepos returns a copy of res holding only the items in
// repos the keep predicate accepts, so a repo-scoped agent's kick describes the
// repositories it serves and nothing else (#6204).
//
// This is the "task filtering" half of the feature, and it is where the cost
// saving lives: cadence stays hive-wide per agent, so a scoped agent still
// wakes — but it wakes to its own repos' work instead of a backlog it must read
// through and discard. An agent shown a schema-migration issue in a repo with
// no database will look at it; the cheapest fix is not to show it.
//
// A nil result or a nil predicate returns res unchanged, so an unscoped agent
// costs nothing.
func FilterActionableForRepos(res *ActionableResult, keep func(repo string) bool) *ActionableResult {
	if res == nil || keep == nil {
		return res
	}
	out := &ActionableResult{GeneratedAt: res.GeneratedAt}

	issues := make([]Issue, 0, len(res.Issues.Items))
	for _, issue := range res.Issues.Items {
		if keep(issue.Repo) {
			issues = append(issues, issue)
		}
	}
	// IssueResultFromItems recomputes Count and SLAViolations from what
	// survived. Carrying the unfiltered counts would tell an agent scoped to
	// one repo that it has forty issues and then list three.
	out.Issues = IssueResultFromItems(issues)

	prs := make([]PullRequest, 0, len(res.PRs.Items))
	for _, pr := range res.PRs.Items {
		if keep(pr.Repo) {
			prs = append(prs, pr)
		}
	}
	stale := make([]PullRequest, 0, len(res.PRs.StaleDrafts))
	for _, pr := range res.PRs.StaleDrafts {
		if keep(pr.Repo) {
			stale = append(stale, pr)
		}
	}
	out.PRs = PRResult{Count: len(prs), Items: prs}
	if len(stale) > 0 {
		out.PRs.StaleDrafts = stale
	}

	hold := make([]HoldItem, 0, len(res.Hold.Items))
	for _, item := range res.Hold.Items {
		if keep(item.Repo) {
			hold = append(hold, item)
		}
	}
	for _, item := range hold {
		if item.Type == "pr" {
			out.Hold.PRs++
		} else {
			out.Hold.Issues++
		}
	}
	out.Hold.Items = hold
	out.Hold.Total = len(hold)

	for _, cluster := range res.Clusters {
		kept := make([]Issue, 0, len(cluster.Issues))
		for _, issue := range cluster.Issues {
			if keep(issue.Repo) {
				kept = append(kept, issue)
			}
		}
		if len(kept) > 0 {
			out.Clusters = append(out.Clusters, IssueCluster{Key: cluster.Key, Count: len(kept), Issues: kept})
		}
	}

	if res.TotalByRepo != nil {
		out.TotalByRepo = make(map[string]RepoCounts, len(res.TotalByRepo))
		for repo, counts := range res.TotalByRepo {
			if keep(repo) {
				out.TotalByRepo[repo] = counts
			}
		}
	}
	return out
}
