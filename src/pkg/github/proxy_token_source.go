package github

import "sync"

// This file is the hub-side half of proxy-side GitHub credential injection
// (#1861). When config.ProxyInjectGHAuth() is on, WriteAgentToken diverts the
// freshly-minted tier-scoped token HERE — an in-memory, hub-process-only
// registry the MITM proxy reads per request — and writes a visibly-fake
// placeholder to the agent-readable cache file instead. The agent's tooling
// (gh-wrapper.sh, git-credential-hive.sh, the manager's GITHUB_TOKEN env
// injection) keeps functioning because each still finds a syntactically-valid
// credential where it expects one, but that credential authenticates nowhere:
// the proxy strips it off every upstream request and substitutes the real
// scoped token from this registry.
//
// The registry is package-level, NOT a field on AppAuth, deliberately: the
// hive re-creates its AppAuth at runtime (key rotation, App re-discovery —
// see the `appAuth = newAppAuth` sites in cmd/hive/main.go), while the proxy
// is wired to its token source exactly once at boot. Hanging the map off an
// AppAuth instance would silently strand the proxy on the pre-rotation
// instance's (empty, stale) map. A package-level store keyed by agent name
// survives AppAuth replacement, exactly like agentTokenCacheDir does for the
// file-based lane.
//
// Lifecycle: entries are overwritten on every mint for the same agent (launch,
// relaunch, and the hourly refreshAgentTokens sweep — the same #3967 cadence
// that keeps the file cache fresh, reused rather than duplicated). Entries for
// removed agents linger until process restart; that is accepted — the
// underlying installation token expires within the hour regardless, so a
// lingering entry decays into a useless string, and it never leaves this
// process.
var (
	agentProxyTokensMu sync.RWMutex
	agentProxyTokens   = make(map[string]string)
)

// agentDummyTokenPrefix is the prefix of the placeholder written to the
// agent-visible token cache under injection mode. It is deliberately NOT a
// GitHub token shape (no ghs_/ghp_ prefix) and self-describing, so that if an
// agent leaks it — into a log, a PR body, a prompt transcript — the leak is
// inert AND immediately diagnosable as the injection placeholder rather than
// mistaken for a live credential.
const agentDummyTokenPrefix = "hive-proxy-injected-"

// AgentDummyToken returns the placeholder credential delivered to an agent in
// place of its real scoped token when proxy-side injection (#1861) is active.
// Including the agent name makes any leak attributable at a glance.
func AgentDummyToken(agentName string) string {
	return agentDummyTokenPrefix + agentName
}

// storeAgentProxyToken records an agent's freshly-minted scoped token for the
// proxy to inject. Called only from WriteAgentToken under the injection flag.
func storeAgentProxyToken(agentName, token string) {
	agentProxyTokensMu.Lock()
	agentProxyTokens[agentName] = token
	agentProxyTokensMu.Unlock()
}

// AgentProxyToken resolves an agent name to its hub-held scoped token for
// proxy-side injection (#1861). ok is false when no token has been minted for
// that agent in this process's lifetime — the proxy then injects NOTHING and
// lets the request fail loud at GitHub (401), never falling back to a shared
// or ambient token (that would recreate the pre-#3888 identity hole where an
// unattributed caller could ride another identity's credential).
func AgentProxyToken(agentName string) (string, bool) {
	agentProxyTokensMu.RLock()
	token, ok := agentProxyTokens[agentName]
	agentProxyTokensMu.RUnlock()
	return token, ok && token != ""
}
