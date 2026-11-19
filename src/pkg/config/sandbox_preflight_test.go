package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testPreflight() *SandboxPreflightConfig {
	return &SandboxPreflightConfig{Commands: []string{`python -c 'import torch; torch.ones(1).to("spyre")'`}, TimeoutS: 60, TotalTimeoutS: 120, RetryDelayS: 3}
}

func TestSandboxPreflightRejectedAtValidate(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{AgentSandbox: AgentSandboxConfig{Preflight: &SandboxPreflightConfig{Commands: []string{" "}}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agent_sandbox.preflight.commands[0]") {
		t.Fatalf("global empty command error = %v", err)
	}
	cfg.AgentSandbox = AgentSandboxConfig{Job: &SandboxJobConfig{ScheduleTimeoutS: -1}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agent_sandbox.job.schedule_timeout_s") {
		t.Fatalf("global negative schedule timeout error = %v", err)
	}
	cfg.AgentSandbox = AgentSandboxConfig{}
	cfg.Agents = map[string]AgentConfig{"quality": {Sandbox: &AgentSandboxOverride{Preflight: &SandboxPreflightConfig{Commands: []string{"true"}, TimeoutS: -1}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agents.quality.sandbox.preflight.timeout_s") {
		t.Fatalf("agent negative timeout error = %v", err)
	}
	cfg.Agents = map[string]AgentConfig{"quality": {Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{ScheduleTimeoutS: -1}}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "agents.quality.sandbox.job.schedule_timeout_s") {
		t.Fatalf("agent negative schedule timeout error = %v", err)
	}
}

func TestSandboxPreflightRejectedOnLoad(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "hive.yaml")
	seed := "project:\n  org: testorg\ngithub:\n  token: ghp_test123456789\nagents:\n  quality:\n    backend: copilot\n    enabled: true\n    kick_template: quality-advisory.md\n    mode: ADVISORY\n"
	invalid := "agent_sandbox:\n  preflight:\n    commands: [\"\"]\n"
	if err := os.WriteFile(source, []byte(seed+invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOverrides(source, "-"); err == nil || !strings.Contains(err.Error(), "agent_sandbox.preflight.commands[0]") {
		t.Fatalf("load error = %v", err)
	}
}

func TestSandboxPreflightSaveReload(t *testing.T) {
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
	cfg.AgentSandbox.Preflight = testPreflight()
	cfg.AgentSandbox.Job = &SandboxJobConfig{ScheduleTimeoutS: 900}
	a := cfg.Agents["quality"]
	a.Sandbox = &AgentSandboxOverride{Preflight: &SandboxPreflightConfig{Commands: []string{"true"}, RetryDelayS: 1}, Job: &SandboxJobConfig{ScheduleTimeoutS: 30}}
	cfg.Agents["quality"] = a
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWithDashboardOverlay(source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AgentSandbox.Preflight, testPreflight()) {
		t.Fatalf("global preflight lost: %+v", got.AgentSandbox.Preflight)
	}
	if got.AgentSandbox.Job == nil || got.AgentSandbox.Job.ScheduleTimeoutS != 900 {
		t.Fatalf("global schedule timeout lost: %+v", got.AgentSandbox.Job)
	}
	own := got.Agents["quality"].Sandbox
	if own == nil || own.Preflight == nil || !reflect.DeepEqual(own.Preflight.Commands, []string{"true"}) || own.Preflight.RetryDelayS != 1 {
		t.Fatalf("agent preflight lost: %+v", own)
	}
	if own.Job == nil || own.Job.ScheduleTimeoutS != 30 {
		t.Fatalf("agent schedule timeout lost: %+v", own)
	}
}
