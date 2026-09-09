package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/sandbox"
)

// The Job runtime (#6311) must route a sandboxed kick through the launcher the
// factory builds from the agent's job block, NOT through the Podman launcher
// installed with SetSandboxLauncher — and the rest of the kick (workspace,
// commits, state machine) must be untouched.

type countingLauncher struct{ runs *int32 }

func (c countingLauncher) Run(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
	atomic.AddInt32(c.runs, 1)
	return sandbox.Result{Stdout: "ok"}, nil
}

func jobRuntimeManager(t *testing.T, override *config.AgentSandboxOverride, global config.AgentSandboxConfig) (*Manager, *int32, *int32) {
	t.Helper()
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude", Sandbox: override}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	if global.WorkspaceDir == "" {
		global.WorkspaceDir = t.TempDir()
	}
	m.SetSandboxConfig(global)
	var podmanRuns, jobRuns int32
	m.SetSandboxLauncher(countingLauncher{runs: &podmanRuns})
	m.setSandboxJobLauncherFactoryForTest(func(job config.SandboxJobConfig) sandbox.Launcher {
		if job.WorkspaceClaim != "hive-data" {
			t.Errorf("factory got job.WorkspaceClaim=%q, want hive-data", job.WorkspaceClaim)
		}
		return countingLauncher{runs: &jobRuns}
	})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m, &podmanRuns, &jobRuns
}

func waitIdle(t *testing.T, m *Manager, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.AllStatuses()[name].State == StateIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %s did not return to idle: %+v", name, m.AllStatuses()[name])
}

func TestJobRuntimeKickUsesTheJobLauncher(t *testing.T) {
	yes := true
	m, podman, job := jobRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob,
			Job: &config.SandboxJobConfig{NodeSelector: map[string]string{"accelerator": "spyre"}}},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}})
	if err := m.SendKick("deps", "bump the pinned dependency"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "deps")
	if got := atomic.LoadInt32(job); got != 1 {
		t.Errorf("job launcher runs = %d, want 1", got)
	}
	if got := atomic.LoadInt32(podman); got != 0 {
		t.Errorf("podman launcher ran %d time(s) for a job-runtime agent", got)
	}
}

// POSITIVE CONTROL: the default runtime still goes through the Podman
// launcher, so the routing above is a real branch and not an always-job path.
func TestDefaultRuntimeKickUsesThePodmanLauncher(t *testing.T) {
	yes := true
	m, podman, job := jobRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes},
		config.AgentSandboxConfig{Enabled: true, Image: "agent-image",
			Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}})
	if err := m.SendKick("deps", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "deps")
	if got := atomic.LoadInt32(podman); got != 1 {
		t.Errorf("podman launcher runs = %d, want 1", got)
	}
	if got := atomic.LoadInt32(job); got != 0 {
		t.Errorf("job launcher ran %d time(s) for a podman-runtime agent", got)
	}
}

func TestJobRuntimeRefusesKickWithoutWorkspaceClaim(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude",
		Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob}}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "dev-image", WorkspaceDir: t.TempDir()})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := m.SendKick("deps", "fix")
	if err == nil || !strings.Contains(err.Error(), "workspace_claim") {
		t.Fatalf("SendKick error = %v, want a refusal naming job.workspace_claim", err)
	}
	if got := m.AllStatuses()["deps"].State; got != StateIdle {
		t.Errorf("a refused kick must leave the agent idle, got %s", got)
	}
}

func TestUnknownSandboxRuntimeRefusesKick(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude",
		Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: "lambda"}}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "dev-image", WorkspaceDir: t.TempDir()})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := m.SendKick("deps", "fix")
	if err == nil || !strings.Contains(err.Error(), `"lambda"`) {
		t.Fatalf("SendKick error = %v, want a refusal naming the unknown runtime", err)
	}
}

func TestJobOptionsFromConfig(t *testing.T) {
	got := JobOptionsFromConfig(config.SandboxJobConfig{
		WorkspaceClaim:      "hive-data",
		WorkspaceClaimMount: "/data",
		NodeSelector:        map[string]string{"accelerator": "spyre"},
		Tolerations:         []config.SandboxJobToleration{{Key: "spyre", Operator: "Exists", Effect: "NoSchedule"}},
		Resources:           config.SandboxJobResources{Limits: map[string]string{"ibm.com/spyre_pf": "2"}, Requests: map[string]string{"cpu": "4"}},
		ServiceAccount:      "hive",
		EnvFromSecrets:      []string{"artifact-registry"},
		Volumes:             []config.SandboxJobVolume{{Name: "cache", Claim: "compile-cache", MountPath: "/cache", ReadOnly: true}},
		TTLSeconds:          600,
	})
	if got.WorkspaceClaim != "hive-data" || got.WorkspaceClaimMount != "/data" || got.ServiceAccount != "hive" || got.TTLSeconds != 600 {
		t.Errorf("scalar fields not carried: %+v", got)
	}
	if got.NodeSelector["accelerator"] != "spyre" {
		t.Errorf("node selector not carried: %+v", got.NodeSelector)
	}
	if len(got.Tolerations) != 1 || got.Tolerations[0].Key != "spyre" || got.Tolerations[0].Effect != "NoSchedule" {
		t.Errorf("tolerations not carried: %+v", got.Tolerations)
	}
	if got.Resources.Limits["ibm.com/spyre_pf"] != "2" || got.Resources.Requests["cpu"] != "4" {
		t.Errorf("resources not carried: %+v", got.Resources)
	}
	if len(got.EnvFromSecrets) != 1 || got.EnvFromSecrets[0] != "artifact-registry" {
		t.Errorf("env_from_secrets not carried: %+v", got.EnvFromSecrets)
	}
	if len(got.ExtraVolumes) != 1 || got.ExtraVolumes[0].Claim != "compile-cache" || !got.ExtraVolumes[0].ReadOnly {
		t.Errorf("volumes not carried: %+v", got.ExtraVolumes)
	}
}

// With no factory injected the manager must build the real in-cluster
// launcher rather than nil — a nil launcher would fall back to Podman inside
// the executor and silently run the kick on the hive node.
func TestJobLauncherDefaultsToKubejob(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, quietTestLogger(), ProjectContext{})
	l := m.jobLauncherLocked(config.SandboxJobConfig{WorkspaceClaim: "hive-data"})
	if l == nil {
		t.Fatal("jobLauncherLocked returned nil")
	}
	if _, isPodman := l.(sandbox.PodmanLauncher); isPodman {
		t.Fatal("job runtime must not resolve to the Podman launcher")
	}
}
