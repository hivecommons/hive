package agent

// Asynchronous kick dispatch (#5325).
//
// SendKick is synchronous and its slow leg — waitForInputPromptForAgent — is
// bounded by inputPromptTimeout (120s). A dashboard handler that calls it
// inline therefore outlives any normal ingress/proxy idle timeout (commonly
// 60s), so the proxy answers 504 while the wait is still running. The wait then
// finishes server-side, the prompt IS typed, and the agent runs the session —
// but the operator was told the kick failed. The natural response to a false
// failure is to click Kick again, which delivers the prompt TWICE; on a
// hold-gated lane that means duplicate advisory comments and beads.
//
// The fix is to take the prompt wait off the request path. SendKickAsync keeps
// every FAST, deterministic precondition on the caller's goroutine — agent
// exists, sandbox routing, state is running, tmux session exists — so a
// genuinely un-kickable agent still fails synchronously and is still reported
// as a failure. Only the slow legs (crash-restart recovery, the input-prompt
// wait, and the typing itself) move to a background goroutine.
//
// Exactly-once delivery is enforced by an in-flight guard keyed on agent name:
// a second SendKickAsync for an agent whose dispatch is still running does NOT
// start a second delivery. This is the property that makes the async contract
// safe for a UI that used to see false failures — even a retry that predates
// this fix's UI changes cannot double-type.
//
// Outcome is published two ways, both off the request path:
//   - KickDispatchState(name) — a polled snapshot for the dashboard.
//   - the existing kick observer — "kick-delivered" still fires from
//     deliverKickLocked exactly as before.
//
// Locking: SendKickAsync must NOT be called with m.mu held. It takes m.mu for
// the precondition check, releases it, and the background goroutine then calls
// the same lock-taking helpers SendKick uses. Nothing here re-enters m.mu on a
// goroutine that already holds it — the repo has had startup deadlocks from
// exactly that mistake (see the isGatewayBackend comment in manager.go).

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Kick dispatch phases. A dispatch is INDETERMINATE while it is pending: the
// prompt may still be delivered. Only KickPhaseFailed is a definitive failure.
const (
	// KickPhasePending means the kick passed its preconditions and a delivery
	// goroutine is waiting for the CLI's input prompt.
	KickPhasePending = "pending"
	// KickPhaseDelivered means the message was typed into the agent's pane.
	KickPhaseDelivered = "delivered"
	// KickPhaseFailed means delivery will not happen: the CLI never reached
	// its input prompt within inputPromptTimeout, a restart failed, or the
	// agent disappeared mid-flight.
	KickPhaseFailed = "failed"
)

// KickDispatch is the observable outcome of one asynchronous kick.
type KickDispatch struct {
	// Agent is the resolved agent name.
	Agent string `json:"agent"`
	// Phase is one of KickPhasePending / KickPhaseDelivered / KickPhaseFailed.
	Phase string `json:"phase"`
	// Error carries the failure reason when Phase is KickPhaseFailed. It is
	// empty in every other phase.
	Error string `json:"error,omitempty"`
	// QueuedAt is when the dispatch passed its preconditions.
	QueuedAt time.Time `json:"queuedAt"`
	// SettledAt is when the dispatch reached a terminal phase. Zero while
	// pending.
	SettledAt time.Time `json:"settledAt,omitempty"`
}

// Pending reports whether this dispatch is still in flight — the state in
// which the outcome is INDETERMINATE and must never be rendered as a failure.
func (d KickDispatch) Pending() bool { return d.Phase == KickPhasePending }

// kickDispatchRegistry holds the latest dispatch per agent plus the in-flight
// guard. It is deliberately independent of m.mu: the background delivery
// goroutine settles a dispatch while holding no manager lock at all, and a
// concurrent poll of KickDispatchState must never contend with the launch
// path.
type kickDispatchRegistry struct {
	mu      sync.Mutex
	byAgent map[string]*KickDispatch
}

func (r *kickDispatchRegistry) begin(name string) (*KickDispatch, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byAgent == nil {
		r.byAgent = make(map[string]*KickDispatch)
	}
	if cur, ok := r.byAgent[name]; ok && cur.Pending() {
		// Already in flight. Return the existing dispatch and refuse to start
		// a second delivery — this is the exactly-once guarantee.
		return cur, false
	}
	d := &KickDispatch{Agent: name, Phase: KickPhasePending, QueuedAt: time.Now()}
	r.byAgent[name] = d
	return d, true
}

func (r *kickDispatchRegistry) settle(name, phase, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byAgent[name]
	if !ok || !d.Pending() {
		return
	}
	d.Phase = phase
	d.Error = errMsg
	d.SettledAt = time.Now()
}

func (r *kickDispatchRegistry) get(name string) (KickDispatch, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byAgent[name]
	if !ok {
		return KickDispatch{}, false
	}
	return *d, true
}

// KickDispatchState returns the most recent asynchronous kick dispatch for an
// agent and whether one exists. The dashboard polls this to report the true
// outcome after answering the POST with 202.
func (m *Manager) KickDispatchState(name string) (KickDispatch, bool) {
	return m.kickDispatches.get(name)
}

// SendKickAsync validates a kick's preconditions synchronously and then
// performs the slow delivery in the background, returning as soon as the kick
// is queued.
//
// The returned bool reports whether THIS call started a new delivery. False
// with a nil error means a delivery for the same agent was already in flight
// and this call was deduplicated — the operator's second click is a no-op, not
// a second prompt.
//
// A non-nil error is a genuine, definitive failure (agent unknown, paused,
// stopped, no tmux session, sandbox kick rejected) and callers should report it
// as such. Sandbox-backed agents keep the synchronous path entirely: their
// kick has no pane wait, so there is no slow leg to move off the request.
//
// MUST NOT be called with m.mu held.
func (m *Manager) SendKickAsync(name string, message string) (started bool, err error) {
	m.mu.Lock()

	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("agent %s not found", name)
	}

	// Sandbox kicks start a container, not a pane wait. They are already fast
	// and fully synchronous, so run them inline and report the real result.
	if m.agentSandboxEnabledLocked(agent) {
		sErr := m.startSandboxKickLocked(agent, message)
		m.mu.Unlock()
		if sErr != nil {
			return false, sErr
		}
		// Record it as already delivered so a client polling the dispatch
		// state settles immediately instead of waiting out its poll budget on
		// a kick that never had an asynchronous leg.
		if _, fresh := m.kickDispatches.begin(name); fresh {
			m.kickDispatches.settle(name, KickPhaseDelivered, "")
		}
		return true, nil
	}

	if agent.State != StateRunning {
		m.mu.Unlock()
		return false, fmt.Errorf("agent %s cannot be kicked: %s", name, notRunningReason(agent))
	}
	if remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now()); remaining > 0 {
		class, line := agent.ProviderErrorClass, agent.ProviderErrorLine
		m.mu.Unlock()
		return false, fmt.Errorf("agent %s blocked: inference (%s): %s; next provider probe in %v",
			name, class, line, remaining.Round(time.Second))
	}

	if !m.tmuxSessionExistsForAgent(agent) {
		session := agent.tmuxSession
		m.mu.Unlock()
		return false, fmt.Errorf("tmux session %s not found", session)
	}

	m.mu.Unlock()

	// Claim the in-flight slot BEFORE spawning, so two concurrent callers can
	// never both spawn. The loser returns started=false with no error.
	if _, fresh := m.kickDispatches.begin(name); !fresh {
		m.logger.Info("kick already in flight, not delivering again", "name", name)
		return false, nil
	}

	go func() {
		if dErr := m.deliverKickAsync(name, message); dErr != nil {
			m.kickDispatches.settle(name, KickPhaseFailed, dErr.Error())
			m.logger.Warn("async kick delivery failed", "name", name, "error", dErr)
			return
		}
		m.kickDispatches.settle(name, KickPhaseDelivered, "")
	}()

	return true, nil
}

// deliverKickAsync is the slow half of SendKickAsync, run on its own
// goroutine. It re-checks liveness under the lock (the agent can be paused or
// restarted between queueing and delivery), recovers a crashed or
// consent-wedged CLI, waits for the input prompt, and types the message.
//
// It holds NO lock on entry and holds none on return; every m.mu acquisition
// below is balanced within this function, mirroring SendKick's unlock/relock
// dance around the two slow waits.
func (m *Manager) deliverKickAsync(name, message string) error {
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}
	if agent.State != StateRunning {
		reason := notRunningReason(agent)
		m.mu.Unlock()
		return fmt.Errorf("agent %s cannot be kicked: %s", name, reason)
	}
	if remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now()); remaining > 0 {
		class, line := agent.ProviderErrorClass, agent.ProviderErrorLine
		m.mu.Unlock()
		return fmt.Errorf("agent %s blocked: inference (%s): %s; next provider probe in %v",
			name, class, line, remaining.Round(time.Second))
	}

	// Detect a crashed CLI (bare shell) or a CLI stuck on a consent screen and
	// restart before sending — identical to SendKick's recovery. A consent pane
	// contains "❯" so it passes the marker check, but a kick typed into it is
	// consumed by the menu, or by bash once "No, exit" exits the CLI.
	pane := m.captureVisiblePaneForAgent(agent)
	if !paneHasCLIMarker(pane) || paneShowsConsentScreen(pane) {
		consentScreen := paneShowsConsentScreen(pane)
		if consentScreen {
			// Same wedge bookkeeping as SendKick — see noteConsentWedge.
			m.noteConsentWedge(name)
		}
		m.logger.Warn("agent CLI crashed or stuck on consent screen, restarting before kick",
			"name", name, "consent_screen", consentScreen)
		m.mu.Unlock()
		if err := m.Restart(context.Background(), name); err != nil {
			return fmt.Errorf("failed to restart crashed agent %s: %w", name, err)
		}
		if !m.waitForCLIReadyForAgent(agent) {
			return fmt.Errorf("agent %s CLI did not become ready after restart", name)
		}
		m.mu.Lock()
		agent, ok = m.agents[name]
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("agent %s disappeared after restart", name)
		}
	}

	// Wait for the input prompt (❯) before sending — the CLI may be showing a
	// trust prompt or still initializing even though the pane matched a broad
	// marker like "Copilot". This is the leg that can take up to
	// inputPromptTimeout and is exactly why this function is not on the request
	// path. Exhausting it is a genuine failure and is reported as one.
	m.mu.Unlock()
	if !m.waitForInputPromptForAgent(agent) {
		return fmt.Errorf("agent %s CLI did not reach input prompt", name)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok = m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s disappeared while waiting for input prompt", name)
	}
	if remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now()); remaining > 0 {
		return fmt.Errorf("agent %s blocked: inference (%s): %s; next provider probe in %v",
			name, agent.ProviderErrorClass, agent.ProviderErrorLine, remaining.Round(time.Second))
	}
	m.deliverKickLocked(agent, message, "send-kick")
	return nil
}
