package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/outputschema"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/sandbox"
)

const sandboxStateTransitionTimeout = 30 * time.Second

type sandboxFakeRunner struct {
	mu       sync.Mutex
	envs     [][]string
	pushed   bool
	commits  int
	cloneDir string
	revParse int
	// defaultBranch is what `git symbolic-ref --short refs/remotes/origin/HEAD`
	// reports for the clone; tests that don't care about default-branch
	// resolution leave it at the zero value, which resolves to "main" below so
	// existing fetch/checkout expectations keep working unchanged.
	defaultBranch string
}

func (r *sandboxFakeRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.envs = append(r.envs, append([]string(nil), env...))
	r.mu.Unlock()
	if name != "git" {
		return nil, errors.New("unexpected command")
	}
	effective := stripGitConfigArgs(args)
	joined := strings.Join(effective, " ")
	switch {
	case len(effective) >= 4 && effective[0] == "clone":
		workspace := effective[len(effective)-1]
		r.cloneDir = workspace
		if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o770); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(filepath.Join(workspace, ".git", "config"), []byte("[remote]\nurl = "+effective[len(effective)-2]+"\n"), 0o660)
	case joined == "symbolic-ref --short refs/remotes/origin/HEAD":
		branch := r.defaultBranch
		if branch == "" {
			branch = "main"
		}
		return []byte("origin/" + branch + "\n"), nil
	case joined == "fetch origin main", strings.HasPrefix(joined, "checkout -B "):
		return []byte("ok\n"), nil
	case joined == "rev-parse HEAD":
		r.revParse++
		if r.commits > 0 && r.revParse > 1 {
			return []byte("headsha\n"), nil
		}
		return []byte("basesha\n"), nil
	case joined == "rev-list --count basesha..HEAD":
		if r.commits > 0 {
			return []byte("1\n"), nil
		}
		return []byte("0\n"), nil
	case joined == "rev-parse --verify basesha":
		return []byte("basesha\n"), nil
	case joined == "diff --no-renames --name-only -z basesha...HEAD":
		return []byte("file.txt\x00"), nil
	case joined == "diff --no-ext-diff basesha...HEAD":
		return []byte("diff --git a/file.txt b/file.txt\n"), nil
	case strings.Contains(joined, "push ") && strings.Contains(joined, " origin HEAD:refs/heads/"):
		r.pushed = true
		return []byte("pushed\n"), nil
	default:
		return []byte(""), nil
	}
}

func stripGitConfigArgs(args []string) []string {
	out := append([]string(nil), args...)
	for len(out) >= 2 && out[0] == "-c" {
		out = out[2:]
	}
	return out
}

type sandboxFakeLauncher struct {
	waitForCancel bool
	// started, when non-nil, is closed once Run begins so a test can prove
	// the sandbox execution is in flight. Single-use: reuse across kicks
	// would close it twice and panic.
	started chan struct{}
	// release, when non-nil, blocks Run until the test closes it (or the
	// context is cancelled). This pins the "sandbox is running" window open
	// deterministically instead of racing the goroutine to completion.
	release chan struct{}
}

func (l sandboxFakeLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	if l.started != nil {
		close(l.started)
	}
	if l.waitForCancel {
		<-ctx.Done()
		return sandbox.Result{Stderr: ctx.Err().Error(), ExitCode: -1}, ctx.Err()
	}
	if l.release != nil {
		select {
		case <-l.release:
		case <-ctx.Done():
			return sandbox.Result{Stderr: ctx.Err().Error(), ExitCode: -1}, ctx.Err()
		}
	}
	if err := os.WriteFile(filepath.Join(spec.Workspace, "agent-report.json"), []byte(`{"lane":"scanner","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok"}`), 0o660); err != nil {
		return sandbox.Result{}, err
	}
	return sandbox.Result{Stdout: "done\n"}, nil
}

type sandboxFakeMinter struct{}

func (sandboxFakeMinter) MintPushToken(context.Context, string) (string, error) {
	return "ghs_fake", nil
}

type sandboxFakePR struct{ called bool }

func (p *sandboxFakePR) CreatePR(context.Context, string, string, string, string, string) (crux.PRResult, error) {
	p.called = true
	return crux.PRResult{Number: 1, URL: "https://example.test/pr/1"}, nil
}

func TestSandboxExecutorWorkspacePrepSanitizesCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	t.Setenv("SAFE_ENV", "ok")
	root := t.TempDir()
	runner := &sandboxFakeRunner{}
	exec := &SandboxExecutor{Runner: runner, Launcher: sandboxFakeLauncher{}, Now: func() time.Time { return time.Unix(1, 0) }}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent:        "scanner",
		AgentConfig:  configSnapshot{Backend: "claude"},
		Message:      "fix",
		Org:          "kubestellar",
		Repo:         "hive",
		WorkspaceDir: root,
		Image:        "agent-image",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Workspace == "" || res.Report == nil || res.TranscriptPath == "" {
		t.Fatalf("result missing collected artifacts: %+v", res)
	}
	// The workspace and transcript follow the shared sandbox naming so the
	// dashboard's job-runtime terminal pane can find them by agent name
	// (hivecommons/hive#14798), and the transcript ends with the exit code.
	if want := sandbox.WorkspaceDir(root, "scanner", time.Unix(1, 0).UnixNano()); res.Workspace != want {
		t.Fatalf("workspace = %q, want %q", res.Workspace, want)
	}
	if latest, ok := sandbox.LatestTranscript(root, "scanner"); !ok || latest != res.TranscriptPath {
		t.Fatalf("LatestTranscript = %q, %v; want %q", latest, ok, res.TranscriptPath)
	}
	transcript, err := os.ReadFile(res.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if code, ok := sandbox.TranscriptExitCode(string(transcript)); !ok || code != 0 {
		t.Fatalf("transcript exit code = %d, %v; transcript:\n%s", code, ok, transcript)
	}
	if !strings.HasPrefix(string(transcript), "stdout:\ndone\n") {
		t.Fatalf("transcript body changed shape:\n%s", transcript)
	}
	for _, env := range runner.envs {
		for _, pair := range env {
			if strings.HasPrefix(pair, "GITHUB_TOKEN=") {
				t.Fatalf("credential leaked to git environment: %q", pair)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(res.Workspace, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghs_secret") {
		t.Fatal("credential leaked into workspace git config")
	}
}

func TestSandboxExecutorBrokerHandoff(t *testing.T) {
	tests := []struct {
		name       string
		commits    int
		wantPush   bool
		wantPR     bool
		withBroker bool
	}{
		{name: "no commits skips broker", commits: 0},
		{name: "commits push and open pr", commits: 1, wantPush: true, wantPR: true, withBroker: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &sandboxFakeRunner{commits: tt.commits}
			pr := &sandboxFakePR{}
			exec := &SandboxExecutor{Runner: runner, Launcher: sandboxFakeLauncher{}, PRClient: pr, PushEnabled: tt.withBroker}
			if tt.withBroker {
				exec.Minter = sandboxFakeMinter{}
			}
			_, err := exec.Run(context.Background(), SandboxKickSpec{
				Agent: "scanner", AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix",
				Org: "kubestellar", Repo: "hive", WorkspaceDir: t.TempDir(), Image: "agent-image",
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if runner.pushed != tt.wantPush {
				t.Fatalf("pushed=%v want %v", runner.pushed, tt.wantPush)
			}
			if pr.called != tt.wantPR {
				t.Fatalf("pr.called=%v want %v", pr.called, tt.wantPR)
			}
		})
	}
}

func TestSandboxExecutorTimeout(t *testing.T) {
	runner := &sandboxFakeRunner{}
	exec := &SandboxExecutor{Runner: runner, Launcher: sandboxFakeLauncher{waitForCancel: true}}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "scanner", AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix",
		Org: "kubestellar", Repo: "hive", WorkspaceDir: t.TempDir(), Image: "agent-image", Timeout: time.Millisecond,
	})
	if err == nil || !res.TimedOut {
		t.Fatalf("expected timeout, got err=%v result=%+v", err, res)
	}
}

func TestManagerSandboxStateMachine(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	// Gate the fake launcher: without the release channel the sandbox
	// goroutine can finish and return the agent to idle before the second
	// SendKick below, so the "refused" assertion raced the scheduler and
	// flaked under full-suite load.
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	var completeOnce sync.Once
	m.SetSandboxAuditCallback(func(agent, action, detail string) {
		if agent == "scanner" && action == "sandbox_complete" {
			completeOnce.Do(func() { close(completed) })
		}
	})
	m.SetSandboxLauncher(sandboxFakeLauncher{started: started, release: release})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := m.AllStatuses()["scanner"].State; got != StateIdle {
		t.Fatalf("state after Start=%s want idle", got)
	}
	if err := m.SendKick("scanner", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	select {
	case <-started:
	case <-time.After(sandboxStateTransitionTimeout):
		t.Fatal("sandbox launcher was never invoked")
	}
	if err := m.SendKick("scanner", "again"); err == nil {
		t.Fatal("second kick while sandbox is running should be refused")
	}
	close(release)
	select {
	case <-completed:
	case <-time.After(sandboxStateTransitionTimeout):
		t.Fatalf("sandbox completion audit did not arrive: %+v", m.AllStatuses()["scanner"])
	}
	if got := m.AllStatuses()["scanner"].State; got != StateIdle {
		t.Fatalf("sandbox agent state after completion=%s want idle: %+v", got, m.AllStatuses()["scanner"])
	}
}

func TestManagerSandboxRespectsPause(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Paused: true, Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := m.AllStatuses()["scanner"].State; got != StatePaused {
		t.Fatalf("state after Start=%s want paused", got)
	}
	if err := m.SendKick("scanner", "fix"); err == nil {
		t.Fatal("paused sandbox agent should reject kicks")
	}
	if err := m.Resume(context.Background(), "scanner", "test", "resume"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := m.AllStatuses()["scanner"].State; got != StateIdle {
		t.Fatalf("state after Resume=%s want idle", got)
	}
}

func TestManagerSandboxPauseCancelsRunningKick(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(sandboxFakeLauncher{waitForCancel: true})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("scanner", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	if err := m.Pause("scanner", "test", "pause while running"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitFor(t, "sandbox agent to stay paused after cancellation", func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.agents["scanner"].cancel == nil && m.agents["scanner"].State == StatePaused
	})
}

func TestManagerSandboxResumeWhileCancelDrainsReturnsIdle(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(sandboxFakeLauncher{waitForCancel: true})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("scanner", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	if err := m.Pause("scanner", "test", "pause while running"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := m.Resume(context.Background(), "scanner", "test", "resume immediately"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, "sandbox agent to return idle after resumed cancellation", func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.agents["scanner"].cancel == nil && m.agents["scanner"].State == StateIdle
	})
}

func TestManagerSandboxRunningSkippedByTmuxCrashDetector(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(sandboxFakeLauncher{waitForCancel: true})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("scanner", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	if restarted := m.CheckAndRestartCrashedAgents(context.Background()); len(restarted) != 0 {
		t.Fatalf("sandbox agent should not be tmux-restarted: %v", restarted)
	}
	if got := m.AllStatuses()["scanner"].State; got != StateRunning {
		t.Fatalf("state=%s want running", got)
	}
	if err := m.Pause("scanner", "test", "cleanup"); err != nil {
		t.Fatalf("Pause cleanup: %v", err)
	}
	waitFor(t, "sandbox cleanup to finish", func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.agents["scanner"].cancel == nil
	})
}

func TestManagerSandboxDoesNotPushWhenModeCannotPush(t *testing.T) {
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	runner := &sandboxFakeRunner{commits: 1}
	pr := &sandboxFakePR{}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}, ACMMLevel: 1, PRsAllowed: true})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(sandboxFakeLauncher{})
	m.setSandboxRunnerForTest(runner)
	m.SetSandboxPushMinter(sandboxFakeMinter{})
	m.SetSandboxPRClient(pr)
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("scanner", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitFor(t, "sandbox agent to complete", func() bool { return m.AllStatuses()["scanner"].State == StateIdle })
	if runner.pushed || pr.called {
		t.Fatalf("non-push mode pushed=%v pr=%v", runner.pushed, pr.called)
	}
}

func TestAgentSandboxConfigGating(t *testing.T) {
	yes := true
	agent := &config.AgentConfig{Sandbox: &config.AgentSandboxOverride{Enabled: &yes, NetworkMode: "none", TimeoutS: 7}}
	global := config.AgentSandboxConfig{Enabled: true, NetworkMode: "restricted", TimeoutS: 30}
	if !agent.SandboxEnabled(global) {
		t.Fatal("agent should be sandbox-enabled")
	}
	if got := agent.SandboxNetworkMode(global); got != "none" {
		t.Fatalf("network mode=%q", got)
	}
	if got := agent.SandboxTimeoutS(global); got != 7 {
		t.Fatalf("timeout=%d", got)
	}
}

func TestSandboxExecutorPodmanIntegrationSkippedWhenUnavailable(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("podman not available")
	}
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

var _ pushbroker.TokenMinter = sandboxFakeMinter{}

// sandboxReportLauncher lets a test pick the agent-report.json the sandbox
// leaves behind and the exit code the launcher reports, so the publish gate
// can be exercised on every input it reads.
type sandboxReportLauncher struct {
	report   string
	exitCode int
}

func (l sandboxReportLauncher) Run(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	if l.report != "" {
		dir := filepath.Join(spec.Workspace, ".hive")
		if err := os.MkdirAll(dir, 0o770); err != nil {
			return sandbox.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "agent-report.json"), []byte(l.report), 0o660); err != nil {
			return sandbox.Result{}, err
		}
	}
	return sandbox.Result{Stdout: "done\n", ExitCode: l.exitCode}, nil
}

// A commit is not a success signal. When the agent's report says the run
// failed (or the sandbox exited non-zero) hive must keep the branch local
// and open no PR — the a1 deps run that opened a PR after its RPM install
// 401'd is the case this guards against.
func TestSandboxExecutorPublishGate(t *testing.T) {
	const okReport = `{"lane":"deps","kind":"fix","findings":[],"prs_opened":[],"beads_filed":[],"summary":"bumped"%s}`
	tests := []struct {
		name       string
		report     string
		exitCode   int
		commits    int
		wantPR     bool
		wantReason string
		wantStatus outputschema.RunStatus
	}{
		{name: "success status publishes", report: fmt.Sprintf(okReport, `,"status":"success"`), commits: 1, wantPR: true, wantStatus: outputschema.StatusSuccess},
		{name: "unspecified status publishes", report: fmt.Sprintf(okReport, ""), commits: 1, wantPR: true},
		{name: "no report publishes", commits: 1, wantPR: true},
		{name: "failed status withholds", report: fmt.Sprintf(okReport, `,"status":"failed"`), commits: 1, wantReason: "agent report status=failed: bumped", wantStatus: outputschema.StatusFailed},
		{name: "blocked status withholds", report: fmt.Sprintf(okReport, `,"status":"blocked"`), commits: 1, wantReason: "agent report status=blocked", wantStatus: outputschema.StatusBlocked},
		{name: "malformed report still carries status", report: `{"status":"failed","summary":"rpm install 401"}`, commits: 1, wantReason: "agent report status=failed", wantStatus: outputschema.StatusFailed},
		{name: "non-zero exit withholds", report: fmt.Sprintf(okReport, ""), exitCode: 3, commits: 1, wantReason: "sandbox exited 3"},
		{name: "failed status without commits reports failure", report: fmt.Sprintf(okReport, `,"status":"failed"`), wantReason: "agent report status=failed", wantStatus: outputschema.StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &sandboxFakeRunner{commits: tt.commits}
			pr := &sandboxFakePR{}
			exec := &SandboxExecutor{
				Runner:      runner,
				Launcher:    sandboxReportLauncher{report: tt.report, exitCode: tt.exitCode},
				PRClient:    pr,
				PushEnabled: true,
				Minter:      sandboxFakeMinter{},
			}
			res, err := exec.Run(context.Background(), SandboxKickSpec{
				Agent: "deps", AgentConfig: configSnapshot{Backend: "claude"}, Message: "bump",
				Org: "kubestellar", Repo: "hive", WorkspaceDir: t.TempDir(), Image: "agent-image",
			})
			if res.ReportStatus != tt.wantStatus {
				t.Errorf("ReportStatus = %q, want %q", res.ReportStatus, tt.wantStatus)
			}
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if !runner.pushed || !pr.called {
					t.Fatalf("pushed=%v pr.called=%v, want both true", runner.pushed, pr.called)
				}
				if res.PRWithheld != "" {
					t.Fatalf("PRWithheld = %q, want empty", res.PRWithheld)
				}
				return
			}
			if !errors.Is(err, ErrSandboxRunReportedFailure) {
				t.Fatalf("Run error = %v, want ErrSandboxRunReportedFailure", err)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error %q does not name reason %q", err, tt.wantReason)
			}
			if runner.pushed || pr.called {
				t.Fatalf("pushed=%v pr.called=%v after a reported failure", runner.pushed, pr.called)
			}
			if tt.commits > 0 {
				if !strings.Contains(res.PRWithheld, tt.wantReason) {
					t.Errorf("PRWithheld = %q, want it to contain %q", res.PRWithheld, tt.wantReason)
				}
				if !strings.Contains(err.Error(), "1 commit(s) not pushed, no PR opened") {
					t.Errorf("error %q should say the commits were not pushed", err)
				}
			} else if res.PRWithheld != "" {
				t.Errorf("PRWithheld = %q with no commits, want empty", res.PRWithheld)
			}
		})
	}
}
