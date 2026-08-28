package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	ghpkg "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/pushbroker"
	"github.com/kubestellar/hive/pkg/sandbox"
)

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
	case joined == "diff --name-only basesha...HEAD":
		return []byte("file.txt\n"), nil
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
}

func (l sandboxFakeLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	if l.waitForCancel {
		<-ctx.Done()
		return sandbox.Result{Stderr: ctx.Err().Error(), ExitCode: -1}, ctx.Err()
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

func (p *sandboxFakePR) CreatePR(context.Context, string, string, string, string, string) (ghpkg.CreatePRResult, error) {
	p.called = true
	return ghpkg.CreatePRResult{Number: 1, URL: "https://example.test/pr/1"}, nil
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
	m.SetSandboxLauncher(sandboxFakeLauncher{})
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
	if err := m.SendKick("scanner", "again"); err == nil {
		t.Fatal("second kick while sandbox is running should be refused")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := m.AllStatuses()["scanner"].State; got == StateIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sandbox agent did not return to idle: %+v", m.AllStatuses()["scanner"])
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		done := m.agents["scanner"].cancel == nil && m.agents["scanner"].State == StatePaused
		m.mu.RUnlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sandbox agent did not stay paused after cancellation: %+v", m.AllStatuses()["scanner"])
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		done := m.agents["scanner"].cancel == nil && m.agents["scanner"].State == StateIdle
		m.mu.RUnlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sandbox agent did not return idle after resumed cancellation: %+v", m.AllStatuses()["scanner"])
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		done := m.agents["scanner"].cancel == nil
		m.mu.RUnlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sandbox cleanup did not finish")
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := m.AllStatuses()["scanner"].State; got == StateIdle {
			if runner.pushed || pr.called {
				t.Fatalf("non-push mode pushed=%v pr=%v", runner.pushed, pr.called)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sandbox agent did not complete: %+v", m.AllStatuses()["scanner"])
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
