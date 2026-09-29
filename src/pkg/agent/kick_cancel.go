package agent

import (
	"errors"
	"fmt"
	"time"
)

// errKickCancelledByRestart is the sentinel wrapped into every "the agent was
// restarted while this kick was pending" error, so callers (and tests) can
// tell a cancelled kick from a CLI that genuinely never showed its prompt.
var errKickCancelledByRestart = errors.New("kick cancelled by restart")

// errKickHeldAfterRestart is the sentinel for the breaker refusal.
var errKickHeldAfterRestart = errors.New("kick held after restart")

// restartInterruptedKickHold is how long a restart that destroyed a producing
// turn holds new kicks for that agent. A var so tests can shorten it.
var restartInterruptedKickHold = 60 * time.Second

// kickHoldNow is the breaker's clock, seamed for tests.
var kickHoldNow = time.Now

// invalidateKicksOnRestartLocked is the restart teardown's kick bookkeeping.
// Callers hold m.mu. It always bumps the epoch; it fails the agent's pending
// async dispatch when failDispatch is set, and arms the breaker when armHold
// is set.
func (m *Manager) invalidateKicksOnRestartLocked(agent *AgentProcess, reason string, armHold, failDispatch bool) {
	agent.kickEpoch++
	m.resetBusyVisibilityLocked(agent)
	reason = sanitizeRestartReason(reason)
	if failDispatch {
		if m.kickDispatches.cancel(agent.Name, "cancelled: agent restarted ("+reason+") while the kick was pending") {
			m.logger.Info("audit: pending kick cancelled by restart",
				"name", agent.Name, "reason", reason, "kick_epoch", agent.kickEpoch)
		}
	}
	if armHold {
		now := kickHoldNow()
		agent.kickHoldUntil = now.Add(restartInterruptedKickHold)
		agent.kickHoldReason = reason
		m.logger.Info("audit: kicks held after restart interrupted a producing turn",
			"name", agent.Name, "reason", reason,
			"hold_s", restartInterruptedKickHold.Seconds(),
			"interruptions_total", agent.TurnLoss.Interruptions)
	}
}

// restartKickHoldErrLocked reports the breaker's refusal, or nil when no hold
// is in force. Callers hold m.mu. The message says how long remains and why,
// so an operator who did mean to re-kick knows to wait rather than to click
// again — and a scheduler kick that lands inside the window is simply the
// one that does not restart the agent for a third time.
func (m *Manager) restartKickHoldErrLocked(agent *AgentProcess, now time.Time) error {
	if agent.kickHoldUntil.IsZero() || !now.Before(agent.kickHoldUntil) {
		return nil
	}
	remaining := agent.kickHoldUntil.Sub(now).Round(time.Second)
	return fmt.Errorf("%w: agent %s was restarted (%s) while producing a turn; kicks are held for %v more so the restart is not followed by a kick that gets restarted too (#7363)",
		errKickHeldAfterRestart, agent.Name, agent.kickHoldReason, remaining)
}

// kickEpochChanged reports whether a restart has invalidated a kick that
// captured epoch. Takes the read lock; must NOT be called with m.mu held.
func (m *Manager) kickEpochChanged(agent *AgentProcess, epoch int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return agent.kickEpoch != epoch
}

// kickEpochChangedFn adapts kickEpochChanged to the abort predicate
// waitForInputPromptForAgentUnless polls.
func (m *Manager) kickEpochChangedFn(agent *AgentProcess, epoch int) func() bool {
	return func() bool { return m.kickEpochChanged(agent, epoch) }
}
