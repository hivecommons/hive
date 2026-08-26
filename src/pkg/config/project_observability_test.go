package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProjectObservabilityConfigRoundTripAndPrompt(t *testing.T) {
	in := Config{Governor: GovernorConfig{ProjectObservability: ProjectObservabilityConfig{
		OpenSource: []string{"opentelemetry", "prometheus"},
		KubeNative: []string{"servicemonitor"},
		Commercial: []string{"honeycomb"},
		References: map[string]ProjectObservabilityBackendRef{
			"honeycomb": {EndpointEnv: "OTEL_EXPORTER_OTLP_ENDPOINT", CredentialSecret: "observability/honeycomb-key"},
		},
	}}}
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Config
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := out.Governor.ProjectObservability
	if len(got.OpenSource) != 2 || got.References["honeycomb"].CredentialSecret != "observability/honeycomb-key" {
		t.Fatalf("round-trip config = %#v", got)
	}
	prompt := got.PromptSection()
	for _, want := range []string{"opentelemetry", "servicemonitor", "honeycomb", "OTEL_EXPORTER_OTLP_ENDPOINT", "observability/honeycomb-key"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %s", want, prompt)
		}
	}
}

func TestProjectObservabilityEmptyFailsClosed(t *testing.T) {
	prompt := (ProjectObservabilityConfig{}).PromptSection()
	if !strings.Contains(prompt, "No backend is confirmed") || !strings.Contains(prompt, "do not add an exporter") {
		t.Fatalf("empty config must fail closed, got: %s", prompt)
	}
}
