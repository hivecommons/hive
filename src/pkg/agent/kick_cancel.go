package agent

// Restart cancels pending kicks, and a turn-destroying restart holds the next
// one (#7363).
//
// Observed live on a hosted spoke: an operator interrupted an agent by
// accident and clicked Restart on the dashboard. The restart tore the session
// down — and the kick that had been waiting for the CLI's input prompt
// (SendKickAsync's background goroutine, or a synchronous SendKick parked in
// waitForInputPromptForAgent) simply carried on waiting, saw the RELAUNCHED
// CLI's prompt, and typed the same 71 KB prompt into the fresh session. Ten
// seconds later another restart landed, and the cycle repeated: restart_count
// climbed 47 → 53 while the agent produced nothing, every turn interrupted
// while producing (`interruptions_total` 13, 14, …), every cycle paying a full
// large-prompt delivery.
//
// Two mechanisms, both keyed on state the restart path already owns:
//
//   - kickEpoch. Every restart teardown increments it. A kick captures the
//     epoch after its own recovery restart (if any) and before it starts
//     waiting for the prompt; it re-checks under the lock immediately before
//     typing and drops the message if the epoch moved. The prompt wait itself
//     polls the same check so the goroutine is released as soon as the restart
//     begins, and the dispatch registry entry is failed at the same moment so
//     the dashboard's poll reports "cancelled by restart" instead of pending.
//     A restart is the operator saying "stop"; nothing replays after it.
//
//   - kickHoldUntil. When the teardown destroyed a PRODUCING turn — the exact
//     signal the turn-loss accounting logs as producing=true — the restart
//     arms a short hold during which SendKick / SendKickAsync refuse with a
//     reason naming the hold and why. That is the breaker: a restart cannot be
//     followed within seconds by a kick that will itself be restarted. The
//     hold is deliberately short (restartInterruptedKickHold): it has to
//     outlast the seconds-scale loop, not an operator's patience, and any
//     scheduler cadence is minutes anyway.
//
// A kick's own recovery restart (restartForKick — the CLI was a bare shell or
// a consent screen) is exempt from both the dispatch failure and the breaker:
// that restart's caller is the intended next delivery, and it captures the
// post-restart epoch. It still invalidates any OTHER pending kick.

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
