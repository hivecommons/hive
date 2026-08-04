package agent

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// TestInvocationMetadata pins the contract the invocation-attribution trail
// depends on: the EFFECTIVE backend/model (runtime overrides win over config),
// and ok=false for an agent the manager does not know.
func TestInvocationMetadata(t *testing.T) {
	tests := []struct {
		name        string
		agent       *AgentProcess
		wantBackend string
		wantModel   string
	}{
		{
			name:        "config values with no overrides",
			agent:       &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude", Model: "claude-opus-4-6"}},
			wantBackend: "claude",
			wantModel:   "claude-opus-4-6",
		},
		{
			name: "backend override wins over config",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude", Model: "claude-opus-4-6"},
				BackendOverride: "copilot"},
			wantBackend: "copilot",
			wantModel:   "claude-opus-4-6",
		},
		{
			name: "model override wins over config",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude", Model: "claude-opus-4-6"},
				ModelOverride: "claude-haiku-4-5"},
			wantBackend: "claude",
			wantModel:   "claude-haiku-4-5",
		},
		{
			name:        "empty config stays empty (omitted, never guessed)",
			agent:       &AgentProcess{Name: "a"},
			wantBackend: "",
			wantModel:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(acmmLevelPushCapable)
			m.agents["a"] = tc.agent

			backend, model, ok := m.InvocationMetadata("a")
			if !ok {
				t.Fatal("InvocationMetadata: ok=false for a known agent")
			}
			if backend != tc.wantBackend || model != tc.wantModel {
				t.Errorf("InvocationMetadata = (%q, %q), want (%q, %q)",
					backend, model, tc.wantBackend, tc.wantModel)
			}
		})
	}
}

// TestInvocationMetadataUnknownAgent: the caller falls back to static config
// on ok=false, so an unknown name must report exactly that — not guesses.
func TestInvocationMetadataUnknownAgent(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	if backend, model, ok := m.InvocationMetadata("nobody"); ok || backend != "" || model != "" {
		t.Errorf("unknown agent must be (\"\", \"\", false); got (%q, %q, %v)", backend, model, ok)
	}
}
