package config

import (
	"reflect"
	"testing"
)

func TestSandboxOpenShellFallsBackToGlobal(t *testing.T) {
	var nilA *AgentConfig
	if got := nilA.SandboxOpenShell(AgentSandboxConfig{}); !reflect.DeepEqual(got, SandboxOpenShellConfig{}) {
		t.Fatalf("nil agent, nil global = %+v, want zero value", got)
	}

	global := AgentSandboxConfig{OpenShell: &SandboxOpenShellConfig{
		GatewayURL: "https://gw.example:443", Namespace: "hive", PolicyProfile: "strict",
		TimeoutS: 900, TokenEnv: "OPENSHELL_TOKEN", InsecureTLS: true,
		WorkspaceClaim: "hive-data", WorkspaceClaimMount: "/data",
		AllowHosts: []string{"hub.example"}, Binaries: []string{"git", "gh"},
		RuntimeClassName: "gvisor", NodeSelector: map[string]string{"pool": "sandbox"},
		Tolerations: []SandboxJobToleration{{Key: "sandbox", Operator: "Exists"}},
	}}
	if got := nilA.SandboxOpenShell(global); !reflect.DeepEqual(got, *global.OpenShell) {
		t.Fatalf("nil agent = %+v, want global block", got)
	}
	noBlock := &AgentConfig{Sandbox: &AgentSandboxOverride{Image: "a-img"}}
	if got := noBlock.SandboxOpenShell(global); !reflect.DeepEqual(got, *global.OpenShell) {
		t.Fatalf("agent without openshell block = %+v, want global block", got)
	}

	// A per-agent block that names nothing but whitespace inherits every
	// cluster fact and every scheduling field from the global one.
	empty := &AgentConfig{Sandbox: &AgentSandboxOverride{OpenShell: &SandboxOpenShellConfig{GatewayURL: "  "}}}
	if got := empty.SandboxOpenShell(global); !reflect.DeepEqual(got, *global.OpenShell) {
		t.Fatalf("blank per-agent block = %+v, want global block", got)
	}
}

func TestSandboxOpenShellPerAgentFieldsWin(t *testing.T) {
	global := AgentSandboxConfig{OpenShell: &SandboxOpenShellConfig{
		GatewayURL: "gw-global:50051", Namespace: "global-ns", PolicyProfile: "global-policy",
		TimeoutS: 900, TokenEnv: "GLOBAL_TOKEN", InsecureTLS: false,
		WorkspaceClaim: "global-claim", WorkspaceClaimMount: "/global",
		AllowHosts: []string{"global.example", "hub.example"}, Binaries: []string{"git", "gh", "curl"},
		RuntimeClassName: "gvisor", NodeSelector: map[string]string{"pool": "global"},
		Tolerations: []SandboxJobToleration{{Key: "global", Operator: "Exists"}, {Key: "spot", Operator: "Exists"}},
	}}
	a := &AgentConfig{Sandbox: &AgentSandboxOverride{OpenShell: &SandboxOpenShellConfig{
		GatewayURL: "gw-agent:50051", TimeoutS: 60, InsecureTLS: true,
		WorkspaceClaimMount: "/agent",
		AllowHosts:          []string{"models.example"}, Binaries: []string{"node"},
		RuntimeClassName: "kata", NodeSelector: map[string]string{"gpu": "a100"},
		Tolerations: []SandboxJobToleration{{Key: "gpu", Operator: "Exists"}},
	}}}
	got := a.SandboxOpenShell(global)

	// Cluster facts: set per-agent wins, empty per-agent inherits.
	if got.GatewayURL != "gw-agent:50051" || got.TimeoutS != 60 || !got.InsecureTLS || got.WorkspaceClaimMount != "/agent" {
		t.Fatalf("per-agent cluster facts not kept: %+v", got)
	}
	if got.Namespace != "global-ns" || got.PolicyProfile != "global-policy" || got.TokenEnv != "GLOBAL_TOKEN" || got.WorkspaceClaim != "global-claim" {
		t.Fatalf("empty per-agent cluster facts not inherited: %+v", got)
	}

	// Scheduling and egress fields replace the global ones wholesale: the
	// global hosts, binaries, selector keys and tolerations must not leak in.
	if !reflect.DeepEqual(got.AllowHosts, []string{"models.example"}) {
		t.Fatalf("allow_hosts = %v, want per-agent list only", got.AllowHosts)
	}
	if !reflect.DeepEqual(got.Binaries, []string{"node"}) {
		t.Fatalf("binaries = %v, want per-agent list only", got.Binaries)
	}
	if got.RuntimeClassName != "kata" {
		t.Fatalf("runtime_class_name = %q, want kata", got.RuntimeClassName)
	}
	if !reflect.DeepEqual(got.NodeSelector, map[string]string{"gpu": "a100"}) {
		t.Fatalf("node_selector = %v, want per-agent map only", got.NodeSelector)
	}
	if !reflect.DeepEqual(got.Tolerations, []SandboxJobToleration{{Key: "gpu", Operator: "Exists"}}) {
		t.Fatalf("tolerations = %v, want per-agent list only", got.Tolerations)
	}

	// A global InsecureTLS cannot be switched off per-agent (false is the
	// zero value, so it reads as "unset" and inherits the global true).
	global.OpenShell.InsecureTLS = true
	a.Sandbox.OpenShell.InsecureTLS = false
	if !a.SandboxOpenShell(global).InsecureTLS {
		t.Fatal("per-agent InsecureTLS=false inherits global true")
	}

	// The merge must not write the inherited values back into its inputs.
	if global.OpenShell.AllowHosts[0] != "global.example" || a.Sandbox.OpenShell.Namespace != "" {
		t.Fatal("merge mutated its inputs")
	}
}

func TestOpenShellPreflightRegistry(t *testing.T) {
	t.Cleanup(func() { SetOpenShellPreflight(nil) })
	cfgs := []SandboxOpenShellConfig{{GatewayURL: "gw:50051"}}

	SetOpenShellPreflight(nil)
	if got := OpenShellPreflightWarnings(cfgs); got != nil {
		t.Fatalf("unregistered preflight = %v, want nil", got)
	}

	var seen []SandboxOpenShellConfig
	SetOpenShellPreflight(func(c []SandboxOpenShellConfig) []string {
		seen = c
		return []string{"gateway unreachable"}
	})
	if got := OpenShellPreflightWarnings(nil); got != nil || seen != nil {
		t.Fatalf("empty cfgs = %v (called=%v), want nil and no call", got, seen != nil)
	}
	if got := OpenShellPreflightWarnings(cfgs); !reflect.DeepEqual(got, []string{"gateway unreachable"}) || !reflect.DeepEqual(seen, cfgs) {
		t.Fatalf("registered preflight = %v (seen %v), want its warnings for the given cfgs", got, seen)
	}

	SetOpenShellPreflight(nil)
	if got := OpenShellPreflightWarnings(cfgs); got != nil {
		t.Fatalf("after unregister = %v, want nil", got)
	}
}
