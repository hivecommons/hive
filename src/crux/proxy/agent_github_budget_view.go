package proxy

import "time"

// AgentGitHubCallsLastHour reports, per agent, how many GitHub API responses
// the proxy has counted against that agent's hourly cap in the current
// sliding hour: the same window the cap check reads. It is read-only (stale
// entries are skipped, not pruned), so calling it never changes what the next
// cap check sees.
//
// The window only grows while an agent has a cap: the proxy records a call
// only when the agent's cap is above 0, so an uncapped agent reads 0 here
// whatever it has spent, and a cap switched on starts from an empty window.
func (p *GitHubProxy) AgentGitHubCallsLastHour() map[string]int {
	out := map[string]int{}
	if p == nil || p.githubBudget == nil {
		return out
	}
	return p.githubBudget.callsInWindow()
}

func (b *agentGitHubBudget) callsInWindow() map[string]int {
	out := map[string]int{}
	if b == nil {
		return out
	}
	now := time.Now()
	if b.now != nil {
		now = b.now()
	}
	cutoff := now.Add(-agentGitHubBudgetWindow)
	b.mu.Lock()
	defer b.mu.Unlock()
	for agent, events := range b.events {
		n := 0
		for _, at := range events {
			if at.After(cutoff) {
				n++
			}
		}
		if n > 0 {
			out[agent] = n
		}
	}
	return out
}
