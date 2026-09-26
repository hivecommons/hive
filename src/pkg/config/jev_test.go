package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJevConfigDefaults(t *testing.T) {
	var j JevConfig
	if j.EffectiveProvider() != JevProviderOpenRouter || j.EffectiveModel() != "typesafe/jev-1.13" ||
		j.EffectiveEndpoint() != "https://openrouter.ai/api/v1/systemone" || j.EffectiveAPIKeyEnv() != JevDefaultAPIKeyEnv ||
		j.EffectiveTimeout() != 5*time.Second {
		t.Errorf("zero-value defaults wrong: %q %q %q %q %v", j.EffectiveProvider(), j.EffectiveModel(), j.EffectiveEndpoint(), j.EffectiveAPIKeyEnv(), j.EffectiveTimeout())
	}
	ts := JevConfig{Provider: " TypeSafe ", Endpoint: "https://x.example/v1/systemone/"}
	if ts.EffectiveProvider() != JevProviderTypeSafe || ts.EffectiveModel() != "jev-latest" || ts.EffectiveEndpoint() != "https://x.example/v1/systemone" {
		t.Errorf("typesafe overrides: %q %q %q", ts.EffectiveProvider(), ts.EffectiveModel(), ts.EffectiveEndpoint())
	}
	if (JevConfig{Provider: "typesafe"}).EffectiveEndpoint() != "https://api.typesafe.ai/v1/systemone" {
		t.Error("typesafe default endpoint")
	}
}

func TestJevConfigValidate(t *testing.T) {
	if err := (JevConfig{}).Validate(); err != nil {
		t.Fatalf("zero value must validate: %v", err)
	}
	for name, j := range map[string]JevConfig{
		"provider":     {Provider: "anthropic"},
		"endpoint":     {Endpoint: "ftp://x"},
		"endpoint-rel": {Endpoint: "/v1/systemone"},
		"timeout":      {Timeout: -time.Second},
	} {
		if err := j.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// TestValidateRejectsInvalidJevMode pins the load-time gate and its error text
// (troubleshooting.md documents `agent <name>: invalid jev_mode`).
func TestValidateRejectsInvalidJevMode(t *testing.T) {
	cfg := &Config{Project: ProjectConfig{Org: "my-org"}, GitHub: GitHubConfig{Token: "t"}, Agents: map[string]AgentConfig{"scanner": {Enabled: true, Backend: "claude", JevMode: "always"}}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `agent scanner: invalid jev_mode "always"`) {
		t.Fatalf("want jev_mode error, got %v", err)
	}
	for _, mode := range []string{"", JevModeOff, JevModeAssist} {
		cfg.Agents["scanner"] = AgentConfig{Enabled: true, Backend: "claude", JevMode: mode}
		if err := cfg.Validate(); err != nil {
			t.Errorf("jev_mode=%q: %v", mode, err)
		}
	}
	cfg.Jev = JevConfig{Provider: "nope"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "jev: invalid provider") {
		t.Errorf("hive-level jev block must be validated: %v", err)
	}
}

func TestValidateAgentOverlayRejectsInvalidJevMode(t *testing.T) {
	cfg := &Config{}
	if err := cfg.validateAgentOverlay("x", AgentConfig{Backend: "claude", JevMode: "maybe"}); err == nil || !strings.Contains(err.Error(), "invalid jev_mode") {
		t.Fatalf("want overlay rejection, got %v", err)
	}
	if err := cfg.validateAgentOverlay("x", AgentConfig{Backend: "claude", JevMode: JevModeAssist}); err != nil {
		t.Fatal(err)
	}
}

func TestJevEnabledAndAssistEnabled(t *testing.T) {
	if (AgentConfig{}).JevEnabled() || (AgentConfig{JevMode: JevModeOff}).JevEnabled() || !(AgentConfig{JevMode: JevModeAssist}).JevEnabled() {
		t.Error("JevEnabled: only assist is on")
	}
	cfg := &Config{Agents: map[string]AgentConfig{"on": {JevMode: JevModeAssist}, "off": {}}}
	if !cfg.JevAssistEnabled("on") || cfg.JevAssistEnabled("off") || cfg.JevAssistEnabled("missing") || (*Config)(nil).JevAssistEnabled("on") {
		t.Error("JevAssistEnabled predicate wrong")
	}
}

// TestResolveJevAPIKey pins the precedence: the Jev key env var (configurable
// name) first, then the connected OpenRouter gateway's key, else empty.
func TestResolveJevAPIKey(t *testing.T) {
	t.Setenv("JEV_API_KEY", "")
	t.Setenv("MY_JEV", "")
	t.Setenv("OR_KEY", "")
	cfg := &Config{}
	if cfg.ResolveJevAPIKey() != "" || cfg.JevReady() {
		t.Fatal("no key anywhere must read as not ready")
	}
	cfg.Governor.Gateways = []GatewayConfig{{Name: "openrouter", Kind: GatewayKindOpenRouter, APIKeyEnv: "OR_KEY"}}
	if cfg.ResolveJevAPIKey() != "" {
		t.Fatal("gateway with empty key is not a key")
	}
	t.Setenv("OR_KEY", "or-1")
	if got := cfg.ResolveJevAPIKey(); got != "or-1" {
		t.Fatalf("gateway fallback = %q", got)
	}
	t.Setenv("JEV_API_KEY", " jev-1 ")
	if got := cfg.ResolveJevAPIKey(); got != "jev-1" {
		t.Fatalf("JEV_API_KEY must win: %q", got)
	}
	cfg.Jev.APIKeyEnv = "MY_JEV"
	if got := cfg.ResolveJevAPIKey(); got != "or-1" {
		t.Fatalf("renamed env var not set → gateway: %q", got)
	}
	t.Setenv("MY_JEV", "jev-2")
	if got := cfg.ResolveJevAPIKey(); got != "jev-2" || !cfg.JevReady() {
		t.Fatalf("renamed env var: %q", got)
	}
}

// TestLoadJevModeFromYAML: jev_mode and the jev block round-trip through the
// loader, and the example config keeps Jev off for every agent.
func TestLoadJevModeFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(`
project:
  org: acme
github:
  token: t
agents:
  scanner:
    enabled: true
    backend: claude
    jev_mode: assist
jev:
  provider: typesafe
  timeout: 3s
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["scanner"].JevMode != JevModeAssist || !cfg.JevAssistEnabled("scanner") {
		t.Errorf("jev_mode not loaded: %+v", cfg.Agents["scanner"].JevMode)
	}
	if cfg.Jev.EffectiveProvider() != JevProviderTypeSafe || cfg.Jev.EffectiveTimeout() != 3*time.Second {
		t.Errorf("jev block not loaded: %+v", cfg.Jev)
	}
}
