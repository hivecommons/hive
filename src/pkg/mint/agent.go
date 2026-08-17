package mint

import (
	"fmt"
	"time"
)

// This file is the agent-credential integration for the mint: it turns an
// agent's trust tier (the same string pkg/agent's AgentMode.TokenTier() yields —
// "advisor"/"newcomer"/"contributor"/"trusted") into a scoped, short-lived JWT
// the agent can request ALONGSIDE its existing GitHub App token. It never
// replaces the App token path — it is an additional, opt-in credential.
//
// Tiers are passed as plain strings (not an AgentMode) on purpose so pkg/mint
// stays free of a pkg/agent import; the caller already has the tier in hand at
// the WriteAgentToken call site.

// agentTokenAudience is the audience embedded in agent-scoped mint tokens. A
// downstream WIF/verifier is configured to accept this audience for hive agent
// workloads. It is a stable constant (not a secret) that identifies the intended
// consumer class of the token.
const agentTokenAudience = "hive-agent"

// Scope strings granted to minted agent tokens. They mirror the capability
// ladder AgentMode encodes (advisory → issues → issues+PRs → +merge) but live
// here as mint-native scope names so a WIF policy can key off them directly.
const (
	// ScopeIssuesRead lets the bearer read issue/PR metadata. Every tier gets it.
	ScopeIssuesRead = "issues:read"
	// ScopeIssuesWrite lets the bearer create/comment on issues.
	ScopeIssuesWrite = "issues:write"
	// ScopeContentsWrite lets the bearer push branches / open PRs.
	ScopeContentsWrite = "contents:write"
	// ScopePullsWrite lets the bearer create/update pull requests.
	ScopePullsWrite = "pulls:write"
	// ScopeMerge lets the bearer merge pull requests.
	ScopeMerge = "pulls:merge"
)

// tierScopes maps a trust-tier string to the scopes an agent at that tier is
// entitled to. Unknown tiers fall through to the least-privilege advisor set
// (read-only) — fail closed, never widen on a typo.
var tierScopes = map[string][]string{
	"advisor":     {ScopeIssuesRead},
	"newcomer":    {ScopeIssuesRead, ScopeIssuesWrite},
	"contributor": {ScopeIssuesRead, ScopeIssuesWrite, ScopeContentsWrite, ScopePullsWrite},
	"trusted":     {ScopeIssuesRead, ScopeIssuesWrite, ScopeContentsWrite, ScopePullsWrite, ScopeMerge},
}

// ScopesForTier returns the scope set for a trust tier. An unrecognized tier
// yields the least-privilege read-only set (fail closed). The returned slice is
// a fresh copy the caller may mutate.
func ScopesForTier(tier string) []string {
	scopes, ok := tierScopes[tier]
	if !ok {
		scopes = tierScopes["advisor"]
	}
	return append([]string(nil), scopes...)
}

// AgentMinter is the opt-in agent-credential facade over a Minter. When the mint
// is disabled it is a nil-safe no-op: MintAgentToken returns ("", nil) so a
// caller can request a mint credential unconditionally and simply get nothing
// back when the feature is off — never an error, never a broken code path.
type AgentMinter struct {
	minter *Minter // nil ⇒ disabled (no-op)
	ttl    time.Duration
}

// NewAgentMinter wraps a Minter for agent-token issuance with the given per-token
// TTL. A nil minter yields a disabled AgentMinter (all MintAgentToken calls
// no-op). ttl<=0 falls back to the minter's configured max.
func NewAgentMinter(m *Minter, ttl time.Duration) *AgentMinter {
	return &AgentMinter{minter: m, ttl: ttl}
}

// Enabled reports whether this AgentMinter will actually issue tokens.
func (a *AgentMinter) Enabled() bool { return a != nil && a.minter != nil }

// MintAgentToken issues a scoped, short-lived JWT for the named agent, with
// scopes derived from its trust tier. subject is the agent name so a verifier
// can attribute the token, and the audience is the fixed hive-agent audience.
//
// When the AgentMinter is disabled (mint off) it returns ("", nil): no token, no
// error — the caller keeps using its GitHub App token unchanged. When enabled it
// returns a signed token verifiable against the mint's JWKS. An empty agentName
// is a caller error and returns a non-nil error (fail closed rather than mint an
// unattributable token).
func (a *AgentMinter) MintAgentToken(agentName, tier string) (string, error) {
	if !a.Enabled() {
		return "", nil
	}
	if agentName == "" {
		return "", fmt.Errorf("mint: agent name is required")
	}
	return a.minter.Mint(agentName, agentTokenAudience, ScopesForTier(tier), a.ttl)
}
