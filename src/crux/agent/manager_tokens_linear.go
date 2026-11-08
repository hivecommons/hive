package agent

import "github.com/hivecommons/hive/crux"

// LinearCredential is crux.LinearCredential, the host-provided Linear
// write credential.
type LinearCredential = crux.LinearCredential

// SetLinearCredentialResolver injects the resolver for the Linear write
// credential. Called from main.go with a closure over the live Linear agent
// install + config, so a workspace connected after boot is picked up on the
// next launch or hourly refresh. The resolver returns credential VALUES,
// which are never logged. A nil fn clears it (no Linear credential injected).
func (m *Manager) SetLinearCredentialResolver(fn func() LinearCredential) {
	// Atomic store — no m.mu — so linearCredential can be read lock-free from
	// the lock-holding launch path (see bobAPIKeyResolver docs).
	if fn == nil {
		m.linearCredentialResolver.Store(nil)
		return
	}
	m.linearCredentialResolver.Store(&fn)
}

// linearCredential returns the current Linear credential, or the zero value
// when none is configured (or no resolver was injected, as in tests/bare
// setups). Safe to call while holding m.mu.
func (m *Manager) linearCredential() LinearCredential {
	fnp := m.linearCredentialResolver.Load()
	if fnp == nil || *fnp == nil {
		return LinearCredential{}
	}
	return (*fnp)()
}

// linearEnvPairs renders the Linear credential as env pairs for one agent, or
// nil when the agent may not hold one. Gated on CanCreateIssues() — the
// ISSUES_ONLY floor — because that is the tier at which the proxy's Linear
// gate (pkg/proxy/linear_rules.go) first permits a mutation: below it the
// credential could only ever be used for reads the hive already performs on
// the agent's behalf, and GitHub parity says advisory agents stay token-less.
// Both values are Secret so they never reach the shell command line.
func (m *Manager) linearEnvPairs(agent *AgentProcess) []agentEnvPair {
	if !m.agentMode(agent).CanCreateIssues() {
		return nil
	}
	cred := m.linearCredential()
	switch {
	case cred.AccessToken != "":
		return []agentEnvPair{{linearAccessTokenEnvVar, cred.AccessToken, true}}
	case cred.APIKey != "":
		return []agentEnvPair{{linearAPIKeyEnvVar, cred.APIKey, true}}
	}
	return nil
}

// linearRefreshTmuxArgs returns the tmux invocations the refresh tick applies
// to one agent's session for the Linear credential: set-environment pushes of
// the current credential for ISSUES_ONLY+ agents, or explicit unsets ("-u") of
// BOTH credential vars for agents below the floor. The unsets exist because
// the creation-time strip in ensureTmuxSession fires only when the session is
// created (it early-returns on an existing session): a live agent downgraded
// below ISSUES_ONLY keeps its session, and tmux forks a fresh CLI per turn, so
// without them every post-downgrade turn would inherit the last pushed value
// indefinitely — LINEAR_API_KEY never expires. They are unconditional on prior
// state (set-environment -r is idempotent) so a downgrade is closed on the
// first tick regardless of which variable, if any, was pushed before.
//
// -r, not -u: -u only deletes the SESSION entry, and when the variable also
// sits in the server's GLOBAL environment (inherited from the hive process —
// LINEAR_API_KEY is exactly such a variable) the global value shows through
// to the next pane. -r records a removal that hides the global value from
// every process the server forks afterwards; a later plain set overrides it,
// so the ISSUES_ONLY+ push above is unaffected. See applySessionEnv.
// Callers must skip agents with no tmux session.
func (m *Manager) linearRefreshTmuxArgs(a *AgentProcess) [][]string {
	if !m.agentMode(a).CanCreateIssues() {
		return [][]string{
			{"set-environment", "-t", a.tmuxSession, "-r", linearAccessTokenEnvVar},
			{"set-environment", "-t", a.tmuxSession, "-r", linearAPIKeyEnvVar},
		}
	}
	var out [][]string
	for _, p := range m.linearEnvPairs(a) {
		out = append(out, []string{"set-environment", "-t", a.tmuxSession, p.Key, p.Value})
	}
	return out
}

// Environment variables carrying the Linear credential into an agent session.
// LINEAR_API_KEY is the name Linear's own SDK, MCP server, and CLI read for a
// personal key; LINEAR_ACCESS_TOKEN is the OAuth (Bearer) form. The kick's
// work-tracker section (pkg/scheduler) tells the agent which header each one
// maps to.
const (
	linearAccessTokenEnvVar = "LINEAR_ACCESS_TOKEN"
	linearAPIKeyEnvVar      = "LINEAR_API_KEY"
)
