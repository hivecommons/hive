package config

import (
	"strings"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"gopkg.in/yaml.v3"
)

// sandbox.runtime: openshell (ADR-0026) — parse, merge semantics and the
// boot warnings that keep a misconfigured runtime from failing silently.

func TestSandboxOpenShellParsesAndMerges(t *testing.T) {
	src := `
agent_sandbox:
  enabled: true
  image: img
  runtime: openshell
  openshell:
    gateway_url: https://openshell-gateway.openshell.svc:8443
    namespace: hive
    token_env: OPENSHELL_TOKEN
    workspace_claim: hive-data
    allow_hosts: [litellm.example.com]
    timeout_s: 600
agents:
  deps:
    sandbox:
      enabled: true
      openshell:
        allow_hosts: [hub.example.org]
        node_selector: { kernel: "6.12" }
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["deps"]
	if got := a.SandboxRuntime(cfg.AgentSandbox); got != SandboxRuntimeOpenShell {
		t.Fatalf("runtime = %q", got)
	}
	eff := a.SandboxOpenShell(cfg.AgentSandbox)
	if eff.GatewayURL != "https://openshell-gateway.openshell.svc:8443" || eff.Namespace != "hive" || eff.TokenEnv != "OPENSHELL_TOKEN" || eff.WorkspaceClaim != "hive-data" || eff.TimeoutS != 600 {
		t.Errorf("cluster facts must fall back to the global block: %+v", eff)
	}
	if len(eff.AllowHosts) != 1 || eff.AllowHosts[0] != "hub.example.org" {
		t.Errorf("per-agent allow_hosts must replace the global list: %v", eff.AllowHosts)
	}
	if eff.NodeSelector["kernel"] != "6.12" {
		t.Errorf("node_selector = %v", eff.NodeSelector)
	}
	other := AgentConfig{}
	if got := other.SandboxOpenShell(cfg.AgentSandbox); len(got.AllowHosts) != 1 || got.AllowHosts[0] != "litellm.example.com" {
		t.Errorf("agent without a block gets the global one: %+v", got)
	}
}

func TestSandboxOpenShellGateWarnings(t *testing.T) {
	yes := true
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{Enabled: true, Image: "img"},
			Agents: map[string]AgentConfig{
				"deps": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeOpenShell}},
			},
		},
	}
	got := AgentSandboxGateWarnings(cfg)
	joined := strings.Join(got, "\n")
	if len(got) != 2 || !strings.Contains(joined, "gateway_url") || !strings.Contains(joined, "workspace_claim") {
		t.Errorf("warnings = %q, want one for gateway_url and one for workspace_claim", got)
	}
	cfg.AgentSandbox.OpenShell = &SandboxOpenShellConfig{GatewayURL: "gw:8080", WorkspaceClaim: "hive-data"}
	if got := AgentSandboxGateWarnings(cfg); len(got) != 0 {
		t.Errorf("fully configured openshell runtime still warns: %q", got)
	}
}

func TestAgentSandboxGateWarningsIncludeOpenShellPreflight(t *testing.T) {
	yes := true
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{
				Enabled:   true,
				Image:     "img",
				OpenShell: &SandboxOpenShellConfig{GatewayURL: "gw:8080", WorkspaceClaim: "hive-data"},
			},
			Agents: map[string]AgentConfig{
				"a": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeOpenShell}},
				"b": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeOpenShell}},
			},
		},
	}
	var calls int
	var gotBlocks int
	cruxconfig.SetOpenShellPreflight(func(b []SandboxOpenShellConfig) []string {
		calls++
		gotBlocks = len(b)
		return []string{"gateway gw:8080 is unreachable"}
	})
	defer cruxconfig.SetOpenShellPreflight(nil)
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "unreachable") || calls != 1 || gotBlocks != 2 {
		t.Errorf("warnings = %q calls=%d blocks=%d, want the preflight warning from one call over both agents", got, calls, gotBlocks)
	}

	cfg.Agents = map[string]AgentConfig{"p": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimePodman}}}
	calls = 0
	AgentSandboxGateWarnings(cfg)
	if calls != 0 {
		t.Errorf("preflight ran with no openshell agent")
	}
}
