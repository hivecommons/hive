package github

// Tests for the hub-side half of proxy credential injection (#1861): under
// the opt-in flag, WriteAgentToken must divert the real scoped token into the
// proxy's in-memory registry and hand the agent-visible cache file only the
// inert placeholder; with the flag off (the fleet default), delivery must be
// byte-identical to before — real token in the file, registry untouched.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
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

// TestInjectionMode_NoRealTokenInAnyAgentReadableCacheFile is the #9586
// acceptance invariant for the cache side: with injection on, across several
// agents and a refresh cycle, NO file in the agent-readable token cache
// directory contains a real token, every agent's token file is exactly its
// hive-proxy-injected-<agent> placeholder, and the hub-held registry has the
// real token for the proxy to inject. The GH_TOKEN / GITHUB_TOKEN an agent sees
// are read from these files (gh-wrapper.sh, git-credential-hive.sh, the
// manager's GITHUB_TOKEN push), so this also bounds the env lane. The log is
// checked too: a token in the hive log is one `cat` away for any operator tool.
func TestInjectionMode_NoRealTokenInAnyAgentReadableCacheFile(t *testing.T) {
	const realToken = "ghs_realScopedTokenForInvariant"
	agents := []string{"scanner", "reviewer", "outreach"}
	t.Setenv(config.ProxyInjectGHAuthEnv, config.ProxyInjectGHAuthOnValue)
	resetProxyTokenRegistry(t)

	auth, logBuf, closeFn := newFakeAppAuth(t, realToken)
	defer closeFn()
	dir := useTempCacheDir(t)

	const refreshCycles = 2 // launch mint + one hourly refresh
	for cycle := 0; cycle < refreshCycles; cycle++ {
		for i, name := range agents {
			if err := auth.WriteAgentToken(context.Background(), name, "advisor", 2001+i); err != nil {
				t.Fatalf("cycle %d WriteAgentToken(%s): %v", cycle, name, err)
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("cache dir is empty - the invariant below would pass vacuously")
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), realToken) {
			t.Errorf("agent-readable cache file %s contains the real token under injection", e.Name())
		}
	}

	placeholder := regexp.MustCompile(`^hive-proxy-injected-([a-z0-9-]+)$`)
	for _, name := range agents {
		b, err := os.ReadFile(AgentTokenCachePath(name))
		if err != nil {
			t.Fatalf("reading %s cache: %v", name, err)
		}
		m := placeholder.FindStringSubmatch(string(b))
		if m == nil || m[1] != name {
			t.Errorf("%s cache = %q, want exactly the placeholder %q", name, b, AgentDummyToken(name))
		}
		if got, ok := AgentProxyToken(name); !ok || got != realToken {
			t.Errorf("AgentProxyToken(%s) = (%q, %v), want the real token", name, got, ok)
		}
	}

	if strings.Contains(logBuf.String(), realToken) {
		t.Error("the real token appeared in the hive log")
	}
}
