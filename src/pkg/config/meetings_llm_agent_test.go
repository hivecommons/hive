package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
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
	r := cfg.ResolveMeetingsLLM(ctx)
	if r.Backend != MeetingsLLMModeAgentBackend || r.AgentBackend != "copilot" || r.Model != "gpt-x" || r.CLI.Warm ||
		r.CLI.Binary != "copilot" || r.CLI.AddressedTimeoutMS != DefaultMeetingsColdCLIAddressedTimeoutMS ||
		r.CLI.UnpromptedTimeoutMS != DefaultMeetingsColdCLIUnpromptedTimeoutMS || r.CLI.RestyleTimeoutMS != DefaultMeetingsColdCLIRestyleTimeoutMS {
		t.Fatalf("copilot %+v", r)
	}
	cfg.Meetings.LLM.Backend = "omp"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "omp needs a model") {
		t.Fatalf("omp without a model %+v", r)
	}
	cfg.Agents["o"] = AgentConfig{Backend: "omp", Model: "anthropic/claude-sonnet-5-5"}
	if r := cfg.ResolveMeetingsLLM(ctx); r.AgentBackend != "omp" || r.Model != "anthropic/claude-sonnet-5-5" || r.CLI.Warm {
		t.Fatalf("omp %+v", r)
	}
	cfg.Meetings.LLM.Backend = "codex"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "codex") {
		t.Fatalf("codex %+v", r)
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
		{MeetingsLLMConfig{Mode: "agent-backend", Backend: "codex"}, "cannot answer for meetings"},
		{MeetingsLLMConfig{Mode: "agent-backend", Backend: "gemini"}, "not installed in the hive image"},
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
		{Mode: "agent-backend", Backend: "copilot"}, {Mode: "agent-backend", Backend: "omp"}, {Mode: "agent-backend", Backend: "vllm"},
		{Mode: "agent-backend", Backend: "llm-d"}, {Mode: "agent-backend", Backend: "corp-gw"},
		{Mode: "claude-cli"}, {Mode: "claude-cli", Backend: "claude"},
	} {
		if err := (MeetingsConfig{Enabled: true, LLM: llm}).Validate(); err != nil {
			t.Errorf("%+v: %v", llm, err)
		}
	}
	if !strings.Contains(MeetingsLLMBackendRefusal("gemini"), "claude, copilot, litellm, llm-d, omp, vllm") {
		t.Fatal(MeetingsLLMBackendRefusal("gemini"))
	}
	if !strings.Contains(MeetingsLLMBackendRefusal("nope"), "configured model gateway") {
		t.Fatal(MeetingsLLMBackendRefusal("nope"))
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

// vllm and llm-d answer through the OpenAI-compatible path at the agents'
// first endpoint, keyless; a named gateway brings its endpoint, key, model
// and CA bundle; a missing endpoint or model is rules only with a reason.
func TestResolveMeetingsLLMInferenceAndGateways(t *testing.T) {
	ctx := context.Background()
	cfg := meetingsAgentCfg(t)
	cfg.Meetings.LLM.Mode = MeetingsLLMModeAgentBackend
	cfg.Agents["v"] = AgentConfig{Backend: "vllm", Model: "llama-4"}
	cfg.Meetings.LLM.Backend = "vllm"
	t.Setenv(VLLMEndpointEnv, " http://vllm-a:8000/ , http://vllm-b:8000")
	r := cfg.ResolveMeetingsLLM(ctx)
	if r.Backend != MeetingsLLMModeOpenAI || r.Endpoint != "http://vllm-a:8000" || r.Model != "llama-4" || r.APIKey != "" ||
		r.AgentBackend != "vllm" || r.Label() != "vllm model llama-4" {
		t.Fatalf("vllm %+v %q", r, r.Label())
	}
	t.Setenv(VLLMEndpointEnv, "")
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "no endpoint") {
		t.Fatalf("vllm without endpoint %+v", r)
	}
	cfg.Meetings.LLM.Backend = "llm-d"
	t.Setenv(LLMDEndpointEnv, "")
	if r := cfg.ResolveMeetingsLLM(ctx); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "needs a model") {
		t.Fatalf("llm-d without model %+v", r)
	}
	cfg.Meetings.LLM.Model = "qwen-3"
	if r := cfg.ResolveMeetingsLLM(ctx); r.Endpoint != DefaultLLMDEndpoint || r.Model != "qwen-3" {
		t.Fatalf("llm-d %+v", r)
	}
	cfg.Meetings.LLM.Model = ""
	t.Setenv("CORP_GW_KEY", "sk-corp")
	cfg.Governor.Gateways = []GatewayConfig{{Name: "Corp-GW", Kind: "litellm", Endpoint: "https://gw.example/v1/", APIKeyEnv: "CORP_GW_KEY",
		DefaultModel: "gw-default", CABundle: "/etc/ca.pem"}}
	cfg.Meetings.LLM.Backend = "corp-gw"
	r = cfg.ResolveMeetingsLLM(ctx)
	if r.Backend != MeetingsLLMModeOpenAI || r.Endpoint != "https://gw.example/v1" || r.APIKey != "sk-corp" || r.Model != "gw-default" ||
		r.CABundle != "/etc/ca.pem" || r.AgentBackend != "corp-gw" {
		t.Fatalf("gateway %+v", r)
	}
	if r := cfg.resolveMeetingsGateway(ResolvedMeetingsLLM{}, "absent"); r.Backend != MeetingsLLMModeNone || !strings.Contains(r.Reason, "not configured") {
		t.Fatalf("absent gateway %+v", r)
	}
}

func TestMeetingsGatewayBackends(t *testing.T) {
	if got := (*Config)(nil).MeetingsGatewayBackends(); got != nil {
		t.Fatal(got)
	}
	cfg := &Config{}
	cfg.Governor.Gateways = []GatewayConfig{
		{Name: "zeta", Endpoint: "https://z"},
		{Name: "Alpha", Endpoint: "https://a"},
		{Name: "alpha", Endpoint: "https://a2"},
		{Name: "ibm", Kind: GatewayKindWatsonx, Endpoint: "https://w"},
		{Name: "empty"},
		{Name: "litellm", Endpoint: "https://l"},
		{Name: "codex", Endpoint: "https://c"},
		{Name: "", Endpoint: "https://n"},
	}
	if got := strings.Join(cfg.MeetingsGatewayBackends(), ","); got != "alpha,zeta" {
		t.Fatalf("gateways %q", got)
	}
	if got := strings.Join(cfg.MeetingsLLMSupportedBackendsFor(), ","); got != "alpha,claude,copilot,litellm,llm-d,omp,vllm,zeta" {
		t.Fatalf("supported %q", got)
	}
	for backend, want := range map[string]string{
		"": "", "claude": "", "vllm": "", "zeta": "", "ALPHA": "",
		"ibm": "watsonx gateway", "empty": "no endpoint", "codex": "shell tool", "nope": "configured model gateway",
	} {
		got := cfg.MeetingsLLMBackendRefusalFor(backend)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q: refusal %q, want %q", backend, got, want)
		}
	}
	if got := (*Config)(nil).MeetingsLLMBackendRefusalFor("nope"); !strings.Contains(got, "configured model gateway") {
		t.Fatal(got)
	}
}

func TestValidateMeetingsGatewayBackend(t *testing.T) {
	cfg := &Config{}
	cfg.Governor.Gateways = []GatewayConfig{{Name: "corp-gw", Endpoint: "https://gw"}}
	cfg.Meetings.LLM = MeetingsLLMConfig{Mode: MeetingsLLMModeAgentBackend, Backend: "nope"}
	if err := cfg.validateMeetingsGatewayBackend(); err != nil {
		t.Fatalf("disabled meetings rejected: %v", err)
	}
	cfg.Meetings.Enabled = true
	if err := cfg.validateMeetingsGatewayBackend(); err == nil || !strings.Contains(err.Error(), "configured model gateway") {
		t.Fatalf("unknown backend err %v", err)
	}
	cfg.Meetings.LLM.Backend = "corp-gw"
	if err := cfg.validateMeetingsGatewayBackend(); err != nil {
		t.Fatal(err)
	}
	cfg.Meetings.LLM = MeetingsLLMConfig{Mode: MeetingsLLMModeAuto, Backend: "nope"}
	if err := cfg.validateMeetingsGatewayBackend(); err != nil {
		t.Fatalf("auto mode checked the backend: %v", err)
	}
}

func TestValidateMeetingsLLMSelection(t *testing.T) {
	cfg := &Config{}
	cfg.Governor.Gateways = []GatewayConfig{{Name: "corp-gw", Endpoint: "https://gw"}}
	for _, ok := range [][2]string{{"agent-backend", "Corp-GW"}, {"agent-backend", "copilot"}, {"agent-backend", ""}, {"auto", ""}} {
		if err := cfg.ValidateMeetingsLLMSelection(ok[0], ok[1]); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{{"agent-backend", "codex"}, {"agent-backend", "nope"}, {"claude-cli", "copilot"}, {"gpt", ""}} {
		if err := cfg.ValidateMeetingsLLMSelection(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// Every backend the agents can run on either answers for meetings or has a
// refusal reason, and none is both.
func TestMeetingsBackendDecisions(t *testing.T) {
	for _, b := range append(append([]string{}, cruxconfig.CLIBackends...), cruxconfig.InferenceBackends...) {
		_, refused := meetingsRefusedBackends[b]
		if MeetingsLLMBackendSupported(b) == refused {
			t.Errorf("backend %q: supported %v, refused %v", b, MeetingsLLMBackendSupported(b), refused)
		}
	}
	reasons := MeetingsRefusedBackendReasons()
	reasons["codex"] = "changed"
	if meetingsRefusedBackends["codex"] == "changed" {
		t.Fatal("reasons not copied")
	}
	for _, b := range MeetingsWarmCLIBackends {
		if !meetingsContains(MeetingsCompletionBackends, b) {
			t.Errorf("warm backend %q has no completion profile", b)
		}
	}
}
