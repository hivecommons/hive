package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestAgentEnvPairs_ClaudeTokenAndExtras covers the claude-token, BeadsDir,
// CavemanMode, and UID>0 (HOME + token cache + codex home) branches.
func TestAgentEnvPairs_ClaudeTokenAndExtras(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {
			Backend:     "claude",
			Model:       "opus",
			BeadsDir:    "/data/beads/a",
			CavemanMode: "loud",
		},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	// Force a claude token so the CLAUDE_CODE_OAUTH_TOKEN (secret) branch fires.
	m.mu.Lock()
	m.claudeAuthToken = "sk-ant-oauth-test"
	agent := m.agents["a"]
	agent.UID = 2001 // triggers HOME + token cache + codex home dir branches
	m.mu.Unlock()

	found := map[string]bool{}
	var haveClaudeSecret bool
	for _, p := range m.agentEnvPairs(agent) {
		found[p.Key] = true
		if p.Key == "CLAUDE_CODE_OAUTH_TOKEN" && p.Secret {
			haveClaudeSecret = true
		}
	}
	if !haveClaudeSecret {
		t.Error("claude backend with token should set CLAUDE_CODE_OAUTH_TOKEN secret")
	}
	for _, k := range []string{"BD_DIR", "HIVE_CAVEMAN_MODE", "HOME", "HIVE_AGENT_TOKEN_CACHE", "GIT_SSL_CAINFO"} {
		if !found[k] {
			t.Errorf("expected env var %s to be present", k)
		}
	}
}

// TestAgentEnvPairs_AdvisoryStripsTokens: an advisory agent (CanPush false) has
// its gh/git tokens stripped in ensureTmuxSession; agentEnvPairs itself always
// emits HIVE_AGENT_MODE=ADVISORY for such an agent.
func TestAgentEnvPairs_Advisory(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ADVISORY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 1})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	var mode string
	for _, p := range m.agentEnvPairs(agent) {
		if p.Key == "HIVE_AGENT_MODE" {
			mode = p.Value
		}
	}
	if mode != "ADVISORY" {
		t.Errorf("HIVE_AGENT_MODE = %q, want ADVISORY", mode)
	}
}

// TestAgentEnvPairs_CodexUIDHome covers the codex-backend HOME/CODEX_HOME branch.
func TestAgentEnvPairs_CodexUIDHome(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "codex"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["a"]
	agent.UID = 2001
	m.mu.Unlock()
	found := map[string]string{}
	for _, p := range m.agentEnvPairs(agent) {
		found[p.Key] = p.Value
	}
	if found["HOME"] == "" {
		t.Error("codex UID>0 agent should set HOME")
	}
}
