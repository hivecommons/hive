package config

import "testing"

func TestAgentSandboxOptInRequiresGlobalGateAndAgentOverride(t *testing.T) {
	yes := true
	no := false
	global := AgentSandboxConfig{Enabled: true, Image: "global", EnvAllowlist: []string{"PATH"}}
	if (&AgentConfig{}).SandboxEnabled(global) {
		t.Fatal("missing agent override enabled sandbox")
	}
	if (&AgentConfig{Sandbox: &AgentSandboxOverride{Enabled: &no}}).SandboxEnabled(global) {
		t.Fatal("explicit false enabled sandbox")
	}
	if (&AgentConfig{Sandbox: &AgentSandboxOverride{Enabled: &yes}}).SandboxEnabled(AgentSandboxConfig{}) {
		t.Fatal("global disabled enabled sandbox")
	}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Enabled: &yes, Image: "agent", EnvAllowlist: []string{"HOME"}}}
	if !agent.SandboxEnabled(global) {
		t.Fatal("agent opt-in was not honored")
	}
	if got := agent.SandboxImage(global); got != "agent" {
		t.Fatalf("image=%q", got)
	}
	if got := agent.SandboxEnvAllowlist(global); len(got) != 1 || got[0] != "HOME" {
		t.Fatalf("allowlist=%v", got)
	}
	plain := &AgentConfig{Sandbox: &AgentSandboxOverride{Enabled: &yes}}
	if got := plain.SandboxImage(global); got != "global" {
		t.Fatalf("global image=%q", got)
	}
}
