package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestValidateKnowledgeConnectors(t *testing.T) {
	off := false
	valid := func() KnowledgeConnector {
		return KnowledgeConnector{
			Name:     "eng-wiki",
			Type:     "git",
			Interval: "30m",
			Layer:    "org",
			Scope:    map[string]string{"url": "https://github.com/acme/wiki.git"},
			Auth:     KnowledgeConnectorAuth{Env: "WIKI_TOKEN"},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*KnowledgeConnector)
		extra   []KnowledgeConnector
		wantErr string
	}{
		{name: "valid", mutate: func(*KnowledgeConnector) {}},
		{name: "valid disabled no interval file auth", mutate: func(c *KnowledgeConnector) {
			c.Enabled = &off
			c.Interval = ""
			c.Auth = KnowledgeConnectorAuth{File: "/secrets/wiki-token"}
		}},
		{name: "valid no auth", mutate: func(c *KnowledgeConnector) { c.Auth = KnowledgeConnectorAuth{} }},
		{name: "missing name", mutate: func(c *KnowledgeConnector) { c.Name = " " }, wantErr: "knowledge.connectors[0].name is required"},
		{name: "bad name", mutate: func(c *KnowledgeConnector) { c.Name = "Eng Wiki" }, wantErr: "knowledge.connectors[0].name \"Eng Wiki\""},
		{name: "duplicate", mutate: func(*KnowledgeConnector) {}, extra: []KnowledgeConnector{valid()}, wantErr: "knowledge.connectors[1].name \"eng-wiki\" is a duplicate"},
		{name: "missing type", mutate: func(c *KnowledgeConnector) { c.Type = "" }, wantErr: "(eng-wiki).type is required"},
		{name: "bad type", mutate: func(c *KnowledgeConnector) { c.Type = "Notion!" }, wantErr: "(eng-wiki).type \"Notion!\""},
		{name: "bad layer", mutate: func(c *KnowledgeConnector) { c.Layer = "team" }, wantErr: "(eng-wiki).layer \"team\""},
		{name: "missing layer", mutate: func(c *KnowledgeConnector) { c.Layer = "" }, wantErr: ".layer \"\""},
		{name: "bad interval", mutate: func(c *KnowledgeConnector) { c.Interval = "soon" }, wantErr: "(eng-wiki).interval \"soon\""},
		{name: "short interval", mutate: func(c *KnowledgeConnector) { c.Interval = "10s" }, wantErr: "must be at least 1m0s"},
		{name: "env and file", mutate: func(c *KnowledgeConnector) { c.Auth.File = "/secrets/x" }, wantErr: ".auth: set only one of env or file"},
		{name: "bad env", mutate: func(c *KnowledgeConnector) { c.Auth.Env = "WIKI-TOKEN" }, wantErr: ".auth.env \"WIKI-TOKEN\""},
		{name: "relative file", mutate: func(c *KnowledgeConnector) { c.Auth = KnowledgeConnectorAuth{File: "secrets/x"} }, wantErr: ".auth.file \"secrets/x\" must be an absolute path"},
		{name: "inline auth secret", mutate: func(c *KnowledgeConnector) {
			c.Auth.Inline = map[string]any{"token": "ghp_x", "a": 1}
		}, wantErr: ".auth.a is not allowed: inline secrets are rejected"},
		{name: "scope secret", mutate: func(c *KnowledgeConnector) { c.Scope["api_token"] = "x" }, wantErr: ".scope.api_token looks like an inline secret"},
		{name: "scope password", mutate: func(c *KnowledgeConnector) { c.Scope["Password"] = "x" }, wantErr: ".scope.Password looks like an inline secret"},
		{name: "scope empty key", mutate: func(c *KnowledgeConnector) { c.Scope[" "] = "x" }, wantErr: ".scope has an empty key"},
		{name: "valid confluence cloud", mutate: func(c *KnowledgeConnector) {
			c.Type = "confluence"
			c.Scope = map[string]string{"base_url": "https://acme.atlassian.net/wiki", "email": "bot@acme.example", "spaces": "ENG,OPS", "include_labels": "runbook"}
			c.Auth = KnowledgeConnectorAuth{Env: "CONFLUENCE_API_TOKEN"}
		}},
		{name: "confluence inline api token", mutate: func(c *KnowledgeConnector) {
			c.Type = "confluence"
			c.Scope = map[string]string{"base_url": "https://wiki.acme.example", "api_token": "x"}
		}, wantErr: ".scope.api_token looks like an inline secret"},
		{name: "valid notion", mutate: func(c *KnowledgeConnector) {
			c.Type = "notion"
			c.Scope = map[string]string{"root_page_ids": "0123456789abcdef0123456789abcdef", "include_archived": "false"}
			c.Auth = KnowledgeConnectorAuth{File: "/var/run/secrets/notion/token"}
		}},
		{name: "notion inline secret", mutate: func(c *KnowledgeConnector) {
			c.Type = "notion"
			c.Auth = KnowledgeConnectorAuth{Inline: map[string]any{"token": "secret_x"}}
		}, wantErr: ".auth.token is not allowed: inline secrets are rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := ValidateKnowledgeConnectors(append([]KnowledgeConnector{c}, tt.extra...))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
	if err := ValidateKnowledgeConnectors(nil); err != nil {
		t.Fatalf("empty list: %v", err)
	}
}

func TestKnowledgeConnectorDefaults(t *testing.T) {
	var c KnowledgeConnector
	if !c.IsEnabled() {
		t.Fatal("absent enabled must default to true")
	}
	off := false
	c.Enabled = &off
	if c.IsEnabled() {
		t.Fatal("explicit enabled:false ignored")
	}
	if d, err := c.IntervalDuration(); err != nil || d != 0 {
		t.Fatalf("empty interval = %v, %v", d, err)
	}
	c.Interval = " 2h "
	if d, err := c.IntervalDuration(); err != nil || d != 2*time.Hour {
		t.Fatalf("interval = %v, %v", d, err)
	}
}

func TestKnowledgeConnectorsYAML(t *testing.T) {
	src := `
git_sources:
  - name: legacy
    url: https://github.com/acme/docs.git
    layer: project
documents:
  - name: guide
    url: https://example.com/guide.pdf
    layer: project
connectors:
  - name: eng-wiki
    type: git
    layer: org
    interval: 1h
    enabled: false
    scope:
      url: https://github.com/acme/wiki.git
      branch: main
    auth:
      env: WIKI_TOKEN
  - name: leaked
    type: document
    layer: project
    scope:
      url: https://example.com/x
    auth:
      token: abc123
`
	var kc KnowledgeConfig
	if err := yaml.Unmarshal([]byte(src), &kc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Existing keys keep decoding exactly as before.
	if len(kc.GitSources) != 1 || kc.GitSources[0].Name != "legacy" || len(kc.Documents) != 1 || kc.Documents[0].Name != "guide" {
		t.Fatalf("legacy sources changed: %+v / %+v", kc.GitSources, kc.Documents)
	}
	if len(kc.Connectors) != 2 {
		t.Fatalf("connectors = %d", len(kc.Connectors))
	}
	c := kc.Connectors[0]
	if c.Name != "eng-wiki" || c.Type != "git" || c.Layer != "org" || c.IsEnabled() ||
		c.Scope["url"] != "https://github.com/acme/wiki.git" || c.Auth.Env != "WIKI_TOKEN" || len(c.Auth.Inline) != 0 {
		t.Fatalf("decoded connector = %+v", c)
	}
	if kc.Connectors[1].Auth.Inline["token"] != "abc123" {
		t.Fatalf("inline auth key not captured: %+v", kc.Connectors[1].Auth)
	}
	err := ValidateKnowledgeConnectors(kc.Connectors)
	if err == nil || !strings.Contains(err.Error(), "knowledge.connectors[1] (leaked).auth.token is not allowed") {
		t.Fatalf("err = %v", err)
	}

	// A config without the key marshals without it.
	out, err := yaml.Marshal(KnowledgeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "connectors") {
		t.Fatalf("empty connectors should be omitted:\n%s", out)
	}
}

func TestConfigValidateRunsKnowledgeConnectors(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude", Model: "sonnet"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline config invalid: %v", err)
	}
	cfg.Knowledge.Connectors = []KnowledgeConnector{{Name: "w", Type: "git", Layer: "nope"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "knowledge.connectors[0] (w).layer") {
		t.Fatalf("Validate() err = %v, want connector layer rejection", err)
	}
}
