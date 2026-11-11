package config

import (
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
)

func TestBenchmarksConfigDefaultsOn(t *testing.T) {
	var b BenchmarksConfig
	if !b.PublishEnabled() || !b.RepoConsentEnabled() {
		t.Fatal("unset benchmarks block must publish and name public repos")
	}
	off := false
	b = BenchmarksConfig{Publish: &off, RepoConsent: &off}
	if b.PublishEnabled() || b.RepoConsentEnabled() {
		t.Fatal("explicit false must opt out")
	}
}

func TestBenchmarkSandboxRuntime(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.BenchmarkSandboxRuntime("a"); got != BenchmarkSandboxRuntimeNone {
		t.Fatalf("nil config = %q", got)
	}
	on, off := true, false
	cfg := &Config{}
	cfg.AgentSandbox = cruxconfig.AgentSandboxConfig{Enabled: false, Runtime: "kubernetes-job"}
	cfg.Agents = map[string]cruxconfig.AgentConfig{
		"opted-in":  {Sandbox: &cruxconfig.AgentSandboxOverride{Enabled: &on, Runtime: " openshell "}},
		"inherits":  {Sandbox: &cruxconfig.AgentSandboxOverride{Enabled: &on}},
		"opted-out": {Sandbox: &cruxconfig.AgentSandboxOverride{Enabled: &off}},
	}
	for agent, want := range map[string]string{
		"opted-in":  "openshell",
		"inherits":  "kubernetes-job",
		"opted-out": BenchmarkSandboxRuntimeNone,
		"unknown":   BenchmarkSandboxRuntimeNone, // global sandbox off
	} {
		if got := cfg.BenchmarkSandboxRuntime(agent); got != want {
			t.Errorf("%s: got %q want %q", agent, got, want)
		}
	}
	cfg.AgentSandbox.Enabled = true
	if got := cfg.BenchmarkSandboxRuntime("unknown"); got != "kubernetes-job" {
		t.Errorf("global on, unlisted agent: got %q", got)
	}
	cfg.AgentSandbox.Runtime = ""
	if got := cfg.BenchmarkSandboxRuntime("unknown"); got != cruxconfig.SandboxRuntimePodman {
		t.Errorf("default runtime: got %q", got)
	}
}
