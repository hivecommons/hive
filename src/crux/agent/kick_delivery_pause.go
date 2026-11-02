package agent

import "time"

// kickChunkPauseUnlocked waits out one inter-chunk delay with m.mu released,
// then re-acquires it and re-validates that the kick still belongs to the
// session it was pinned to.
//
// Returns false if the agent disappeared, was replaced, or restarted while
// unlocked, meaning the remaining chunks must not be typed — the pane they
// would land in is a different run's. This mirrors the epoch revalidation
// SendKick already does around waitForInputPromptForAgent (#7363).
//
// The identity check is deliberately pointer-based as well as epoch-based: a
// replacement AgentProcess for the same name starts its kickEpoch at zero, so
// an epoch comparison alone could alias a fresh object as "unchanged".
//
// Always returns with m.mu held, including on the false path, so callers can
// keep their `Locked` contract.
func (m *Manager) kickChunkPauseUnlocked(agent *AgentProcess, epoch int, d time.Duration) bool {
	name := agent.Name
	m.mu.Unlock()
	time.Sleep(d)
	m.mu.Lock()
	current, ok := m.agents[name]
	if !ok || current != agent {
		return false
	}
	return agent.kickEpoch == epoch
}
