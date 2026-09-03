package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestInvocationMetadata pins the contract the invocation-attribution trail
// depends on: the EFFECTIVE backend/model/effort (runtime overrides win over config),
// and ok=false for an agent the manager does not know.
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
			wantEffort:  "",
		},
		{
			name: "backend override wins over config",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude", Model: "claude-opus-4-6"},
				BackendOverride: "copilot"},
			wantBackend: "copilot",
			wantModel:   "claude-opus-4-6",
			wantEffort:  "",
		},
		{
			name: "model override wins over config",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude", Model: "claude-opus-4-6"},
				ModelOverride: "claude-haiku-4-5"},
			wantBackend: "claude",
			wantModel:   "claude-haiku-4-5",
			wantEffort:  "",
		},
		{
			name:        "agy with model resolves default effort",
			agent:       &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "agy", Model: "gemini-3.7-flash"}},
			wantBackend: "agy",
			wantModel:   "gemini-3.7-flash",
			wantEffort:  "low",
		},
		{
			name:        "agy without model has no effort",
			agent:       &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "agy"}},
			wantBackend: "agy",
			wantModel:   "",
			wantEffort:  "",
		},
		{
			name:        "empty config stays empty (omitted, never guessed)",
			agent:       &AgentProcess{Name: "a"},
			wantBackend: "",
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

// TestResolveReasoningEffort pins the single rule both attribution resolvers use.
// It exists because cmd/hive's config fallback previously carried its own
// hardcoded "low": the two paths could disagree, and the one that stamps PR
// bodies would have kept reporting an effort agy was no longer launched with.
func TestResolveReasoningEffort(t *testing.T) {
	cases := []struct {
		backend, model, want string
	}{
		// agy REQUIRES --effort whenever --model is given.
		{"agy", "gemini-3.7-flash", agyDefaultEffort},
		// No model means agy is given no --effort at all, so claiming one
		// would advertise an effort agy never applied.
		{"agy", "", ""},
		// Every other backend takes effort from its own config, which the hive
		// does not resolve — "" is the honest answer, and an omitted field.
		{"codex", "gpt-5.6-terra", ""},
		{"claude", "claude-sonnet-5", ""},
		{"bob", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := ResolveReasoningEffort(c.backend, c.model); got != c.want {
			t.Errorf("ResolveReasoningEffort(%q, %q) = %q, want %q", c.backend, c.model, got, c.want)
		}
	}

	// The exported constant must not silently diverge from what agy is told.
	if agyDefaultEffort == "" {
		t.Error("agyDefaultEffort must name a real effort agy accepts (low/medium/high)")
	}
}
