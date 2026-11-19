package agent

import (
	"context"
	"time"

	"github.com/hivecommons/hive/crux/claude"
)

// Login-prompt heal routing for Claude agents (hivecommons/hive#18732).
//
// Before this, the pane poller and the login detector agreed on one rule: a
// login prompt on screen plus a credential file that HasUsableToken accepts
// means "stuck CLI, restart it". That rule is right for the case it was built
// for (an operator authenticated in one terminal, the other agents need a
// nudge) and exactly wrong for a REVOKED grant, which leaves the file intact
// while the provider refuses every token in it. The hive then restarted agents
// into the same dead login until the 3-per-24h budget ran out, logged
// `giving up on token-triggered restart`, and went quiet; the login detector
// meanwhile logged `credential is valid — leaving it to the token-restart
// heal` and did nothing. Nobody paged the operator.
//
// "Valid" now means the provider accepts the token. When a Claude agent sits
// at a login prompt with a file that looks valid, the poller asks the provider
// (claude.Verifier, the same read-only usage endpoint the headroom probe uses)
// before choosing a heal:
//
//   - rejected (HTTP 401 to a live token) → the agent is marked
//     BackendAuthRevoked, which the dashboard banner, the login detector's
//     operator notification and the hub's health verdict all read. No restart
//     fires and the restart ledger is not touched, so the fleet still has its
//     budget the moment the operator signs in.
//   - accepted / inconclusive → the existing capped heal runs unchanged.
//
// A dashboard sign-in (ReloadClaudeToken with a usable credential on disk) is
// a NEW signal, not another retry: every agent still at the prompt is
// restarted once even when its 24h window is exhausted.

const (
	// claudeCredentialVerifyInterval bounds how often one credential file is
	// checked live against the provider while agents sit at a login prompt.
	// The poller runs every ~3s per agent; the verdict is shared across agents
	// on the same file, so a six-agent fleet makes one call per interval, not
	// one per poll.
	claudeCredentialVerifyInterval = 2 * time.Minute
	// operatorSignInRestartWindow is how long after a dashboard Claude sign-in
	// an agent still at the login prompt is restarted outside the
	// token-restart budget. Bounded so a sign-in cannot be "consumed" hours
	// later by an unrelated login prompt.
	operatorSignInRestartWindow = 15 * time.Minute
)

// ClaudeLoginRevokedMessage is the operator-facing one-liner used for the
// agent's LastError, the dashboard banner and the hub remediation. One string
// so the three surfaces cannot drift.
const ClaudeLoginRevokedMessage = "Claude login revoked: sign in again"

// claudeCredentialVerifyFn is the seam the live check goes through; nil uses
// claude.Verifier{}. Tests substitute a fake provider verdict.
type claudeCredentialVerifyFn func(ctx context.Context, credPath string) (claude.CredentialVerdict, string)

// claudeCredentialVerification is one cached provider answer for one file.
type claudeCredentialVerification struct {
	at       time.Time
	verdict  claude.CredentialVerdict
	detail   string
	inflight bool
}

// claudeLoginState is the manager-wide half of the #18732 routing: the
// per-file verification cache, the provider seam, the once-per-episode WARN
// gate per agent, and the operator sign-in signal. Guarded by Manager.mu.
type claudeLoginState struct {
	verified map[string]*claudeCredentialVerification
	// verify is the live-check seam; nil uses claude.Verifier{}.
	verify claudeCredentialVerifyFn
	// warned gates the "Claude login revoked" WARN to once per agent per
	// episode; cleared when the provider accepts the credential again or the
	// operator signs in.
	warned    map[string]bool
	signInAt  time.Time
	signInGen uint64
}

// loginPromptHeal is what the poller does about a Claude agent whose pane
// shows a login prompt while the credential FILE looks valid.
type loginPromptHeal int

const (
	// loginHealTokenRestart — the existing capped token-triggered restart
	// (decideTokenRestart) decides, exactly as before #18732.
	loginHealTokenRestart loginPromptHeal = iota
	// loginHealOperatorLogin — the provider rejected the credential. Alert the
	// operator; never restart; leave the restart ledger alone.
	loginHealOperatorLogin
	// loginHealSignInRestart — the operator just signed in from the dashboard.
	// Restart once, bypassing the cooldown, the attempt cap and the 24h window.
	loginHealSignInRestart
)

// decideLoginPromptHeal routes one login-prompt sighting. Called from the
// agent's pane poller once the login streak and the file-level credential
// gate have both passed; signInGenSeen is the poller's record of the last
// operator sign-in it acted on, so one sign-in restarts each agent once.
//
// Non-Claude backends have no live verifier and fall straight through to the
// existing heal.
func (m *Manager) decideLoginPromptHeal(ctx context.Context, agent *AgentProcess, signInGenSeen *uint64, now time.Time) loginPromptHeal {
	if agent == nil || effectiveBackend(agent) != "claude" {
		return loginHealTokenRestart
	}
	if gen, due := m.operatorSignInDue(*signInGenSeen, now); due {
		*signInGenSeen = gen
		// The operator's login is a new theory, so the ledger the old one
		// exhausted is cleared — and this restart is booked against the fresh
		// ledger like any other, so a second sign-in minutes later is still
		// bounded by the ordinary cap.
		agent.clearTokenRestartCap()
		agent.recordTokenRestart(now)
		m.clearClaudeLoginRevoked(agent, now)
		return loginHealSignInRestart
	}
	verdict, detail := m.verifyClaudeCredential(ctx, agent, now)
	switch verdict {
	case claude.CredentialRejected:
		m.markClaudeLoginRevoked(agent, detail, now)
		return loginHealOperatorLogin
	case claude.CredentialAccepted:
		m.clearClaudeLoginRevoked(agent, now)
	}
	return loginHealTokenRestart
}

// verifyClaudeCredential asks the provider about the credential file this
// agent would restart with, through a per-file cache so a fleet at the prompt
// makes one live call per claudeCredentialVerifyInterval. While a call is in
// flight other pollers read inconclusive and keep their current behaviour.
func (m *Manager) verifyClaudeCredential(ctx context.Context, agent *AgentProcess, now time.Time) (claude.CredentialVerdict, string) {
	credPath := ""
	for _, p := range agentClaudeCredentialPaths(agent.Name, agent.UID, effectiveBackend(agent)) {
		if claude.HasUsableToken(p) {
			credPath = p
			break
		}
	}
	if credPath == "" {
		return claude.CredentialInconclusive, ""
	}

	m.mu.Lock()
	if m.claudeLogin.verified == nil {
		m.claudeLogin.verified = map[string]*claudeCredentialVerification{}
	}
	entry := m.claudeLogin.verified[credPath]
	if entry == nil {
		entry = &claudeCredentialVerification{}
		m.claudeLogin.verified[credPath] = entry
	}
	if !entry.at.IsZero() && now.Sub(entry.at) < claudeCredentialVerifyInterval {
		verdict, detail := entry.verdict, entry.detail
		m.mu.Unlock()
		return verdict, detail
	}
	if entry.inflight {
		m.mu.Unlock()
		return claude.CredentialInconclusive, ""
	}
	entry.inflight = true
	verify := m.claudeLogin.verify
	m.mu.Unlock()

	if verify == nil {
		verify = claude.Verifier{}.Verify
	}
	verdict, detail := verify(ctx, credPath)

	m.mu.Lock()
	entry.inflight = false
	entry.at = now
	entry.verdict = verdict
	entry.detail = detail
	m.mu.Unlock()
	return verdict, detail
}

// markClaudeLoginRevoked records the provider's rejection on the agent. The
// WARN fires once per agent per episode, not once per poll; the restart
// ledger is read for the log line and never written.
func (m *Manager) markClaudeLoginRevoked(agent *AgentProcess, detail string, now time.Time) {
	m.mu.Lock()
	agent.markBackendAuthLocked(BackendAuthRevoked, "provider rejected the stored Claude credential ("+detail+")", now)
	if m.claudeLogin.warned == nil {
		m.claudeLogin.warned = map[string]bool{}
	}
	warned := m.claudeLogin.warned[agent.Name]
	m.claudeLogin.warned[agent.Name] = true
	m.mu.Unlock()
	agent.LastError = ClaudeLoginRevokedMessage + " — the provider rejected the stored credential (" + detail + "); no restart will be attempted until you sign in from the dashboard"
	if warned || m.logger == nil {
		return
	}
	m.logger.Warn("Claude login revoked: the provider rejected the stored credential — no restart will be attempted; sign in again from the dashboard",
		"agent", agent.Name,
		"detail", detail,
		"restarts_24h", agent.tokenRestartsInWindow(now),
		"window_max", tokenRestartWindowMax,
	)
}

// clearClaudeLoginRevoked lifts the revoked mark once the provider accepts the
// credential again or the operator signed in. A no-op for agents not marked.
func (m *Manager) clearClaudeLoginRevoked(agent *AgentProcess, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.claudeLogin.warned, agent.Name)
	if agent.BackendAuth.Status == BackendAuthRevoked {
		agent.clearBackendAuthLocked(now)
	}
}

// AgentCredentialRevoked reports whether this agent's provider rejected its
// stored credential on a live check (BackendAuthRevoked), with the redacted
// detail. The login detector (pkg/loginscan) uses it to route a login prompt
// to the operator instead of either pausing the agent or deferring to a
// restart heal that cannot work.
func (m *Manager) AgentCredentialRevoked(agentName string) (bool, string) {
	if m == nil {
		return false, ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	proc := m.agents[agentName]
	if proc == nil || proc.BackendAuth.Status != BackendAuthRevoked {
		return false, ""
	}
	return true, proc.BackendAuth.LastError
}

// noteClaudeOperatorSignInLocked records a dashboard Claude sign-in: the
// verification cache is dropped (the file changed), every revoked mark is
// lifted, and the sign-in generation advances so each poller restarts its
// agent once if it is still at the prompt. Caller holds m.mu.
func (m *Manager) noteClaudeOperatorSignInLocked(now time.Time) {
	m.claudeLogin.verified = nil
	m.claudeLogin.signInAt = now
	m.claudeLogin.signInGen++
	m.claudeLogin.warned = nil
	for _, a := range m.agents {
		if a.BackendAuth.Status == BackendAuthRevoked {
			a.clearBackendAuthLocked(now)
		}
	}
	if m.logger != nil {
		m.logger.Info("operator signed in to Claude; agents still at a login prompt will be restarted once, outside the token-restart window")
	}
}

// claudeSignInGeneration is the poller's starting point: a sign-in that
// happened before this poller existed is not its signal to act on.
func (m *Manager) claudeSignInGeneration() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.claudeLogin.signInGen
}

// operatorSignInDue answers whether a sign-in newer than seenGen happened
// within operatorSignInRestartWindow, returning its generation.
func (m *Manager) operatorSignInDue(seenGen uint64, now time.Time) (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.claudeLogin
	if st.signInGen == seenGen || st.signInAt.IsZero() {
		return seenGen, false
	}
	if now.Sub(st.signInAt) > operatorSignInRestartWindow {
		return seenGen, false
	}
	return st.signInGen, true
}
