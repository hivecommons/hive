package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// hivecommons/hive#19574: a bad sandbox.setup fails config load, and a good one
// survives the dashboard overlay save and reload.

func testSetup() *SandboxSetupConfig {
	return &SandboxSetupConfig{Commands: []string{"./scripts/install-ml-stack.sh"}, HealthCommands: []string{`python -c 'import torch'`}, TimeoutS: 600, TotalTimeoutS: 1200}
}

func TestSandboxSetupRejectedAtValidate(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{AgentSandbox: AgentSandboxConfig{Setup: &SandboxSetupConfig{Commands: []string{" "}}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agent_sandbox.setup.commands[0]") {
		t.Fatalf("global empty command error = %v", err)
	}
	cfg.AgentSandbox = AgentSandboxConfig{}
	cfg.Agents = map[string]AgentConfig{"quality": {Sandbox: &AgentSandboxOverride{Setup: &SandboxSetupConfig{HealthCommands: []string{"true"}, TotalTimeoutS: -1}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agents.quality.sandbox.setup.total_timeout_s") {
		t.Fatalf("agent negative timeout error = %v", err)
	}
}

func TestSandboxSetupRejectedOnLoad(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hive.yaml")
	seed := "project:\n  org: testorg\ngithub:\n  token: ghp_test123456789\nagents:\n  quality:\n    backend: copilot\n    enabled: true\n    kick_template: quality-advisory.md\n    mode: ADVISORY\n"
	invalid := "agent_sandbox:\n  setup:\n    health_commands: [\"\"]\n"
	if err := os.WriteFile(source, []byte(seed+invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOverrides(source, "-"); err == nil || !strings.Contains(err.Error(), "agent_sandbox.setup.health_commands[0]") {
		t.Fatalf("load error = %v", err)
	}
}

func TestSandboxSetupSaveReload(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hive.yaml")
	oldOverlay, oldRuntime := DashboardOverlayFile, RuntimeConfigFile
	DashboardOverlayFile, RuntimeConfigFile = filepath.Join(dir, "dashboard"), filepath.Join(dir, "runtime")
	t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = oldOverlay, oldRuntime })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	seed := "project:\n  org: testorg\ngithub:\n  token: ghp_test123456789\nagents:\n  quality:\n    backend: copilot\n    enabled: true\n    kick_template: quality-advisory.md\n    mode: ADVISORY\n"
	if err := os.WriteFile(source, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentSandbox.Setup = testSetup()
	a := cfg.Agents["quality"]
	a.Sandbox = &AgentSandboxOverride{Setup: &SandboxSetupConfig{HealthCommands: []string{"true"}}}
	cfg.Agents["quality"] = a
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWithDashboardOverlay(source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AgentSandbox.Setup, testSetup()) {
		t.Fatalf("global setup lost: %+v", got.AgentSandbox.Setup)
	}
	own := got.Agents["quality"].Sandbox
	if own == nil || own.Setup == nil || !reflect.DeepEqual(own.Setup.HealthCommands, []string{"true"}) || len(own.Setup.Commands) != 0 {
		t.Fatalf("agent setup lost: %+v", own)
	}
}
