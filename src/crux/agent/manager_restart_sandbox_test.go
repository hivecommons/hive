package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// A restart of a sandboxed agent must refuse instead of creating an
// interactive tmux session on the hive pod (#18826).
func TestRestartRefusesSandboxedAgent(t *testing.T) {
	m := testManager(5)
	m.sandboxConfig = config.AgentSandboxConfig{Enabled: true}
	enabled := true
	m.agents["a"] = &AgentProcess{
		Name:   "a",
		State:  StateStopped,
		Config: config.AgentConfig{Sandbox: &config.AgentSandboxOverride{Enabled: &enabled}},
	}
	err := m.Restart(t.Context(), "a")
	if err == nil || !strings.Contains(err.Error(), "runs in sandbox") {
		t.Fatalf("Restart must refuse a sandboxed agent, got %v", err)
	}
	if _, err := m.RestartMissingSessionWithReason(t.Context(), "a", "test"); err == nil {
		t.Fatal("RestartMissingSessionWithReason must refuse a sandboxed agent")
	}
}
