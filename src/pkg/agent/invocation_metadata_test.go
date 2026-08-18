package agent

import (
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// TestInvocationMetadata pins the contract the invocation-attribution trail
// depends on: the EFFECTIVE backend/model (runtime overrides win over config),
// the launcher-resolved effort (agy's required --effort pairing; empty —
// omitted, never guessed — everywhere else), and ok=false for an agent the
// manager does not know.
func TestInvocationMetadata(t *testing.T) {
	tests := []struct {
		name        string
		agent       *AgentProcess
		wantBackend string
		wantModel   string
		wantEffort  string
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
		{
			// #4083: agy is launched with --effort agyDefaultEffort whenever a
			// model is given (agy requires the pair), so the attribution trail
			// records that same effort.
			name:        "agy with a model reports the launcher's effort",
			agent:       &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "agy", Model: "gemini-3-pro"}},
			wantBackend: "agy",
			wantModel:   "gemini-3-pro",
			wantEffort:  agyDefaultEffort,
		},
		{
			// agy without a model is launched with NO --model/--effort pair, so
			// no effort is recorded.
			name:        "agy without a model reports no effort",
			agent:       &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "agy"}},
			wantBackend: "agy",
			wantModel:   "",
			wantEffort:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(acmmLevelPushCapable)
			m.agents["a"] = tc.agent

			backend, model, effort, ok := m.InvocationMetadata("a")
			if !ok {
				t.Fatal("InvocationMetadata: ok=false for a known agent")
			}
			if backend != tc.wantBackend || model != tc.wantModel || effort != tc.wantEffort {
				t.Errorf("InvocationMetadata = (%q, %q, %q), want (%q, %q, %q)",
					backend, model, effort, tc.wantBackend, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// TestInvocationMetadataUnknownAgent: the caller falls back to static config
// on ok=false, so an unknown name must report exactly that — not guesses.
func TestInvocationMetadataUnknownAgent(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	if backend, model, effort, ok := m.InvocationMetadata("nobody"); ok || backend != "" || model != "" || effort != "" {
		t.Errorf("unknown agent must be (\"\", \"\", \"\", false); got (%q, %q, %q, %v)", backend, model, effort, ok)
	}
}
