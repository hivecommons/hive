package github

import (
	"os"
	"sync"

	"github.com/hivecommons/hive/pkg/credsidecar"
)

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

// Sidecar mode (#9586 phase 2). With HIVE_CRED_SIDECAR_URL set on top of
// injection, the real token never exists in this process at all: the isolated
// credential sidecar (pkg/credsidecar) holds the App key and mints per tier,
// and WriteAgentToken records here only WHICH tier each agent is entitled to.
// The proxy signs that tier into each request it sends to the sidecar. A tier
// name is not a credential, so nothing in this registry is worth stealing.
var (
	agentProxyTiersMu sync.RWMutex
	agentProxyTiers   = make(map[string]string)
)

// credSidecarEnabled reports whether this process runs in sidecar mode. It
// reads the same variable through the same reader as the proxy and the boot
// guard (credsidecar.Enabled), so the three can never disagree.
func credSidecarEnabled() bool {
	return credsidecar.Enabled(os.Getenv)
}

// storeAgentProxyTier records an agent's token tier for sidecar mode. Called
// only from WriteAgentToken, in place of a mint.
func storeAgentProxyTier(agentName, tier string) {
	agentProxyTiersMu.Lock()
	agentProxyTiers[agentName] = tier
	agentProxyTiersMu.Unlock()
}

// AgentProxyTier resolves an agent name to the token tier the sidecar should
// attach for it (#9586 phase 2). ok is false when WriteAgentToken has not run
// for that agent in sidecar mode during this process's lifetime; the proxy
// then asks the sidecar to forward with NO credential, the same fail-loud
// posture as AgentProxyToken's miss.
func AgentProxyTier(agentName string) (string, bool) {
	agentProxyTiersMu.RLock()
	tier, ok := agentProxyTiers[agentName]
	agentProxyTiersMu.RUnlock()
	return tier, ok && tier != ""
}
