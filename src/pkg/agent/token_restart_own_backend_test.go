package agent

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestAgentOwnBackendHasCredential pins #11897: a valid credential for another
// backend must not let the token-triggered restart heal fire.
func TestAgentOwnBackendHasCredential(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(6 * time.Hour).UnixMilli(),
	})
	origCopilot := sharedCopilotConfigPath
	origProbe := copilotUserTokenProbePath
	dir := t.TempDir()
	sharedCopilotConfigPath = filepath.Join(dir, "absent-config.json")
	copilotUserTokenProbePath = filepath.Join(dir, "absent-user-token")
	t.Cleanup(func() {
		sharedCopilotConfigPath = origCopilot
		copilotUserTokenProbePath = origProbe
	})

	m := NewManager(map[string]config.AgentConfig{
		"cop":    {Backend: "copilot"},
		"claude": {Backend: "claude"},
		"agy":    {Backend: "agy"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})

	tests := []struct {
		name  string
		agent string
		want  bool
	}{
		{"copilot agent with only a claude credential", "cop", false},
		{"claude agent with a claude credential", "claude", true},
		{"agy agent with only a claude credential", "agy", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.mu.RLock()
			agent := m.agents[tc.agent]
			m.mu.RUnlock()
			if got := m.agentOwnBackendHasCredential(agent); got != tc.want {
				t.Errorf("agentOwnBackendHasCredential(%s) = %v, want %v", tc.agent, got, tc.want)
			}
		})
	}
	if m.agentOwnBackendHasCredential(nil) {
		t.Error("nil agent must not read as credentialed")
	}
}
