package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// The Substrate runtime (ADR-0026 phase 2) routes a sandboxed kick through
// the launcher built from the agent's substrate block, refuses a kick whose
// options are invalid before the state machine moves, and stays inert for
// every agent that did not select it.

const substrateTestImage = "ghcr.io/hivecommons/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func substrateRuntimeManager(t *testing.T, override *config.AgentSandboxOverride, global config.AgentSandboxConfig) (*Manager, *int32, *int32) {
	t.Helper()
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude", Sandbox: override}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	if global.WorkspaceDir == "" {
		global.WorkspaceDir = t.TempDir()
	}
	m.SetSandboxConfig(global)
	var podmanRuns, substrateRuns int32
	m.SetSandboxLauncher(countingLauncher{runs: &podmanRuns})
	m.setSandboxSubstrateLauncherFactoryForTest(func(sb config.SandboxSubstrateConfig) sandbox.Launcher {
		if sb.Atespace != "hive-test" {
			t.Errorf("factory got atespace=%q", sb.Atespace)
		}
		return countingLauncher{runs: &substrateRuns}
	})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m, &podmanRuns, &substrateRuns
}

func TestSubstrateRuntimeKickUsesTheSubstrateLauncher(t *testing.T) {
	yes := true
	m, podman, sbRuns := substrateRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeSubstrate},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			Substrate: &config.SandboxSubstrateConfig{Atespace: "hive-test", Image: substrateTestImage}})
	if err := m.SendKick("deps", "bump the pinned dependency"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "deps")
	if got := atomic.LoadInt32(sbRuns); got != 1 {
		t.Errorf("substrate launcher runs = %d, want 1", got)
	}
	if got := atomic.LoadInt32(podman); got != 0 {
		t.Errorf("podman launcher ran %d time(s) for a substrate-runtime agent", got)
	}
}

func TestSubstrateRuntimeRefusesKickWithTagOnlyImage(t *testing.T) {
	yes := true
	m, _, sbRuns := substrateRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeSubstrate},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			Substrate: &config.SandboxSubstrateConfig{Atespace: "hive-test", Image: "ghcr.io/x/y:latest"}})
	err := m.SendKick("deps", "fix")
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("SendKick error = %v, want a refusal naming the digest rule", err)
	}
	if got := atomic.LoadInt32(sbRuns); got != 0 {
		t.Errorf("launcher ran %d time(s) despite the refusal", got)
	}
	if st := m.AllStatuses()["deps"].State; st == StateRunning {
		t.Errorf("agent marked running after a refused kick")
	}
}

func TestSubstrateRuntimeRefusesCline(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{"deps": {Backend: "cline", Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeSubstrate}}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "dev-image", WorkspaceDir: t.TempDir(),
		Substrate: &config.SandboxSubstrateConfig{Image: substrateTestImage}})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := m.SendKick("deps", "fix")
	if err == nil || !strings.Contains(err.Error(), "Cline") {
		t.Fatalf("SendKick error = %v, want the Cline refusal", err)
	}
}

func TestSubstrateOptionsFromConfig(t *testing.T) {
	t.Setenv("SUBSTRATE_TEST_TOKEN", "sekrit")
	opts := SubstrateOptionsFromConfig(config.SandboxSubstrateConfig{
		Endpoint:       "api.ate-system.svc:443",
		Atespace:       "hive",
		PolicyProfile:  "strict",
		TimeoutS:       90,
		TokenEnv:       "SUBSTRATE_TEST_TOKEN",
		Image:          substrateTestImage,
		BridgeCommand:  []string{"/bin/bridge"},
		AllowHosts:     []string{"litellm.example.com"},
		WorkerSelector: map[string]string{"class": "gvisor"},
		SnapshotOnIdle: true,
	})
	if opts.Token != "sekrit" || opts.Atespace != "hive" || opts.PolicyProfile != "strict" || opts.Timeout != 90*time.Second || !opts.SnapshotOnIdle {
		t.Errorf("opts = %+v", opts)
	}
	if len(opts.Command) != 1 || opts.Command[0] != "/bin/bridge" || opts.WorkerSelector["class"] != "gvisor" {
		t.Errorf("command/selector = %+v", opts)
	}
	if err := opts.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestSubstrateLauncherDefaultsToControlAPI(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, quietTestLogger(), ProjectContext{})
	l := m.substrateLauncherLocked(config.SandboxSubstrateConfig{Atespace: "hive"})
	if _, ok := l.(*sandbox.SubstrateLauncher); !ok {
		t.Fatalf("launcher = %T, want *sandbox.SubstrateLauncher", l)
	}
}

func TestSubstratePreflightWarningsDedupesControlPlanes(t *testing.T) {
	// Two agents on the same control plane probe once. A CA file that does
	// not exist fails the client constructor before any network I/O, which
	// is reported as the control plane being unreachable.
	blocks := []config.SandboxSubstrateConfig{
		{Endpoint: "127.0.0.1:1", Atespace: "dedupe-" + t.Name(), CAFile: "/nonexistent/ca.crt"},
		{Endpoint: "127.0.0.1:1", Atespace: "dedupe-" + t.Name(), CAFile: "/nonexistent/ca.crt"},
	}
	got := substratePreflightWarnings(blocks)
	if len(got) != 1 || !strings.Contains(got[0], "unreachable") {
		t.Fatalf("warnings = %q, want exactly one unreachable warning for the shared control plane", got)
	}
}
