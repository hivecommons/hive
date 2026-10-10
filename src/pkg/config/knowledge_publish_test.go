package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateKnowledgePublish(t *testing.T) {
	valid := func() KnowledgePublish {
		return KnowledgePublish{
			Connector:    "team-docs",
			Layers:       []string{"org", "project"},
			Root:         "/Hive/Knowledge/",
			IncludeTypes: []string{"decision", "coverage_rule"},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*KnowledgePublish)
		wantErr string
	}{
		{name: "valid", mutate: func(*KnowledgePublish) {}},
		{name: "unconfigured is valid", mutate: func(p *KnowledgePublish) { *p = KnowledgePublish{Layers: []string{"personal"}} }},
		{name: "all types", mutate: func(p *KnowledgePublish) { p.IncludeTypes = nil }},
		{name: "bad connector", mutate: func(p *KnowledgePublish) { p.Connector = "Team Docs" }, wantErr: "knowledge.publish.connector \"Team Docs\""},
		{name: "no layers", mutate: func(p *KnowledgePublish) { p.Layers = nil }, wantErr: "knowledge.publish.layers is required"},
		{name: "personal layer", mutate: func(p *KnowledgePublish) { p.Layers = []string{"org", "personal"} }, wantErr: "layers[1]: the personal layer is never published"},
		{name: "unknown layer", mutate: func(p *KnowledgePublish) { p.Layers = []string{"team"} }, wantErr: "layers[0] \"team\" must be project, org or community"},
		{name: "no root", mutate: func(p *KnowledgePublish) { p.Root = " / " }, wantErr: "knowledge.publish.root is required"},
		{name: "dotdot root", mutate: func(p *KnowledgePublish) { p.Root = "a/../b" }, wantErr: "knowledge.publish.root \"a/../b\""},
		{name: "empty segment", mutate: func(p *KnowledgePublish) { p.Root = "a//b" }, wantErr: "must be a plain page/folder path"},
		{name: "bad type", mutate: func(p *KnowledgePublish) { p.IncludeTypes = []string{"Decision!"} }, wantErr: "include_types[0] \"Decision!\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid()
			tt.mutate(&p)
			err := ValidateKnowledgePublish(p)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestKnowledgePublishYAML(t *testing.T) {
	var k KnowledgeConfig
	src := `
publish:
  connector: team-docs
  layers: [org]
  root: Hive
  include_types: [decision]
  dry_run: true
  propose_via: https://github.com/acme/knowledge
`
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	p := k.Publish
	if !p.IsConfigured() || p.Connector != "team-docs" || !p.DryRun || p.Root != "Hive" ||
		len(p.Layers) != 1 || len(p.IncludeTypes) != 1 || p.ProposeVia != "https://github.com/acme/knowledge" {
		t.Fatalf("decoded %+v", p)
	}
	out, err := yaml.Marshal(KnowledgeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "publish") {
		t.Fatalf("empty publish block should be omitted:\n%s", out)
	}
}
