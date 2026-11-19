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

// The OpenShell runtime (ADR-0026) routes a sandboxed kick through the
// launcher built from the agent's openshell block, refuses a kick whose
// gateway is unconfigured before the state machine moves, and stays inert
// for every agent that did not select it.

func openshellRuntimeManager(t *testing.T, override *config.AgentSandboxOverride, global config.AgentSandboxConfig) (*Manager, *int32, *int32) {
	t.Helper()
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude", Sandbox: override}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	if global.WorkspaceDir == "" {
		global.WorkspaceDir = t.TempDir()
	}
	m.SetSandboxConfig(global)
	var podmanRuns, openshellRuns int32
	m.SetSandboxLauncher(countingLauncher{runs: &podmanRuns})
	m.setSandboxOpenShellLauncherFactoryForTest(func(os config.SandboxOpenShellConfig) sandbox.Launcher {
		if os.GatewayURL != "gw.openshell.svc:8080" {
			t.Errorf("factory got gateway_url=%q", os.GatewayURL)
		}
		return countingLauncher{runs: &openshellRuns}
	})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m, &podmanRuns, &openshellRuns
}

func TestOpenShellRuntimeKickUsesTheOpenShellLauncher(t *testing.T) {
	yes := true
	m, podman, osRuns := openshellRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeOpenShell},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			OpenShell: &config.SandboxOpenShellConfig{GatewayURL: "gw.openshell.svc:8080", WorkspaceClaim: "hive-data"}})
	if err := m.SendKick("deps", "bump the pinned dependency"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "deps")
	if got := atomic.LoadInt32(osRuns); got != 1 {
		t.Errorf("openshell launcher runs = %d, want 1", got)
	}
	if got := atomic.LoadInt32(podman); got != 0 {
		t.Errorf("podman launcher ran %d time(s) for an openshell-runtime agent", got)
	}
}

func TestOpenShellRuntimeRefusesKickWithoutGateway(t *testing.T) {
	yes := true
	m, _, osRuns := openshellRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeOpenShell},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			OpenShell: &config.SandboxOpenShellConfig{WorkspaceClaim: "hive-data"}})
	err := m.SendKick("deps", "fix")
	if err == nil || !strings.Contains(err.Error(), "gateway_url") {
		t.Fatalf("SendKick error = %v, want a refusal naming gateway_url", err)
	}
	if got := atomic.LoadInt32(osRuns); got != 0 {
		t.Errorf("launcher ran %d time(s) despite the refusal", got)
	}
	if st := m.AllStatuses()["deps"].State; st == StateRunning {
		t.Errorf("agent marked running after a refused kick")
	}
}

func TestOpenShellOptionsFromConfig(t *testing.T) {
	t.Setenv("OPENSHELL_TEST_TOKEN", "sekrit")
	opts := OpenShellOptionsFromConfig(config.SandboxOpenShellConfig{
		GatewayURL:     "https://gw",
		Namespace:      "hive",
		PolicyProfile:  "strict",
		TimeoutS:       90,
		TokenEnv:       "OPENSHELL_TEST_TOKEN",
		WorkspaceClaim: "hive-data",
		AllowHosts:     []string{"litellm.example.com"},
		Tolerations:    []config.SandboxJobToleration{{Key: "k", Operator: "Exists"}},
	})
	if opts.Token != "sekrit" || opts.Workspace != "hive" || opts.PolicyProfile != "strict" || opts.Timeout != 90*time.Second {
		t.Errorf("opts = %+v", opts)
	}
	if len(opts.Tolerations) != 1 || opts.Tolerations[0].Key != "k" {
		t.Errorf("tolerations = %+v", opts.Tolerations)
	}
	if err := opts.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestOpenShellLauncherDefaultsToSDK(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, quietTestLogger(), ProjectContext{})
	l := m.openshellLauncherLocked(config.SandboxOpenShellConfig{GatewayURL: "gw:1"})
	if _, ok := l.(*sandbox.OpenShellLauncher); !ok {
		t.Fatalf("launcher = %T, want *sandbox.OpenShellLauncher", l)
	}
}
