package config

import (
	"strings"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"gopkg.in/yaml.v3"
)

// sandbox.runtime: substrate (ADR-0026 phase 2) — parse, merge semantics and
// the boot warnings that keep a misconfigured runtime from failing silently.

func TestSandboxSubstrateParsesAndMerges(t *testing.T) {
	src := `
agent_sandbox:
  enabled: true
  image: img
  runtime: substrate
  substrate:
    endpoint: api.ate-system.svc:443
    atespace: hive
    token_env: ATE_TOKEN
    image: ghcr.io/hivecommons/sandbox@sha256:0000000000000000000000000000000000000000000000000000000000000000
    allow_hosts: [litellm.example.com]
    timeout_s: 600
    snapshot_on_idle: true
agents:
  deps:
    sandbox:
      enabled: true
      substrate:
        allow_hosts: [hub.example.org]
        worker_selector: { class: gvisor }
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["deps"]
	if got := a.SandboxRuntime(cfg.AgentSandbox); got != SandboxRuntimeSubstrate {
		t.Fatalf("runtime = %q", got)
	}
	eff := a.SandboxSubstrate(cfg.AgentSandbox)
	if eff.Endpoint != "api.ate-system.svc:443" || eff.Atespace != "hive" || eff.TokenEnv != "ATE_TOKEN" || eff.TimeoutS != 600 || !eff.SnapshotOnIdle || !strings.Contains(eff.Image, "@sha256:") {
		t.Errorf("cluster facts must fall back to the global block: %+v", eff)
	}
	if len(eff.AllowHosts) != 1 || eff.AllowHosts[0] != "hub.example.org" {
		t.Errorf("per-agent allow_hosts must replace the global list: %v", eff.AllowHosts)
	}
	if eff.WorkerSelector["class"] != "gvisor" {
		t.Errorf("worker_selector = %v", eff.WorkerSelector)
	}
	other := AgentConfig{}
	if got := other.SandboxSubstrate(cfg.AgentSandbox); len(got.AllowHosts) != 1 || got.AllowHosts[0] != "litellm.example.com" {
		t.Errorf("agent without a block gets the global one: %+v", got)
	}
}

func TestSandboxSubstrateGateWarnings(t *testing.T) {
	yes := true
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{Enabled: true, Image: "img"},
			Agents: map[string]AgentConfig{
				"deps": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeSubstrate}},
			},
		},
	}
	cruxconfig.SetSubstratePreflight(func([]SandboxSubstrateConfig) []string { return nil })
	defer cruxconfig.SetSubstratePreflight(nil)
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "digest-pinned") {
		t.Errorf("warnings = %q, want exactly one about the digest-pinned image", got)
	}
	cfg.AgentSandbox.Substrate = &SandboxSubstrateConfig{Image: "ghcr.io/x/y@sha256:" + strings.Repeat("a", 64)}
	if got := AgentSandboxGateWarnings(cfg); len(got) != 0 {
		t.Errorf("fully configured substrate runtime still warns: %q", got)
	}
}

func TestAgentSandboxGateWarningsIncludeSubstratePreflight(t *testing.T) {
	yes := true
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{
				Enabled:   true,
				Image:     "img",
				Substrate: &SandboxSubstrateConfig{Image: "ghcr.io/x/y@sha256:" + strings.Repeat("a", 64)},
			},
			Agents: map[string]AgentConfig{
				"a": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeSubstrate}},
				"b": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeSubstrate}},
			},
		},
	}
	var calls, gotBlocks int
	cruxconfig.SetSubstratePreflight(func(b []SandboxSubstrateConfig) []string {
		calls++
		gotBlocks = len(b)
		return []string{"substrate control API api.ate-system.svc:443 is unreachable"}
	})
	defer cruxconfig.SetSubstratePreflight(nil)
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "unreachable") || calls != 1 || gotBlocks != 2 {
		t.Errorf("warnings = %q calls=%d blocks=%d, want the preflight warning from one call over both agents", got, calls, gotBlocks)
	}

	cfg.Agents = map[string]AgentConfig{"p": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimePodman}}}
	calls = 0
	AgentSandboxGateWarnings(cfg)
	if calls != 0 {
		t.Errorf("preflight ran with no substrate agent")
	}
}
