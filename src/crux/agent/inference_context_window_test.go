package agent

import (
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// contextWindowEnv returns the CLAUDE_CODE_MAX_CONTEXT_TOKENS value in pairs.
func contextWindowEnv(pairs []agentEnvPair) (string, bool) {
	for _, p := range pairs {
		if p.Key == claudeMaxContextTokensEnv {
			return p.Value, true
		}
	}
	return "", false
}

// An inference-routed agent on a model ID the CLI does not know is told the
// model's real window, so it compacts before the server refuses the turn
// (hivecommons/hive#19913). The operator setting wins over discovery.
func TestAgentEnvPairs_InferenceContextWindow(t *testing.T) {
	t.Setenv(inferencePassContextWindowEnv, "")
	const discovered = 262144
	cases := []struct {
		name     string
		cfg      config.AgentConfig
		lookup   func(string) (int, string)
		want     string
		wantNone bool
	}{
		{
			name:   "operator setting",
			cfg:    config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen", MaxContextTokens: 131072},
			lookup: func(string) (int, string) { return discovered, "vllm/Qwen" },
			want:   "131072",
		},
		{
			name:   "discovered window",
			cfg:    config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen"},
			lookup: func(string) (int, string) { return discovered, "vllm/Qwen" },
			want:   "262144",
		},
		{
			name: "operator setting without a lookup",
			cfg:  config.AgentConfig{Backend: "vllm", Model: "Qwen/Qwen3", MaxContextTokens: 32768},
			want: "32768",
		},
		{
			name:     "nothing known",
			cfg:      config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen"},
			lookup:   func(string) (int, string) { return 0, "vllm/Qwen" },
			wantNone: true,
		},
		{
			name:     "claude model id behind a gateway",
			cfg:      config.AgentConfig{Backend: "litellm", Model: "anthropic/claude-sonnet-5-5", MaxContextTokens: 200000},
			lookup:   func(string) (int, string) { return 200000, "anthropic/claude-sonnet-5-5" },
			wantNone: true,
		},
		{
			name:     "empty model resolved to a claude default on the route",
			cfg:      config.AgentConfig{Backend: "litellm", MaxContextTokens: 200000},
			lookup:   func(string) (int, string) { return 0, "Anthropic/Claude-Haiku-5-5" },
			wantNone: true,
		},
		{
			name:     "subscription backend",
			cfg:      config.AgentConfig{Backend: "claude", Model: "sonnet", MaxContextTokens: 200000},
			lookup:   func(string) (int, string) { return discovered, "" },
			wantNone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(map[string]config.AgentConfig{"a": tc.cfg}, discardLogger(), ProjectContext{})
			if tc.lookup != nil {
				m.SetInferenceContextWindowLookup(tc.lookup)
			}
			v, ok := contextWindowEnv(m.agentEnvPairs(&AgentProcess{Name: "a", Config: tc.cfg}))
			if tc.wantNone {
				if ok {
					t.Fatalf("%s = %q, want unset", claudeMaxContextTokensEnv, v)
				}
				return
			}
			if !ok || v != tc.want {
				t.Fatalf("%s = %q (present=%v), want %q", claudeMaxContextTokensEnv, v, ok, tc.want)
			}
		})
	}
}

// The lookup is asked for the agent by name.
func TestAgentEnvPairs_InferenceContextWindowLookupByName(t *testing.T) {
	t.Setenv(inferencePassContextWindowEnv, "")
	cfg := config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen"}
	m := NewManager(map[string]config.AgentConfig{"quality": cfg}, discardLogger(), ProjectContext{})
	var asked string
	m.SetInferenceContextWindowLookup(func(name string) (int, string) {
		asked = name
		return 65536, ""
	})
	v, ok := contextWindowEnv(m.agentEnvPairs(&AgentProcess{Name: "quality", Config: cfg}))
	if asked != "quality" || !ok || v != "65536" {
		t.Fatalf("asked %q, got %q (present=%v)", asked, v, ok)
	}
}

// HIVE_INFERENCE_PASS_CONTEXT_WINDOW turns the export off; anything else,
// including unset, leaves it on.
func TestAgentEnvPairs_InferenceContextWindowKnob(t *testing.T) {
	cfg := config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen", MaxContextTokens: 32768}
	for _, tc := range []struct {
		value string
		on    bool
	}{
		{"", true}, {"1", true}, {"true", true}, {"yes", true},
		{"0", false}, {"false", false}, {"FALSE", false}, {" no ", false}, {"off", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(inferencePassContextWindowEnv, tc.value)
			m := NewManager(map[string]config.AgentConfig{"a": cfg}, discardLogger(), ProjectContext{})
			_, ok := contextWindowEnv(m.agentEnvPairs(&AgentProcess{Name: "a", Config: cfg}))
			if ok != tc.on {
				t.Fatalf("%s=%q: exported=%v, want %v", inferencePassContextWindowEnv, tc.value, ok, tc.on)
			}
		})
	}
}

func TestInferenceContextWindowEnv(t *testing.T) {
	if env := inferenceContextWindowEnv(0); len(env) != 0 {
		t.Fatalf("window 0 env = %v, want none", env)
	}
	if env := inferenceContextWindowEnv(-5); len(env) != 0 {
		t.Fatalf("negative window env = %v, want none", env)
	}
	if env := inferenceContextWindowEnv(4096); len(env) != 1 || env[claudeMaxContextTokensEnv] != "4096" {
		t.Fatalf("window env = %v", env)
	}
}

// The setting describes the configured model: while an override runs another
// model the agent falls back to discovery; an override naming the same model
// keeps the setting.
func TestAgentEnvPairs_InferenceContextWindowIgnoresSettingUnderModelOverride(t *testing.T) {
	t.Setenv(inferencePassContextWindowEnv, "")
	cfg := config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen", MaxContextTokens: 262144}
	for _, tc := range []struct {
		name, override string
		discovered     int
		want           string
		present        bool
	}{
		{"other model, discovered", "vllm/DeepSeek", 1048576, "1048576", true},
		{"other model, nothing discovered", "vllm/DeepSeek", 0, "", false},
		{"same model", "vllm/Qwen", 0, "262144", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(map[string]config.AgentConfig{"a": cfg}, discardLogger(), ProjectContext{})
			m.SetInferenceContextWindowLookup(func(string) (int, string) { return tc.discovered, tc.override })
			v, ok := contextWindowEnv(m.agentEnvPairs(&AgentProcess{Name: "a", Config: cfg, ModelOverride: tc.override}))
			if ok != tc.present || v != tc.want {
				t.Fatalf("%s = %q (present=%v), want %q (present=%v)", claudeMaxContextTokensEnv, v, ok, tc.want, tc.present)
			}
		})
	}
}
