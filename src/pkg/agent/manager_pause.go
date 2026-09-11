// Pause and breaker state: pause/resume with causation provenance,
// breaker engage/release/restore, and pause persistence wiring.
package agent

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// BreakerTrigger is the distinct PausedTrigger stamped on every pause the fleet
// breaker performs. It serves two jobs: the audit log attributes the pause to
// the breaker, and ReleaseBreaker uses it as the guard that distinguishes a
// pause the breaker still owns (safe to auto-resume) from one an operator
// re-applied during the breaker window (must stay paused). Anything whose
// current PausedTrigger != BreakerTrigger was last paused by something else.
const BreakerTrigger = "fleet-breaker"

// SetPersistPauseCallback wires a function that persists an agent's paused
// state to config (called on pause/resume). Idempotent saves are the
// caller's responsibility.
func (m *Manager) SetPersistPauseCallback(fn func(name string, paused bool)) {
	m.mu.Lock()
	m.persistPauseCallback = fn
	m.mu.Unlock()
}

// SetPauseTransitionObserver wires a post-persistence observer for agent
// pause/resume transitions. The observer runs asynchronously.
func (m *Manager) SetPauseTransitionObserver(fn func(PauseTransitionEvent)) {
	if fn == nil {
		m.pauseObserver.Store(nil)
		return
	}
	m.pauseObserver.Store(&fn)
}

func (m *Manager) emitPauseTransition(event PauseTransitionEvent) {
	if event.At.IsZero() {
		event.At = time.Now()
	}
	if fn := m.pauseObserver.Load(); fn != nil && *fn != nil {
		go (*fn)(event)
	}
}

// Pause pauses an agent with no attributed acting user — the right call for
// system-initiated pauses (login-detector, fleet-breaker, acmm-pack, state
// restore of a pause whose actor is unknown). A pause performed on behalf of
// a person goes through PauseBy so provenance survives (#4041).
func (m *Manager) Pause(name, trigger, reason string) error {
	return m.PauseBy(name, trigger, reason, "")
}

// PauseBy pauses an agent and records WHO asked for it. `by` is the acting
// user (the authenticated dashboard user for a dashboard-api pause); empty
// means "no human actor" — never fabricate one.
func (m *Manager) PauseBy(name, trigger, reason, by string) error {
	return m.PauseByCause(name, trigger, reason, by, PauseCausation{})
}

// PauseByCause is PauseBy plus hook causation metadata for the post-commit
// agent_paused emitter. Non-hook callers should use PauseBy/Pause.
func (m *Manager) PauseByCause(name, trigger, reason, by string, cause PauseCausation) error {
	m.mu.Lock()

	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}

	agent.Paused = true
	agent.PausedAt = time.Now()
	agent.PausedReason = reason
	agent.PausedTrigger = trigger
	agent.PausedBy = by
	if agent.State == StateRunning {
		if agent.cancel != nil {
			agent.cancel()
		}
		agent.sandboxResumeAfterCancel = false
		if !m.agentSandboxEnabledLocked(agent) {
			m.tmuxSendKeysForAgent(agent, "C-c", "")
		}
	}
	agent.State = StatePaused
	agent.Config.Paused = true
	// Snapshot the persistence callback under m.mu but invoke it only after
	// the unlock below: it does config disk I/O and may re-enter the manager,
	// and m.mu is a non-reentrant RWMutex — calling it here would deadlock
	// (see the persistPauseCallback field docs).
	persistPause := m.persistPauseCallback
	m.logger.Info("audit: agent paused",
		"name", name,
		"trigger", trigger,
		"reason", reason,
		"by", by,
		"backend", agent.Config.Backend,
		"restart_count", agent.RestartCount,
	)
	pausedAt := agent.PausedAt
	m.audit(AuditAgentPaused, name, auditFields(
		"outcome", "success",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
		"trigger", trigger,
		"reason", reason,
	))
	m.mu.Unlock()

	if persistPause != nil {
		persistPause(name, true)
	}
	m.emitPauseTransition(PauseTransitionEvent{
		Agent:     name,
		Paused:    true,
		Trigger:   trigger,
		Reason:    reason,
		By:        by,
		Causation: cause,
		At:        pausedAt,
	})
	return nil
}

func (m *Manager) SeedPauseState(name string, pausedAt time.Time, trigger, reason, by string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.PausedAt = pausedAt
		agent.PausedTrigger = trigger
		agent.PausedReason = reason
		agent.PausedBy = by
	}
}

func (m *Manager) Resume(ctx context.Context, name, trigger, reason string) error {
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}

	prevTrigger := agent.PausedTrigger
	prevReason := agent.PausedReason
	// Snapshot backend/model while m.mu is held so audit details stay stable
	// even when Resume relaunches the agent before returning.
	resumeBackend := agent.effectiveBackend()
	resumeModel := agent.effectiveModel()
	agent.Paused = false
	agent.Config.Paused = false
	if persistPause := m.persistPauseCallback; persistPause != nil {
		// Deferred so it runs after this function's explicit m.mu.Unlock on
		// every return path: the callback does config disk I/O and may
		// re-enter the manager, and m.mu is a non-reentrant RWMutex —
		// invoking it here with the lock held deadlocks Resume and wedges
		// everything queued behind m.mu (see persistPauseCallback docs).
		defer m.emitPauseTransition(PauseTransitionEvent{
			Agent:   name,
			Paused:  false,
			Trigger: trigger,
			Reason:  reason,
		})
		defer persistPause(name, false)
	}
	agent.PausedAt = time.Time{}
	agent.PausedReason = ""
	agent.PausedTrigger = ""
	agent.PausedBy = ""
	needsRelaunch := agent.State == StatePaused
	if m.agentSandboxEnabledLocked(agent) {
		if needsRelaunch {
			if agent.cancel != nil {
				agent.sandboxResumeAfterCancel = true
			} else {
				agent.State = StateIdle
				if agent.StartedAt == nil {
					now := time.Now()
					agent.StartedAt = &now
				}
			}
		}
		m.mu.Unlock()
		m.logger.Info("audit: sandbox agent resumed",
			"name", name,
			"trigger", trigger,
			"reason", reason,
			"prev_trigger", prevTrigger,
			"prev_reason", prevReason,
		)
		return nil
	}
	if needsRelaunch {
		agent.forceRelaunch = true
	}

	m.logger.Info("audit: agent resumed",
		"name", name,
		"trigger", trigger,
		"reason", reason,
		"prev_trigger", prevTrigger,
		"prev_reason", prevReason,
	)
	m.audit(AuditAgentResumed, name, auditFields(
		"outcome", "success",
		"backend", resumeBackend,
		"model", resumeModel,
		"trigger", trigger,
		"reason", reason,
	))
	if needsRelaunch {
		if err := m.ensureTmuxSession(agent); err != nil {
			m.mu.Unlock()
			return err
		}
		// #3962: mint the per-agent token in an UNLOCKED window before the
		// relaunch, mirroring Start's phase split. Resume used to call
		// launchInTmux without ever minting, so a resumed agent ran with an
		// empty token cache (gh/git push dead) until the whole process
		// restarted. The mint must not run under m.mu — see
		// mintAgentTokenUnlocked — so release, mint, then re-verify under the
		// re-acquired lock exactly as Start does.
		m.mu.Unlock()
		m.mintAgentTokenUnlocked(ctx, agent)
		m.bobPreflightUnlocked(agent)
		m.mu.Lock()
		if cur, ok := m.agents[name]; !ok || cur != agent {
			m.mu.Unlock()
			return fmt.Errorf("agent %s removed during resume", name)
		}
		if agent.State == StateRunning {
			// Another path won the launch race while we were unlocked.
			m.mu.Unlock()
			return nil
		}
		err := m.launchInTmux(ctx, agent)
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	return nil
}

// EngageBreaker throws the fleet-wide kill-switch: it pauses every agent that
// is currently RUNNING and not OnDemand, records exactly that set, and returns
// the names it paused. Agents that are on-demand (e.g. brainstorm) or already
// paused (a prior operator/manual pause) are skipped entirely and never enter
// the recorded set, so releasing the breaker later cannot un-pause them.
//
// Idempotent: if the breaker is already engaged, it re-pauses nothing and
// returns the existing recorded set unchanged.
//
// Pausing is done by calling Pause with BreakerTrigger. Pause takes m.mu
// itself, so this method collects the target names under m.mu, releases it,
// then pauses each — mirroring the lock discipline the dashboard uses when it
// pauses agents one at a time.
func (m *Manager) EngageBreaker() (paused []string) {
	m.mu.Lock()
	if m.breakerEngaged {
		existing := make([]string, 0, len(m.breakerPaused))
		for name := range m.breakerPaused {
			existing = append(existing, name)
		}
		m.mu.Unlock()
		sort.Strings(existing)
		return existing
	}

	targets := make([]string, 0, len(m.agents))
	for name, agent := range m.agents {
		if agent == nil {
			continue
		}
		// Skip on-demand agents (they are meant to sit idle until summoned) and
		// any agent that is already paused — a pause the operator owns, which
		// the breaker must not adopt and must not later reverse.
		if agent.Config.OnDemand {
			continue
		}
		if agent.Paused || agent.State != StateRunning {
			continue
		}
		targets = append(targets, name)
	}

	set := make(map[string]bool, len(targets))
	for _, name := range targets {
		set[name] = true
	}
	m.breakerEngaged = true
	m.breakerPaused = set
	m.mu.Unlock()

	sort.Strings(targets)
	for _, name := range targets {
		if err := m.Pause(name, BreakerTrigger, "fleet breaker engaged"); err != nil {
			m.logger.Warn("fleet breaker: pause failed", "agent", name, "error", err)
		}
	}
	m.logger.Info("fleet breaker engaged", "paused", len(targets))
	return targets
}

// ReleaseBreaker disengages the fleet-wide kill-switch. It resumes ONLY the
// agents the breaker itself paused (the recorded set) and, within that set,
// only those whose pause is STILL attributable to the breaker: current
// PausedTrigger == BreakerTrigger and still paused. An agent an operator
// re-paused during the breaker window has a different trigger and is left
// paused; an on-demand agent could never enter the set in the first place.
// Returns the names it resumed.
func (m *Manager) ReleaseBreaker(ctx context.Context) (resumed []string) {
	m.mu.Lock()
	if !m.breakerEngaged {
		m.mu.Unlock()
		return nil
	}

	candidates := make([]string, 0, len(m.breakerPaused))
	for name := range m.breakerPaused {
		agent, ok := m.agents[name]
		if !ok || agent == nil {
			continue
		}
		// Only resume agents still paused BY the breaker. If the operator
		// re-paused (trigger changed) or resumed-then-repaused during the
		// window, leave the agent exactly as the operator left it.
		if agent.Paused && agent.PausedTrigger == BreakerTrigger {
			candidates = append(candidates, name)
		}
	}
	m.breakerEngaged = false
	m.breakerPaused = nil
	m.mu.Unlock()

	sort.Strings(candidates)
	for _, name := range candidates {
		if err := m.Resume(ctx, name, BreakerTrigger, "fleet breaker released"); err != nil {
			m.logger.Warn("fleet breaker: resume failed", "agent", name, "error", err)
			continue
		}
		resumed = append(resumed, name)
	}
	m.logger.Info("fleet breaker released", "resumed", len(resumed))
	return resumed
}

// BreakerState reports whether the fleet breaker is engaged and the sorted set
// of agent names it currently holds paused.
func (m *Manager) BreakerState() (engaged bool, paused []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	engaged = m.breakerEngaged
	paused = make([]string, 0, len(m.breakerPaused))
	for name := range m.breakerPaused {
		paused = append(paused, name)
	}
	sort.Strings(paused)
	return engaged, paused
}

// RestoreBreaker re-establishes the breaker's in-memory state from persisted
// snapshot data on boot. The agents themselves are restored paused via their
// own persisted pause (with PausedTrigger == BreakerTrigger), so this only has
// to re-mark the breaker engaged and re-record the set. It does NOT re-pause or
// resume anything — a boot restore must never change agent state, only reattach
// the breaker so a later ReleaseBreaker knows which agents to resume. Only
// names still present AND still paused-by-the-breaker are re-adopted.
func (m *Manager) RestoreBreaker(engaged bool, paused []string) {
	if !engaged {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[string]bool, len(paused))
	for _, name := range paused {
		agent, ok := m.agents[name]
		if !ok || agent == nil {
			continue
		}
		if agent.Paused && agent.PausedTrigger == BreakerTrigger {
			set[name] = true
		}
	}
	m.breakerEngaged = true
	m.breakerPaused = set
	m.logger.Info("fleet breaker restored", "engaged", true, "held", len(set))
}
