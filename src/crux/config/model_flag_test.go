package config

import (
	"reflect"
	"testing"
)

func TestModelFlagOmitsEmptyModel(t *testing.T) {
	for _, empty := range []string{"", " ", "\t\n"} {
		if got := ModelFlag(empty); got != "" {
			t.Errorf("ModelFlag(%q) = %q, want empty", empty, got)
		}
		if got := ModelArgs(empty); got != nil {
			t.Errorf("ModelArgs(%q) = %v, want nil", empty, got)
		}
	}
	if got := ModelFlag(" claude-opus-4-8 "); got != " --model claude-opus-4-8" {
		t.Errorf("ModelFlag trimmed = %q", got)
	}
	if got := ModelArgs("auto"); !reflect.DeepEqual(got, []string{"--model", "auto"}) {
		t.Errorf("ModelArgs(auto) = %v", got)
	}
}

func TestDropDanglingModelFlag(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		want    string
		dropped bool
	}{
		{
			name:    "live incident cmdline",
			cmd:     "/usr/local/bin/claude --model --dangerously-skip-permissions --bare --settings /tmp/s.json --disallowed-tools 'mcp__github__create_issue'",
			want:    "/usr/local/bin/claude --dangerously-skip-permissions --bare --settings /tmp/s.json --disallowed-tools 'mcp__github__create_issue'",
			dropped: true,
		},
		{name: "trailing bare flag", cmd: "pi --model", want: "pi", dropped: true},
		{name: "trailing bare flag with space", cmd: "pi --model ", want: "pi", dropped: true},
		{name: "well-formed untouched", cmd: "claude --model claude-opus-4-8 --dangerously-skip-permissions", want: "claude --model claude-opus-4-8 --dangerously-skip-permissions"},
		{name: "quoted value untouched", cmd: "x --model 'a  b' --flag", want: "x --model 'a  b' --flag"},
		{name: "no model flag", cmd: "goose run -s", want: "goose run -s"},
		{name: "repeated dangling", cmd: "x --model --model --flag", want: "x --flag", dropped: true},
		{name: "empty", cmd: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := DropDanglingModelFlag(tc.cmd)
			if got != tc.want || dropped != tc.dropped {
				t.Errorf("DropDanglingModelFlag(%q) = (%q, %v), want (%q, %v)", tc.cmd, got, dropped, tc.want, tc.dropped)
			}
		})
	}
}

func TestDefaultInferenceModel(t *testing.T) {
	t.Setenv(LiteLLMEndpointEnv, "")
	legacy := AgentRuntimeGovernorConfig{LiteLLM: LiteLLMConfig{
		Endpoint:     "https://ete-litellm.example.com",
		DefaultModel: " claude-opus-4-8 ",
	}}
	if got := legacy.DefaultInferenceModel("litellm"); got != "claude-opus-4-8" {
		t.Errorf("legacy litellm default = %q, want claude-opus-4-8 (verbatim, trimmed)", got)
	}
	// The synthetic gateway named "litellm" resolves the same block.
	if got := legacy.DefaultInferenceModel("vllm"); got != "" {
		t.Errorf("vllm has no default model source, got %q", got)
	}
	if got := legacy.DefaultInferenceModel("claude"); got != "" {
		t.Errorf("CLI backend must keep its own default, got %q", got)
	}
	if got := legacy.DefaultInferenceModel(""); got != "" {
		t.Errorf("empty backend must not resolve, got %q", got)
	}

	noEndpoint := AgentRuntimeGovernorConfig{LiteLLM: LiteLLMConfig{DefaultModel: "m"}}
	if got := noEndpoint.DefaultInferenceModel("litellm"); got != "" {
		t.Errorf("default_model without an endpoint must not resolve, got %q", got)
	}
	localProxy := AgentRuntimeGovernorConfig{LiteLLM: LiteLLMConfig{DefaultModel: "m", LocalProxy: true}}
	if got := localProxy.DefaultInferenceModel("litellm"); got != "m" {
		t.Errorf("local_proxy default = %q, want m", got)
	}

	gateways := AgentRuntimeGovernorConfig{
		LiteLLM: LiteLLMConfig{Endpoint: "https://old.example.com", DefaultModel: "old"},
		Gateways: []GatewayConfig{
			{Name: "litellm", Kind: GatewayKindLiteLLM, Endpoint: "https://new.example.com", DefaultModel: "new"},
			{Name: "openrouter", Kind: GatewayKindOpenRouter, Endpoint: "https://openrouter.ai/api/v1", DefaultModel: "or-default"},
		},
	}
	if got := gateways.DefaultInferenceModel("litellm"); got != "new" {
		t.Errorf("explicit litellm gateway must win over legacy block, got %q", got)
	}
	if got := gateways.DefaultInferenceModel("OpenRouter"); got != "or-default" {
		t.Errorf("named gateway default = %q, want or-default", got)
	}
}
