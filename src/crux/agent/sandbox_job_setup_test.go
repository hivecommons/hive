package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
)

// hivecommons/hive#19574: a failed sandbox.setup command fails the kick (not an
// infrastructure idle), and the manager wires the setup step into the launcher.

func TestSetupFailedKickFailsTheAgentWithTheReason(t *testing.T) {
	failure := &sandbox.FailError{Reason: sandbox.FailReasonSetupFailed, Node: "n1", Message: "hive-sandbox-setup: FAILED setup command 1 of 1 exited 1: ./install.sh"}
	m, audits := infraManager(t, infraLauncher{
		res: sandbox.Result{Stdout: "pip: mirror unreachable\n", ExitCode: kubejob.ExitCodeSetupFailed, Node: "n1", FailReason: sandbox.FailReasonSetupFailed},
		err: failure,
	})
	if err := m.SendKick("deps", "bump the dependency"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	pollUntil(t, "the agent to fail", func() bool { return m.AllStatuses()["deps"].State == StateFailed })
	p := m.AllStatuses()["deps"]
	if p.SandboxLastRun == nil || p.SandboxLastRun.FailReason != sandbox.FailReasonSetupFailed || p.SandboxLastRun.Succeeded {
		t.Fatalf("run record = %+v, want setup_failed", p.SandboxLastRun)
	}
	if !strings.Contains(p.LastError, "setup_failed") || !strings.Contains(p.LastError, "./install.sh") {
		t.Errorf("LastError = %q, want the reason and the command", p.LastError)
	}
	if got := audits.find(auditSandboxSetupFailed); !strings.Contains(got, "node=n1") {
		t.Errorf("setup audit = %q, want an entry naming the node", got)
	}
	if audits.find("sandbox_failed") == "" {
		t.Error("a setup failure is a failed kick and must be audited as sandbox_failed")
	}
	if got := audits.find(auditSandboxNodeUnhealthy); got != "" {
		t.Errorf("a setup failure was audited as a node failure: %q", got)
	}
}

func TestPrepareJobKickWiresSetup(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"deps": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob}},
		"own": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob,
			Setup: &config.SandboxSetupConfig{HealthCommands: []string{"own-health"}}}},
		"off": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob, Setup: &config.SandboxSetupConfig{}}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "o", Repos: []string{"r"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "img", WorkspaceDir: t.TempDir(),
		Job:   &config.SandboxJobConfig{WorkspaceClaim: "hive-data"},
		Setup: &config.SandboxSetupConfig{Commands: []string{"./install.sh"}, HealthCommands: []string{"python smoke"}, TimeoutS: 30, TotalTimeoutS: 60}})
	m.mu.Lock()
	defer m.mu.Unlock()

	launcher := &kubejob.Launcher{}
	m.prepareJobKickLocked(launcher, m.agents["deps"], config.SandboxRuntimeJob)
	o := launcher.Options
	if strings.Join(o.Setup, ",") != "./install.sh" || strings.Join(o.SetupHealth, ",") != "python smoke" || o.SetupTimeout != 30*time.Second || o.SetupTotalTimeout != time.Minute {
		t.Fatalf("setup options = %+v", o)
	}
	own := &kubejob.Launcher{}
	m.prepareJobKickLocked(own, m.agents["own"], config.SandboxRuntimeJob)
	if len(own.Options.Setup) != 0 || strings.Join(own.Options.SetupHealth, ",") != "own-health" || own.Options.SetupTotalTimeout != time.Minute {
		t.Errorf("an agent's own block must replace the lists and inherit zero timeouts: %+v", own.Options)
	}
	off := &kubejob.Launcher{}
	m.prepareJobKickLocked(off, m.agents["off"], config.SandboxRuntimeJob)
	if len(off.Options.Setup)+len(off.Options.SetupHealth) != 0 {
		t.Errorf("an empty per-agent block must turn setup off: %+v", off.Options)
	}
	podman := &kubejob.Launcher{}
	m.prepareJobKickLocked(podman, m.agents["deps"], config.SandboxRuntimePodman)
	if len(podman.Options.Setup) != 0 {
		t.Errorf("setup leaked into a non-job runtime: %+v", podman.Options)
	}
}

func TestSandboxEnvironmentNamesTheSetupStep(t *testing.T) {
	setup := config.SandboxSetupConfig{Commands: []string{"./install.sh", " "}, HealthCommands: []string{"python smoke"}}
	env := sandboxEnvironmentFor(config.SandboxRuntimeJob, deviceJob(), config.SandboxPreflightConfig{}, true).withSetup(&kubejob.Launcher{}, setup)
	if env == nil || env.SetupCommands != 1 || env.SetupHealthCommands != 1 {
		t.Fatalf("env = %+v", env)
	}
	if section := sandboxEnvironmentPromptSection(env); !strings.Contains(section, "Workspace setup: the 1 configured sandbox.setup command(s) and 1 health check(s) already ran") {
		t.Errorf("section lacks the setup line: %q", section)
	}
	unwired := sandboxEnvironmentFor(config.SandboxRuntimeJob, deviceJob(), config.SandboxPreflightConfig{}, true).withSetup(sandboxFakeLauncher{}, setup)
	if unwired.SetupCommands != 0 || strings.Contains(sandboxEnvironmentPromptSection(unwired), "Workspace setup") {
		t.Errorf("a launcher that does not run the step must not claim it ran: %+v", unwired)
	}
	var none *sandboxEnvironment
	if none.withSetup(&kubejob.Launcher{}, setup) != nil {
		t.Error("a nil environment must stay nil")
	}
}
