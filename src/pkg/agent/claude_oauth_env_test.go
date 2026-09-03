package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// stageSharedClaudeCredential points the shared credential path at a temp file
// holding the given token set, and returns the path.
func stageSharedClaudeCredential(t *testing.T, oauth map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if oauth != nil {
		data, err := json.Marshal(map[string]any{"claudeAiOauth": oauth})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := sharedClaudeCredentialPath
	sharedClaudeCredentialPath = path
	t.Cleanup(func() { sharedClaudeCredentialPath = orig })

	// Redirect BOTH home roots agentClaudeCredentialPaths can resolve to, or
	// the assertions read a live credential instead of the fixture: the shared
	// /data/home on a developer box that happens to run a hive, and — for an
	// agent with no per-agent UID — the running user's own $HOME.
	origHome := sharedAgentHome
	sharedAgentHome = filepath.Join(t.TempDir(), "home")
	t.Cleanup(func() { sharedAgentHome = origHome })
	t.Setenv("HOME", filepath.Join(t.TempDir(), "user-home"))

	return path
}

func claudeAgentFixture(t *testing.T, token string) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "claude-sonnet-5"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.Lock()
	m.claudeAuthToken = token
	m.mu.Unlock()
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if agent == nil {
		t.Fatal("fixture agent not registered")
	}
	return m, agent
}

func envPairsByKey(m *Manager, agent *AgentProcess) map[string]agentEnvPair {
	out := map[string]agentEnvPair{}
	for _, p := range m.agentEnvPairs(agent) {
		out[p.Key] = p
	}
	return out
}

// TestClaudeOAuthEnv_NotInjectedOverAReadableCredential is the defect of #5454.
//
// CLAUDE_CODE_OAUTH_TOKEN is a static bearer override: with it set, Claude Code
// uses the value verbatim and never opens the credentials file, so it can never
// refresh. The value hive injected was a snapshot of the SHORT-LIVED access
// token taken once at manager construction, which pinned every claude agent to
// the 8h life of whatever token was on disk at container start. Once the
// agent can read the credential itself (true since per-agent homes, #4619),
// injecting the override only takes away the CLI's ability to recover.
func TestClaudeOAuthEnv_NotInjectedOverAReadableCredential(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(6 * time.Hour).UnixMilli(),
	})
	m, agent := claudeAgentFixture(t, "sk-ant-oat-snapshot-from-boot")

	if _, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("injected the static token override over a credential file the CLI can read and refresh")
	}
}

// The same must hold for the state a fleet enters roughly once a day: the
// access token has aged out, the refresh grant has not, and the next CLI start
// mints a new token from the file. Re-injecting the override here would defeat
// the recovery at the exact moment it was about to happen.
func TestClaudeOAuthEnv_NotInjectedOverARefreshableCredential(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken":           "sk-ant-oat-aged-out",
		"expiresAt":             time.Now().Add(-2 * time.Hour).UnixMilli(),
		"refreshToken":          "sk-ant-ort-live",
		"refreshTokenExpiresAt": time.Now().Add(28 * 24 * time.Hour).UnixMilli(),
	})
	m, agent := claudeAgentFixture(t, "sk-ant-oat-snapshot-from-boot")

	if _, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("injected the static token override over a credential the CLI was about to refresh")
	}
}

// The job the variable was added for (c5648bc9) must survive: an agent with no
// credential file it can read still gets the dashboard-obtained token, as a
// secret pair so the value never reaches a command line.
func TestClaudeOAuthEnv_StillInjectedWithNoReadableCredential(t *testing.T) {
	stageSharedClaudeCredential(t, nil) // path exists in name only; no file written
	m, agent := claudeAgentFixture(t, "sk-ant-oat-from-dashboard-login")

	pair, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]
	if !ok {
		t.Fatal("an agent with no readable credential must still receive the injected token")
	}
	if pair.Value != "sk-ant-oat-from-dashboard-login" {
		t.Fatalf("value = %q", pair.Value)
	}
	if !pair.Secret {
		t.Fatal("the token must stay a secret pair — it must never reach a command line or pane scrollback")
	}
}

// A spent credential (expired, no refresh grant) is not something the CLI can
// recover from, so the fallback still applies.
func TestClaudeOAuthEnv_InjectedOverASpentCredential(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-spent",
		"expiresAt":   time.Now().Add(-time.Hour).UnixMilli(),
	})
	m, agent := claudeAgentFixture(t, "sk-ant-oat-from-dashboard-login")

	if _, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; !ok {
		t.Fatal("a spent credential leaves nothing for the CLI to refresh; the fallback must still inject")
	}
}

// Non-claude backends never carried this variable and still must not.
func TestClaudeOAuthEnv_NotInjectedForOtherBackends(t *testing.T) {
	stageSharedClaudeCredential(t, nil)
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.Lock()
	m.claudeAuthToken = "sk-ant-oat-from-dashboard-login"
	m.mu.Unlock()
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	if _, ok := envPairsByKey(m, agent)["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("a copilot agent must not receive a Claude token")
	}
}
