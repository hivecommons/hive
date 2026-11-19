package config

import (
	"reflect"
	"testing"
)

func TestSandboxSubstrateFallsBackToGlobal(t *testing.T) {
	var nilA *AgentConfig
	if got := nilA.SandboxSubstrate(AgentSandboxConfig{}); !reflect.DeepEqual(got, SandboxSubstrateConfig{}) {
		t.Fatalf("nil agent, nil global = %+v, want zero value", got)
	}

	global := AgentSandboxConfig{Substrate: &SandboxSubstrateConfig{
		Endpoint: "api.ate-system.svc:443", Atespace: "hive", TokenEnv: "ATE_TOKEN", TokenFile: "/tok",
		CAFile: "/ca", InsecureTLS: true, TimeoutS: 900, RouterURL: "https://router:8444",
		RouterClientBundle: "/bundle", RouterCAFile: "/router-ca", BridgePort: 9000,
		Image: "img@sha256:abc", BridgeCommand: []string{"/bridge"}, CPUMillicores: 1000, MemoryBytes: 1 << 30,
		WorkerSelector: map[string]string{"class": "gvisor"}, SnapshotOnIdle: true,
		AllowHosts: []string{"hub.example"}, PolicyProfile: "strict",
	}}
	if got := nilA.SandboxSubstrate(global); !reflect.DeepEqual(got, *global.Substrate) {
		t.Fatalf("nil agent = %+v, want global block", got)
	}
	noBlock := &AgentConfig{Sandbox: &AgentSandboxOverride{Image: "a-img"}}
	if got := noBlock.SandboxSubstrate(global); !reflect.DeepEqual(got, *global.Substrate) {
		t.Fatalf("agent without substrate block = %+v, want global block", got)
	}

	// A per-agent block that names nothing but whitespace inherits every
	// field from the global one.
	empty := &AgentConfig{Sandbox: &AgentSandboxOverride{Substrate: &SandboxSubstrateConfig{Endpoint: "  "}}}
	if got := empty.SandboxSubstrate(global); !reflect.DeepEqual(got, *global.Substrate) {
		t.Fatalf("blank per-agent block = %+v, want global block", got)
	}
}

func TestSandboxSubstratePerAgentFieldsWin(t *testing.T) {
	global := AgentSandboxConfig{Substrate: &SandboxSubstrateConfig{
		Endpoint: "api-global:443", Atespace: "global", TokenEnv: "GLOBAL_TOKEN", TokenFile: "/global-tok",
		CAFile: "/global-ca", TimeoutS: 900, RouterURL: "https://router-global:8444",
		RouterClientBundle: "/global-bundle", RouterCAFile: "/global-router-ca", BridgePort: 8477,
		Image: "global@sha256:aaa", BridgeCommand: []string{"/global-bridge"}, CPUMillicores: 2000, MemoryBytes: 4 << 30,
		WorkerSelector: map[string]string{"pool": "global"},
		AllowHosts:     []string{"global.example", "hub.example"}, PolicyProfile: "global-policy",
	}}
	a := &AgentConfig{Sandbox: &AgentSandboxOverride{Substrate: &SandboxSubstrateConfig{
		Endpoint: "api-agent:443", TimeoutS: 60, InsecureTLS: true, BridgePort: 9001,
		Image: "agent@sha256:bbb", BridgeCommand: []string{"/agent-bridge"}, CPUMillicores: 500, MemoryBytes: 1 << 30,
		WorkerSelector: map[string]string{"gpu": "a100"}, SnapshotOnIdle: true,
		AllowHosts: []string{"models.example"},
	}}}
	got := a.SandboxSubstrate(global)

	if got.Endpoint != "api-agent:443" || got.TimeoutS != 60 || !got.InsecureTLS || got.BridgePort != 9001 || got.Image != "agent@sha256:bbb" {
		t.Fatalf("per-agent fields not kept: %+v", got)
	}
	if got.Atespace != "global" || got.TokenEnv != "GLOBAL_TOKEN" || got.TokenFile != "/global-tok" || got.CAFile != "/global-ca" ||
		got.RouterURL != "https://router-global:8444" || got.RouterClientBundle != "/global-bundle" || got.RouterCAFile != "/global-router-ca" || got.PolicyProfile != "global-policy" {
		t.Fatalf("empty per-agent cluster facts not inherited: %+v", got)
	}
	if got.CPUMillicores != 500 || got.MemoryBytes != 1<<30 || !got.SnapshotOnIdle {
		t.Fatalf("per-agent resources/snapshot not kept: %+v", got)
	}
	if !reflect.DeepEqual(got.AllowHosts, []string{"models.example"}) {
		t.Fatalf("allow_hosts = %v, want per-agent list only", got.AllowHosts)
	}
	if !reflect.DeepEqual(got.BridgeCommand, []string{"/agent-bridge"}) {
		t.Fatalf("bridge_command = %v, want per-agent list only", got.BridgeCommand)
	}
	if !reflect.DeepEqual(got.WorkerSelector, map[string]string{"gpu": "a100"}) {
		t.Fatalf("worker_selector = %v, want per-agent map only", got.WorkerSelector)
	}

	// Booleans: a global true cannot be switched off per-agent.
	global.Substrate.InsecureTLS = true
	global.Substrate.SnapshotOnIdle = true
	a.Sandbox.Substrate.InsecureTLS = false
	a.Sandbox.Substrate.SnapshotOnIdle = false
	if merged := a.SandboxSubstrate(global); !merged.InsecureTLS || !merged.SnapshotOnIdle {
		t.Fatal("per-agent false inherits global true")
	}

	// The merge must not write the inherited values back into its inputs.
	if global.Substrate.AllowHosts[0] != "global.example" || a.Sandbox.Substrate.Atespace != "" {
		t.Fatal("merge mutated its inputs")
	}
}

func TestSubstratePreflightRegistry(t *testing.T) {
	t.Cleanup(func() { SetSubstratePreflight(nil) })
	cfgs := []SandboxSubstrateConfig{{Endpoint: "api:443"}}

	SetSubstratePreflight(nil)
	if got := SubstratePreflightWarnings(cfgs); got != nil {
		t.Fatalf("unregistered preflight = %v, want nil", got)
	}

	var seen []SandboxSubstrateConfig
	SetSubstratePreflight(func(c []SandboxSubstrateConfig) []string {
		seen = c
		return []string{"control api unreachable"}
	})
	if got := SubstratePreflightWarnings(nil); got != nil || seen != nil {
		t.Fatalf("empty cfgs = %v (called=%v), want nil and no call", got, seen != nil)
	}
	if got := SubstratePreflightWarnings(cfgs); !reflect.DeepEqual(got, []string{"control api unreachable"}) || !reflect.DeepEqual(seen, cfgs) {
		t.Fatalf("registered preflight = %v (seen %v), want its warnings for the given cfgs", got, seen)
	}

	SetSubstratePreflight(nil)
	if got := SubstratePreflightWarnings(cfgs); got != nil {
		t.Fatalf("after unregister = %v, want nil", got)
	}
}

func TestValidSandboxRuntimesIncludesSubstrate(t *testing.T) {
	if !ValidSandboxRuntimes[SandboxRuntimeSubstrate] {
		t.Fatal("substrate is not a valid sandbox runtime")
	}
}
