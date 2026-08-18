package github

// Tests for the hub-side half of proxy credential injection (#1861): under
// the opt-in flag, WriteAgentToken must divert the real scoped token into the
// proxy's in-memory registry and hand the agent-visible cache file only the
// inert placeholder; with the flag off (the fleet default), delivery must be
// byte-identical to before — real token in the file, registry untouched.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// resetProxyTokenRegistry empties the package-level registry so tests do not
// observe each other's entries.
func resetProxyTokenRegistry(t *testing.T) {
	t.Helper()
	agentProxyTokensMu.Lock()
	agentProxyTokens = make(map[string]string)
	agentProxyTokensMu.Unlock()
	t.Cleanup(func() {
		agentProxyTokensMu.Lock()
		agentProxyTokens = make(map[string]string)
		agentProxyTokensMu.Unlock()
	})
}

// TestWriteAgentToken_InjectionDivertsRealTokenToRegistry: with the flag on,
// nothing the agent can read may hold the real credential. The attack this
// closes is #1861's core: a prompt-injected agent cat'ing its own token cache
// (or echoing $GH_TOKEN) and posting the value somewhere public — after the
// divert, all it can leak is the self-describing placeholder.
func TestWriteAgentToken_InjectionDivertsRealTokenToRegistry(t *testing.T) {
	const realToken = "ghs-real-scoped-token"
	const agentName = "guide"
	t.Setenv(config.ProxyInjectGHAuthEnv, "true")
	resetProxyTokenRegistry(t)

	auth, _, closeFn := newFakeAppAuth(t, realToken)
	defer closeFn()
	useTempCacheDir(t)

	if err := auth.WriteAgentToken(context.Background(), agentName, "advisor", 2001); err != nil {
		t.Fatalf("WriteAgentToken: %v", err)
	}

	fileBytes, err := os.ReadFile(AgentTokenCachePath(agentName))
	if err != nil {
		t.Fatalf("reading agent cache file: %v", err)
	}
	fileContent := string(fileBytes)

	if strings.Contains(fileContent, realToken) {
		t.Fatalf("agent-readable cache contains the REAL token under injection mode: %q", fileContent)
	}
	if want := AgentDummyToken(agentName); fileContent != want {
		t.Errorf("cache file = %q, want the placeholder %q", fileContent, want)
	}
	// The placeholder must be visibly fake: self-describing, attributable to
	// the agent, and not shaped like a GitHub token.
	if !strings.Contains(fileContent, agentName) || strings.HasPrefix(fileContent, "ghs_") || strings.HasPrefix(fileContent, "ghp_") {
		t.Errorf("placeholder %q is not visibly fake/attributable", fileContent)
	}

	got, ok := AgentProxyToken(agentName)
	if !ok || got != realToken {
		t.Errorf("AgentProxyToken(%q) = (%q, %v), want the real token for the proxy to inject", agentName, got, ok)
	}
}

// TestWriteAgentToken_FlagOffDeliveryUnchanged: with the flag unset, the
// pre-#1861 lane must be untouched — the agent gets the real token in its
// cache file and the proxy registry never learns it. This is the fleet-safety
// guarantee that lets this ship dark and soak.
func TestWriteAgentToken_FlagOffDeliveryUnchanged(t *testing.T) {
	const realToken = "ghs-real-scoped-token-flagoff"
	const agentName = "scanner"
	// Explicitly clear rather than assuming the runner env: t.Setenv also
	// restores the prior value afterwards.
	t.Setenv(config.ProxyInjectGHAuthEnv, "")
	resetProxyTokenRegistry(t)

	auth, _, closeFn := newFakeAppAuth(t, realToken)
	defer closeFn()
	useTempCacheDir(t)

	if err := auth.WriteAgentToken(context.Background(), agentName, "advisor", 2001); err != nil {
		t.Fatalf("WriteAgentToken: %v", err)
	}

	fileBytes, err := os.ReadFile(AgentTokenCachePath(agentName))
	if err != nil {
		t.Fatalf("reading agent cache file: %v", err)
	}
	if string(fileBytes) != realToken {
		t.Errorf("flag-off cache file = %q, want the real token %q (delivery must be unchanged)", fileBytes, realToken)
	}
	if tok, ok := AgentProxyToken(agentName); ok {
		t.Errorf("flag-off registry holds a token (%q) — the registry must only be fed under the flag", tok)
	}
}
