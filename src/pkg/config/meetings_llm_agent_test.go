package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func meetingsAgentCfg(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{}
	cfg.Meetings.LLM.APIKeyFile = filepath.Join(dir, "missing")
	cfg.Meetings.LLM.AnthropicKeyFile = filepath.Join(dir, "missing-a")
	cfg.Agents = map[string]AgentConfig{
		"a": {Backend: "claude", Model: "claude-opus-5"},
		"b": {Backend: "claude", Model: "claude-opus-5"},
		"c": {Backend: "Copilot", Model: "gpt-x"},
		"d": {},
	}
	return cfg
}

// claude-cli and agent-backend resolve to the claude CLI with Sonnet 5.5,
// the panel names the model, and the CLI settings carry the named defaults.
func TestResolveMeetingsLLMClaudeCLI(t *testing.T) {
	ctx := context.Background()
	t.Setenv(MeetingsCLIHomeEnv, "/cli-home")
	for _, mode := range []string{MeetingsLLMModeClaudeCLI, MeetingsLLMModeAgentBackend} {
		cfg := meetingsAgentCfg(t)
		cfg.Meetings.LLM.Mode = mode
		r := cfg.ResolveMeetingsLLM(ctx)
		if r.Backend != MeetingsLLMModeAgentBackend || r.AgentBackend != "claude" || r.Model != DefaultMeetingsClaudeCLIModel ||
			r.Label() != "claude CLI "+DefaultMeetingsClaudeCLIModel {
			t.Fatalf("%s: %+v label %q", mode, r, r.Label())
		}
		if r.CLI.Binary != "claude" || r.CLI.Home != "/cli-home" || r.CLI.AddressedTimeoutMS != DefaultMeetingsCLIAddressedTimeoutMS ||
			r.CLI.UnpromptedTimeoutMS != DefaultMeetingsCLIUnpromptedTimeoutMS || r.CLI.RestyleTimeoutMS != DefaultMeetingsCLIRestyleTimeoutMS ||
			r.CLI.OneShotTimeoutMS != DefaultMeetingsCLIOneShotTimeoutMS || r.CLI.RecycleTurns != DefaultMeetingsCLIRecycleTurns {
			t.Fatalf("%s CLI %+v", mode, r.CLI)
		}
	}
	cfg := meetingsAgentCfg(t)
	cfg.Meetings.LLM = MeetingsLLMConfig{Mode: MeetingsLLMModeAgentBackend, Backend: "claude", Model: "claude-haiku-5-5"}
	t.Setenv(MeetingsCLIBinaryEnv, "/opt/claude")
	t.Setenv(MeetingsCLIRecycleTurnsEnv, "3")
	if r := cfg.ResolveMeetingsLLM(ctx); r.Model != "claude-haiku-5-5" || r.CLI.Binary != "/opt/claude" || r.CLI.RecycleTurns != 3 {
		t.Fatalf("overrides %+v", r)
	}
}

// agent-backend follows the hive's agents when no backend is named; LiteLLM
// maps onto the OpenAI-compatible path; a backend with no profile, or no
// backend at all, is rules only with a reason.
func TestResolveMeetingsLLMAgentBackendChoices(t *testing.T) {
	ctx := context.Background()
	cfg := meetingsAgentCfg(t)
	cfg.Meetings.LLM.Mode = MeetingsLLMModeAgentBackend
	if r := cfg.ResolveMeetingsLLM(ctx); r.AgentBackend != "claude" {
		t.Fatalf("default from agents %+v", r)
	}
	cfg.Meetings.LLM.Backend = "litellm"
	cfg.Governor.LiteLLM.Endpoint = "https://litellm.example"
	cfg.Governor.LiteLLM.DefaultModel = "gw-model"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeOpenAI || r.Model != "gw-model" || r.Endpoint != "https://litellm.example" {
		t.Fatalf("litellm %+v", r)
	}
	cfg.Agents["e"] = AgentConfig{Backend: "litellm", Model: "agent-model"}
	if r := cfg.ResolveMeetingsLLM(ctx); r.Model != "agent-model" {
		t.Fatalf("litellm agent model %+v", r)
	}
	cfg.Meetings.LLM.Backend = "copilot"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "copilot") {
		t.Fatalf("copilot %+v", r)
	}
	t.Setenv(MeetingsLLMBackendEnv, "claude")
	if r := cfg.ResolveMeetingsLLM(ctx); r.AgentBackend != "claude" {
		t.Fatalf("env backend %+v", r)
	}
	t.Setenv(MeetingsLLMBackendEnv, "")
	empty := &Config{}
	empty.Meetings.LLM.Mode = MeetingsLLMModeAgentBackend
	if r := empty.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "no agent backend") {
		t.Fatalf("no agents %+v", r)
	}
}

func TestMeetingsAgentDefaults(t *testing.T) {
	if b, m := (*Config)(nil).MeetingsAgentDefault(); b != "" || m != "" {
		t.Fatal("nil config")
	}
	if b, m := (&Config{}).MeetingsAgentDefault(); b != "" || m != "" {
		t.Fatal("no agents")
	}
	cfg := meetingsAgentCfg(t)
	if b, m := cfg.MeetingsAgentDefault(); b != "claude" || m != "claude-opus-5" {
		t.Fatalf("default %q %q", b, m)
	}
	// Ties break by name.
	cfg.Agents["x"] = AgentConfig{Backend: "copilot"}
	if b, _ := cfg.MeetingsAgentDefault(); b != "claude" {
		t.Fatalf("tie %q", b)
	}
	if m := cfg.MeetingsDefaultModelFor("claude"); m != DefaultMeetingsClaudeCLIModel {
		t.Fatal(m)
	}
	if m := cfg.MeetingsDefaultModelFor("copilot"); m != "gpt-x" {
		t.Fatal(m)
	}
	if m := (*Config)(nil).MeetingsDefaultModelFor("copilot"); m != "" {
		t.Fatal(m)
	}
}

func TestMeetingsAgentBackendValidation(t *testing.T) {
	for _, tc := range []struct {
		llm  MeetingsLLMConfig
		want string
	}{
		{MeetingsLLMConfig{Mode: "agent-backend", Backend: "codex"}, "no meetings completion profile"},
		{MeetingsLLMConfig{Mode: "claude-cli", Backend: "copilot"}, "always uses the claude backend"},
		{MeetingsLLMConfig{Mode: "gpt"}, "agent-backend, claude-cli"},
	} {
		m := MeetingsConfig{Enabled: true, LLM: tc.llm}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err %v, want %q", tc.llm, err, tc.want)
		}
	}
	for _, llm := range []MeetingsLLMConfig{
		{Mode: "agent-backend"}, {Mode: "agent-backend", Backend: "claude"}, {Mode: "agent-backend", Backend: "litellm"},
		{Mode: "claude-cli"}, {Mode: "claude-cli", Backend: "claude"},
	} {
		if err := (MeetingsConfig{Enabled: true, LLM: llm}).Validate(); err != nil {
			t.Errorf("%+v: %v", llm, err)
		}
	}
	if !strings.Contains(MeetingsLLMBackendRefusal("gemini"), "claude, litellm") {
		t.Fatal(MeetingsLLMBackendRefusal("gemini"))
	}
}

func TestValidateMeetingsLLMChoice(t *testing.T) {
	for _, ok := range [][2]string{{"", ""}, {"auto", ""}, {"openai", "x"}, {"agent-backend", "claude"}, {"agent-backend", ""}, {"claude-cli", ""}, {"none", ""}} {
		if err := ValidateMeetingsLLMChoice(ok[0], ok[1]); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{{"gpt", ""}, {"claude-cli", "copilot"}, {"agent-backend", "codex"}} {
		if err := ValidateMeetingsLLMChoice(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// Without the env override the CLI HOME is the shared agent home only when
// it exists.
func TestResolveMeetingsCLIHome(t *testing.T) {
	t.Setenv(MeetingsCLIHomeEnv, "")
	r := resolveMeetingsCLI("claude")
	if _, err := filepath.Abs(DefaultMeetingsCLIHome); err != nil {
		t.Fatal(err)
	}
	if r.Home != "" && r.Home != DefaultMeetingsCLIHome {
		t.Fatalf("home %q", r.Home)
	}
}
