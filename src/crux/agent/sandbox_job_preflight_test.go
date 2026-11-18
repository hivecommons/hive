package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
)

// auditLog collects sandbox audit entries from the kick goroutine.
type auditLog struct {
	mu      sync.Mutex
	entries []string
}

func (a *auditLog) add(agent, action, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, agent+":"+action+":"+detail)
}

func (a *auditLog) find(action string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if strings.Contains(e, ":"+action+":") {
			return e
		}
	}
	return ""
}

// infraLauncher is a Job launcher whose run fails for the infrastructure's
// reason, as kubejob reports it.
type infraLauncher struct {
	res sandbox.Result
	err error
}

func (l infraLauncher) Run(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
	return l.res, l.err
}

func infraManager(t *testing.T, launcher sandbox.Launcher) (*Manager, *auditLog) {
	t.Helper()
	yes := true
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob}}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "dev-image", WorkspaceDir: t.TempDir(),
		Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}})
	audits := &auditLog{}
	m.SetSandboxAuditCallback(audits.add)
	m.setSandboxJobLauncherFactoryForTest(func(config.SandboxJobConfig) sandbox.Launcher { return launcher })
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m, audits
}

// pollUntil re-checks cond on a ticker until it holds or the deadline passes.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func waitLastRun(t *testing.T, m *Manager) *crux.Process {
	t.Helper()
	pollUntil(t, "the kick to finish", func() bool {
		p := m.AllStatuses()["deps"]
		return p.SandboxLastRun != nil && p.State != StateRunning
	})
	return m.AllStatuses()["deps"]
}

func TestNodeUnhealthyKickIsNotAnAgentFailure(t *testing.T) {
	failure := &sandbox.FailError{Reason: sandbox.FailReasonNodeUnhealthy, Node: "p1-worker-93", Message: "hive-node-preflight: FAILED command 1 of 1 exited 1: python smoke"}
	m, audits := infraManager(t, infraLauncher{
		res: sandbox.Result{Stdout: "torch: no spyre device\n", ExitCode: kubejob.ExitCodeNodeUnhealthy, Node: "p1-worker-93", FailReason: sandbox.FailReasonNodeUnhealthy},
		err: failure,
	})
	if err := m.SendKick("deps", "bump the dependency"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	p := waitLastRun(t, m)
	if p.State != StateIdle {
		t.Errorf("state = %s, want idle: the agent never ran, so it is not failed", p.State)
	}
	run := p.SandboxLastRun
	if run.Succeeded || run.FailReason != sandbox.FailReasonNodeUnhealthy || run.Node != "p1-worker-93" {
		t.Errorf("run record = %+v, want node_unhealthy on p1-worker-93", run)
	}
	if !strings.Contains(p.LastError, "node_unhealthy") || !strings.Contains(p.LastError, "p1-worker-93") {
		t.Errorf("LastError = %q, want the reason and the node", p.LastError)
	}
	if got := audits.find("sandbox_node_unhealthy"); !strings.Contains(got, "node=p1-worker-93") {
		t.Errorf("audit = %q, want an entry naming the node", got)
	}
	if got := audits.find("sandbox_failed"); got != "" {
		t.Errorf("a node failure was audited as an agent failure: %q", got)
	}
}

func TestWaitingForNodeKickIsNotAnAgentFailure(t *testing.T) {
	m, audits := infraManager(t, infraLauncher{
		res: sandbox.Result{ExitCode: -1, FailReason: sandbox.FailReasonWaitingForNode},
		err: &sandbox.FailError{Reason: sandbox.FailReasonWaitingForNode, Message: "no node took the pod within 10m0s: 0/38 nodes are available"},
	})
	if err := m.SendKick("deps", "bump"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	p := waitLastRun(t, m)
	if p.State != StateIdle || p.SandboxLastRun.FailReason != sandbox.FailReasonWaitingForNode {
		t.Errorf("state=%s run=%+v, want idle with waiting_for_node", p.State, p.SandboxLastRun)
	}
	if got := audits.find("sandbox_waiting_for_node_failed"); !strings.Contains(got, "0/38 nodes are available") {
		t.Errorf("audit = %q, want the scheduler's reason", got)
	}
}

// POSITIVE CONTROL: an ordinary launcher failure is still a failed agent, so
// the two tests above are a real branch and not an always-idle path.
func TestOrdinaryJobFailureStillFailsTheAgent(t *testing.T) {
	m, audits := infraManager(t, infraLauncher{res: sandbox.Result{ExitCode: 3}, err: errors.New("kubejob: job x failed: BackoffLimitExceeded")})
	if err := m.SendKick("deps", "bump"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	pollUntil(t, "the agent to fail", func() bool { return m.AllStatuses()["deps"].State == StateFailed })
	p := m.AllStatuses()["deps"]
	if p.State != StateFailed || p.SandboxLastRun == nil || p.SandboxLastRun.FailReason != "" {
		t.Fatalf("state=%s run=%+v, want failed with no infrastructure reason", p.State, p.SandboxLastRun)
	}
	if audits.find("sandbox_failed") == "" {
		t.Error("an ordinary failure must still be audited as sandbox_failed")
	}
}

func TestBuildSandboxRunCarriesTheInfrastructureReason(t *testing.T) {
	now := time.Now()
	run := buildSandboxRun("job", now, now, SandboxKickResult{Sandbox: sandbox.Result{Node: "n1", FailReason: sandbox.FailReasonNodeUnhealthy}}, errors.New("x"))
	if run.FailReason != sandbox.FailReasonNodeUnhealthy || run.Node != "n1" || run.Succeeded {
		t.Errorf("run = %+v", run)
	}
	if ok := buildSandboxRun("job", now, now, SandboxKickResult{Sandbox: sandbox.Result{Node: "n2"}}, nil); ok.FailReason != "" || ok.Node != "n2" || !ok.Succeeded {
		t.Errorf("a successful run still names its node: %+v", ok)
	}
}

func TestPrepareJobKickWiresPreflightAndEvents(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob}}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "o", Repos: []string{"r"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "img", WorkspaceDir: t.TempDir(),
		Job:       &config.SandboxJobConfig{WorkspaceClaim: "hive-data", ScheduleTimeoutS: 120},
		Preflight: &config.SandboxPreflightConfig{Commands: []string{"python smoke"}, TimeoutS: 30, TotalTimeoutS: 60, RetryDelayS: 2}})
	audits := &auditLog{}
	m.SetSandboxAuditCallback(audits.add)
	if err := m.Start(context.Background(), "deps"); err != nil {
		t.Fatal(err)
	}

	launcher := &kubejob.Launcher{}
	m.mu.Lock()
	agent := m.agents["deps"]
	m.prepareJobKickLocked(launcher, agent, config.SandboxRuntimeJob)
	m.mu.Unlock()
	o := launcher.Options
	if len(o.Preflight) != 1 || o.Preflight[0] != "python smoke" || o.PreflightTimeout != 30*time.Second || o.PreflightTotalTimeout != time.Minute || o.PreflightRetryDelay != 2*time.Second {
		t.Fatalf("preflight options = %+v", o)
	}
	if o.OnEvent == nil {
		t.Fatal("the launcher's events are not wired")
	}
	if got := JobOptionsFromConfig(*m.sandboxConfig.Job).ScheduleTimeout; got != 2*time.Minute {
		t.Errorf("schedule timeout = %s, want 2m", got)
	}

	o.OnEvent(kubejob.Event{Kind: kubejob.EventWaitingForNode, Detail: "0/3 nodes are available"})
	ph := m.AllStatuses()["deps"].SandboxPhase
	if ph == nil || ph.Phase != sandbox.FailReasonWaitingForNode || ph.Detail != "0/3 nodes are available" {
		t.Fatalf("phase = %+v, want waiting_for_node", ph)
	}
	if audits.find("sandbox_waiting_for_node") == "" {
		t.Error("waiting for a node is not audited")
	}
	o.OnEvent(kubejob.Event{Kind: kubejob.EventScheduled, Node: "n1"})
	if ph := m.AllStatuses()["deps"].SandboxPhase; ph != nil {
		t.Errorf("phase = %+v, want it cleared once the pod has a node", ph)
	}
	o.OnEvent(kubejob.Event{Kind: kubejob.EventNodeRetry, Node: "n1", Detail: "smoke failed"})
	if got := audits.find("sandbox_node_unhealthy_retry"); !strings.Contains(got, "node=n1") {
		t.Errorf("retry audit = %q, want the node", got)
	}
}

func TestPrepareJobKickLeavesOtherLaunchersAndRuntimesAlone(t *testing.T) {
	cfg := map[string]config.AgentConfig{"deps": {Backend: "claude"}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{})
	m.SetSandboxConfig(config.AgentSandboxConfig{Preflight: &config.SandboxPreflightConfig{Commands: []string{"smoke"}}})
	m.mu.Lock()
	defer m.mu.Unlock()
	agent := m.agents["deps"]
	podman := &kubejob.Launcher{}
	m.prepareJobKickLocked(podman, agent, config.SandboxRuntimePodman)
	if len(podman.Options.Preflight) != 0 || podman.Options.OnEvent != nil {
		t.Errorf("a preflight leaked into a non-job runtime: %+v", podman.Options)
	}
	m.prepareJobKickLocked(infraLauncher{}, agent, config.SandboxRuntimeJob) // a factory-supplied launcher: no panic, no change
	unconfigured := NewManager(cfg, quietTestLogger(), ProjectContext{})
	unconfigured.mu.Lock()
	defer unconfigured.mu.Unlock()
	plain := &kubejob.Launcher{}
	unconfigured.prepareJobKickLocked(plain, unconfigured.agents["deps"], config.SandboxRuntimeJob)
	if len(plain.Options.Preflight) != 0 {
		t.Errorf("preflight must be off unless configured, got %v", plain.Options.Preflight)
	}
}
