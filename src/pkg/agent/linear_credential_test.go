package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func linearCredManager(t *testing.T, mode string, cred LinearCredential) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: mode},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	m.SetLinearCredentialResolver(func() LinearCredential { return cred })
	m.mu.RLock()
	a := m.agents["a"]
	m.mu.RUnlock()
	return m, a
}

func envByKey(m *Manager, a *AgentProcess) map[string]agentEnvPair {
	out := map[string]agentEnvPair{}
	for _, p := range m.agentEnvPairs(a) {
		out[p.Key] = p
	}
	return out
}

// An ISSUES_ONLY agent — the tier at which the proxy first permits a Linear
// mutation — gets the OAuth token, as a secret, under the Bearer-form name.
func TestLinearCredential_IssuesOnlyGetsAccessToken(t *testing.T) {
	m, a := linearCredManager(t, "ISSUES_ONLY", LinearCredential{AccessToken: "lin_oauth_x"})
	env := envByKey(m, a)
	p, ok := env["LINEAR_ACCESS_TOKEN"]
	if !ok || p.Value != "lin_oauth_x" || !p.Secret {
		t.Fatalf("LINEAR_ACCESS_TOKEN = %+v (present=%v), want secret lin_oauth_x", p, ok)
	}
	if _, ok := env["LINEAR_API_KEY"]; ok {
		t.Error("LINEAR_API_KEY must not be set when the OAuth token is available")
	}
}

// Without a connected workspace the work-source API key is the fallback.
func TestLinearCredential_APIKeyFallback(t *testing.T) {
	m, a := linearCredManager(t, "ISSUES_AND_PRS", LinearCredential{APIKey: "lin_api_y"})
	env := envByKey(m, a)
	p, ok := env["LINEAR_API_KEY"]
	if !ok || p.Value != "lin_api_y" || !p.Secret {
		t.Fatalf("LINEAR_API_KEY = %+v (present=%v), want secret lin_api_y", p, ok)
	}
	if _, ok := env["LINEAR_ACCESS_TOKEN"]; ok {
		t.Error("LINEAR_ACCESS_TOKEN must not be set without an OAuth token")
	}
}

// Advisory agents stay credential-less (GitHub parity: no GITHUB_TOKEN below
// the push tier), even when a credential is configured.
func TestLinearCredential_AdvisoryGetsNothing(t *testing.T) {
	m, a := linearCredManager(t, "ADVISORY", LinearCredential{AccessToken: "x", APIKey: "y"})
	env := envByKey(m, a)
	for _, k := range []string{"LINEAR_ACCESS_TOKEN", "LINEAR_API_KEY"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not be injected into an ADVISORY agent", k)
		}
	}
}

// SECURITY INVARIANT (#4891 review blocker): a live agent downgraded below
// the ISSUES_ONLY floor must have BOTH Linear credential vars actively unset
// by the next refresh tick. The creation-time strip in ensureTmuxSession
// fires only when the session is created; a downgrade leaves the session
// alive, and tmux forks a fresh CLI per turn, so without the tick-time unset
// every post-downgrade turn would inherit the last pushed value — and
// LINEAR_API_KEY never expires.
func TestLinearCredential_RefreshTickStripsAfterDowngrade(t *testing.T) {
	m, a := linearCredManager(t, "ISSUES_ONLY", LinearCredential{APIKey: "lin_api_y"})
	a.tmuxSession = "hive-a"

	// Pin the starting state: at ISSUES_ONLY the tick pushes the credential.
	// The invariant under test is the TRANSITION away from it.
	pre := m.linearRefreshTmuxArgs(a)
	if len(pre) != 1 || pre[0][len(pre[0])-2] != "LINEAR_API_KEY" {
		t.Fatalf("pre-downgrade refresh args = %v, want a single LINEAR_API_KEY push", pre)
	}

	// Downgrade below the floor. The session — and whatever value the last
	// tick pushed into it — survives, so the next tick must emit unsets for
	// BOTH vars, unconditionally on which one (if either) was set before.
	a.Config.Mode = "ADVISORY"
	stripped := map[string]bool{"LINEAR_ACCESS_TOKEN": false, "LINEAR_API_KEY": false}
	for _, cmd := range m.linearRefreshTmuxArgs(a) {
		unset := false
		for i, tok := range cmd {
			if tok == "-u" && i+1 < len(cmd) {
				unset = true
				if _, ok := stripped[cmd[i+1]]; ok {
					stripped[cmd[i+1]] = true
				}
			}
		}
		if !unset {
			t.Errorf("post-downgrade refresh emitted a non-unset command: %v", cmd)
		}
	}
	for k, ok := range stripped {
		if !ok {
			t.Errorf("post-downgrade refresh did not unset %s", k)
		}
	}
}

// No resolver (tests / GitHub-only hives) injects nothing and never panics.
func TestLinearCredential_NoResolver(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.RLock()
	a := m.agents["a"]
	m.mu.RUnlock()
	env := envByKey(m, a)
	for _, k := range []string{"LINEAR_ACCESS_TOKEN", "LINEAR_API_KEY"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s injected with no resolver", k)
		}
	}
	m.SetLinearCredentialResolver(nil)
	if !m.linearCredential().Empty() {
		t.Error("nil resolver should yield an empty credential")
	}
}
