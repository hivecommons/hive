package agent

import (
	"context"
	"fmt"
)

// RelaunchBobAgentsAwaitingKey restarts the bob-backend agents that
// launchInTmux parked in StateFailed because no API key was configured, and
// returns their names. It is the "absent → present" half of the key lifecycle:
// the resolver makes a newly-saved key visible to the NEXT launch, but nothing
// carries it into a session whose shell already exists, so an agent parked for
// a missing key would otherwise sit at bob's key prompt until someone
// intervened.
//
// Each relaunch RECREATES the tmux session rather than just re-typing the
// launch command — see killSessionForRelaunch for why that is required and not
// merely tidy.
//
// Call it after a key is stored. It is a no-op — returning nil — when the key
// still does not resolve, so a clear (or a failed save) never launches
// anything.
//
// Selection is deliberately narrow: awaitingBobKey is set on exactly one
// branch of launchInTmux and cleared on every launch attempt, so a running
// agent, a non-bob agent, and an agent failed for any other reason are all
// skipped. An operator-paused agent is skipped too — a key save must not
// override a deliberate pause. That also makes a double save harmless: the
// first relaunch clears the flag, so the second finds nothing to do.
//
// Locking: candidates are collected under RLock, the lock is RELEASED, and
// only then is Start called — Start takes m.mu.Lock() itself, and m.mu is a
// non-reentrant RWMutex, so calling it under the read lock would deadlock
// (incident #1980→#1988). This mirrors CheckAndRestartCrashedAgents.
func (m *Manager) RelaunchBobAgentsAwaitingKey(ctx context.Context) []string {
	// The key must actually resolve now; otherwise every relaunch would just
	// re-park on the same branch. Read lock-free via the atomic resolver, and
	// never bind or log the value — only its presence matters here.
	if m.bobAPIKey() == "" {
		return nil
	}

	candidates := m.bobAgentsAwaitingKey()

	var relaunched []string
	for _, name := range candidates {
		m.logger.Info("relaunching bob agent parked for missing API key", "name", name)
		// Kill the stale tmux session FIRST. This is load-bearing, not
		// hygiene: BOBSHELL_API_KEY is Secret, so buildEnvPrefix omits it from
		// the typed command line and it is delivered ONLY by tmux
		// set-environment — which updates the SESSION environment and is
		// inherited just by shells created afterwards. The pane's bash was
		// started before the key existed, so it does not have the variable and
		// never will; reapAgentCLI kills the bob CLI but leaves that bash
		// alive, so a plain relaunch retypes the command into the same
		// key-less shell and bob prompts for a key again (observed on
		// hosted-available-vllmd-01: BOBSHELL_API_KEY present in the tmux
		// session env, absent from every bob /proc/<pid>/environ).
		// Killing the session makes ensureTmuxSession build a new one and
		// re-run set-environment before any shell exists, so the fresh bash —
		// and the bob it spawns — inherit the key.
		if err := m.killSessionForRelaunch(name); err != nil {
			m.logger.Warn("could not kill stale session before bob relaunch; launching anyway",
				"name", name, "error", err)
		}
		// The saved key is new information about the exact condition that
		// parked this agent, so the backoff must not hold the retry (#5958).
		m.mu.Lock()
		m.clearStartFailureLocked(m.agents[name])
		m.mu.Unlock()
		if err := m.Start(ctx, name); err != nil {
			// One agent's failure must never abort the rest of the fleet,
			// exactly as in the crash-restart loop above.
			m.logger.Error("failed to relaunch bob agent after key save", "name", name, "error", err)
			continue
		}
		relaunched = append(relaunched, name)
	}
	return relaunched
}

// killSessionForRelaunch tears down an agent's tmux session so the next
// ensureTmuxSession creates a fresh one whose shell inherits the current
// set-environment values (including a newly-saved BOBSHELL_API_KEY).
//
// This is KillSession's behaviour, but it deliberately does not call
// KillSession: that method takes m.mu.Lock() and is a public entry point.
// Keeping a private helper with the same lock discipline (acquire, act,
// release — never held across Start) keeps the relaunch loop's locking
// obvious and avoids any temptation to hold a lock across the launch, which
// is the deadlock class from incident #1980→#1988.
func (m *Manager) killSessionForRelaunch(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	// Not an error if the session is already gone — the goal is "no stale
	// shell", which a missing session already satisfies.
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
	// State must not stay Running: Start refuses to launch a running agent,
	// and the session backing that state no longer exists.
	if agent.State == StateRunning {
		agent.State = StateStopped
	}
	m.logger.Info("killed stale tmux session so the fresh shell inherits the bob key",
		"name", name, "session", agent.tmuxSession)
	return nil
}

// bobAgentsAwaitingKey returns the names of agents parked by launchInTmux for a
// missing bob API key and eligible to be started now. Split out from
// RelaunchBobAgentsAwaitingKey so the selection rules can be tested without
// tmux: this is the part that must never pick up a running, paused, or non-bob
// agent.
//
// Takes m.mu.RLock and releases it before returning; the caller must NOT hold
// m.mu (non-reentrant RWMutex).
func (m *Manager) bobAgentsAwaitingKey() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var candidates []string
	for name, agent := range m.agents {
		if agent == nil || !agent.awaitingBobKey {
			continue
		}
		// Defensive: awaitingBobKey is only ever set on the bob branch, but
		// re-check the effective backend so a backend override applied after
		// parking cannot smuggle a non-bob agent into this path.
		if effectiveBackend(agent) != bobBackend {
			continue
		}
		// Never disturb a healthy agent, and never override a pause — a key
		// save is not a resume.
		if agent.State == StateRunning || agent.Paused {
			continue
		}
		candidates = append(candidates, name)
	}
	return candidates
}
