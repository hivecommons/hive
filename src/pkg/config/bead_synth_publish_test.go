package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateBeadSynthPublish(t *testing.T) {
	for _, tt := range []struct {
		name    string
		publish BeadSynthPublishConfig
		invalid bool
	}{
		{"unset", BeadSynthPublishConfig{}, false},
		{"pr", BeadSynthPublishConfig{Path: "docs/knowledge/", Mode: "pr", Cadence: "per_run"}, false},
		{"commit config only", BeadSynthPublishConfig{Path: ".hive/knowledge/", Mode: "commit", Cadence: "daily"}, false},
		{"mode", BeadSynthPublishConfig{Mode: "push"}, true},
		{"cadence", BeadSynthPublishConfig{Cadence: "weekly"}, true},
		{"absolute", BeadSynthPublishConfig{Path: "/docs/knowledge"}, true},
		{"parent", BeadSynthPublishConfig{Path: "../knowledge"}, true},
		{"internal parent", BeadSynthPublishConfig{Path: "docs/../knowledge"}, true},
		{"trailing parent", BeadSynthPublishConfig{Path: "docs/.."}, true},
		{"windows absolute", BeadSynthPublishConfig{Path: `C:\knowledge`}, true},
		{"windows parent", BeadSynthPublishConfig{Path: `docs\..\knowledge`}, true},
		{"nul", BeadSynthPublishConfig{Path: "docs/\x00knowledge"}, true},
		{"dots in name", BeadSynthPublishConfig{Path: "docs/v1..v2/"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			k := KnowledgeConfig{BeadSynthesizer: BeadSynthesizerConfig{Publish: tt.publish}}
			if err := ValidateKnowledgePipeline(k); (err != nil) != tt.invalid {
				t.Fatalf("ValidateKnowledgePipeline() = %v, invalid = %v", err, tt.invalid)
			}
		})
	}
}

func TestBeadSynthPublishDefaultsAndYAML(t *testing.T) {
	var cfg Config
	cfg.Knowledge.Enabled = true
	cfg.applyDefaults()
	p := cfg.Knowledge.BeadSynthesizer.Publish
	if p.Enabled || p.Path != "docs/knowledge/" || p.Mode != "pr" || p.Cadence != "per_run" || p.Repo != "" || p.BaseBranch != "" {
		t.Fatalf("unsafe or missing defaults: %+v", p)
	}
	want := BeadSynthPublishConfig{Enabled: true, Repo: "org/repo", Path: ".hive/knowledge/", BaseBranch: "v5", Mode: "commit", Cadence: "daily"}
	cfg.Knowledge.BeadSynthesizer.Publish = want
	data, err := yaml.Marshal(cfg.Knowledge)
	if err != nil {
		t.Fatal(err)
	}
	var got KnowledgeConfig
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.BeadSynthesizer.Publish != want {
		t.Fatalf("YAML round trip = %+v, want %+v", got.BeadSynthesizer.Publish, want)
	}
}
