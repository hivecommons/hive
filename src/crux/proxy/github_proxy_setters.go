package proxy

import (
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
)

// SetLinearCredentialResolver installs the resolver injectLinearCredential
// reads on every agent request to api.linear.app. main.go wires the same
// closure the agent manager uses to push LINEAR_ACCESS_TOKEN / LINEAR_API_KEY
// into sessions, so the proxy and the session env can never name different
// credentials. Values are never logged. nil disables injection.
func (p *GitHubProxy) SetLinearCredentialResolver(fn func() agent.LinearCredential) {
	p.linearCredential = fn
}

// SetTokenSink wires the inference token sink so the translator can record
// per-agent usage from upstream OpenAI responses into the shared token store.
func (p *GitHubProxy) SetTokenSink(sink UsageSink) {
	p.tokenSink = sink
}

// SetAgentTokenSource wires the resolver from identified agent name to that
// agent's hub-held scoped token (#1861). Wired once at boot, before Start(),
// so no lock is needed on the read path.
func (p *GitHubProxy) SetAgentTokenSource(source func(agentName string) (string, bool)) {
	p.agentTokenSource = source
}

func (p *GitHubProxy) SetAgentGitHubBudgetConfig(capForAgent func(agentName string) int, reserveFloor func() int) {
	if capForAgent != nil {
		p.githubBudgetCap = capForAgent
	}
	if reserveFloor != nil {
		p.githubReserveFloor = reserveFloor
	}
}

func (p *GitHubProxy) SetAgentGitHubBudgetNudgeFunc(fn func(agentName, message string)) {
	p.githubBudgetNudge = fn
}

// SetCanaryScanner installs the host's canary registry and leak handler.
// enabled gates the scan; failClosed turns a detection (or an unscannable
// body) into a refusal rather than an audit-only event.
func (p *GitHubProxy) SetCanaryScanner(enabled, failClosed bool, reg CanaryScanner, onLeak func(crux.CanaryLeak)) {
	p.canariesEnabled = enabled
	p.canaryFailClosed = failClosed
	p.canaryRegistry = reg
	p.canaryLeakFunc = onLeak
}

// SetRepoPausedFunc installs the operator's per-repo pause predicate (#6203).
// The hive passes config's IsRepoPaused, so pausing a repo in the dashboard is
// enforced on the very next request without a restart — which matters, because
// the pauses this serves (a release freeze, an incident) are measured in hours,
// and an operator who has to roll the pod to quiet one repo is back to stopping
// the whole hive. Passing nil clears it.
func (p *GitHubProxy) SetRepoPausedFunc(fn func(repo string) bool) {
	p.pausedMu.Lock()
	p.repoPaused = fn
	p.pausedMu.Unlock()
}

// SetRepoAutoMergeEnabledFunc installs the live per-repo auto-merge predicate.
// The proxy uses it to give flagged repositories their specific deny reason at
// direct merge endpoints. Passing nil preserves default-on behavior.
func (p *GitHubProxy) SetRepoAutoMergeEnabledFunc(fn func(repo string) bool) {
	p.autoMergeMu.Lock()
	p.repoAutoMergeOn = fn
	p.autoMergeMu.Unlock()
}

// SetAgentRepoScopeFunc installs the per-repo agent-scope predicate (#6204).
// The hive passes config's AgentServesRepo, so re-scoping an agent in the
// dashboard is enforced on the very next request without a restart. Passing nil
// clears it, which is the unscoped behaviour every hive had before.
func (p *GitHubProxy) SetAgentRepoScopeFunc(fn func(agent, repo string) bool) {
	p.scopeMu.Lock()
	p.agentServesRepo = fn
	p.scopeMu.Unlock()
}

// SetWriteSurfaceEnforceFunc installs the per-lane write_surface.enforce
// predicate (#9772). The hive passes config's WriteSurfaceEnforced, so a lane
// listed in config is enforced on the next request. Passing nil clears it,
// which is the behaviour every hive had before: direct writes allowed.
func (p *GitHubProxy) SetWriteSurfaceEnforceFunc(fn func(agent string) bool) {
	p.writeSurfaceMu.Lock()
	p.writeSurfaceEnforced = fn
	p.writeSurfaceMu.Unlock()
}

// SetWriteRefusedAuditFunc installs the sink that records a direct write
// refused under write_surface.enforce as an agent_write_refused audit entry.
func (p *GitHubProxy) SetWriteRefusedAuditFunc(fn func(agent, kind, method, path, repo string)) {
	p.writeSurfaceMu.Lock()
	p.writeRefusedAudit = fn
	p.writeSurfaceMu.Unlock()
}
