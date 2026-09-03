package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/sandbox"
)

// These tests pin the SandboxExecutor's error handling and the pure command/
// binary/report helpers. The happy path and the scrub boundary are covered by
// sandbox_executor_test.go and launch_scrub_test.go; what was missing is the
// contract that EVERY failure surfaces in res.Error (the dashboard renders it)
// AND as a returned error, and that the pure helpers map each backend to the
// documented CLI invocation.

// failingRunner errors on the first git subcommand whose args contain failOn;
// everything else is delegated to the inner runner.
type failingRunner struct {
	inner  sandboxCommandRunner
	failOn string
	// failOnNth makes only the nth matching call fail (1-based); 0 = every match.
	failOnNth int
	seen      int
}

func (r *failingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	joined := strings.Join(stripGitConfigArgs(args), " ")
	if strings.Contains(joined, r.failOn) {
		r.seen++
		if r.failOnNth == 0 || r.seen == r.failOnNth {
			return []byte("boom output"), errors.New("git " + r.failOn + " failed")
		}
	}
	return r.inner.Run(ctx, dir, env, name, args...)
}

type errMinter struct{ err error }

func (m errMinter) MintPushToken(context.Context, string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return "", nil // empty token
}

type fakePRClient struct {
	err    error
	called bool
}

func (c *fakePRClient) CreatePR(ctx context.Context, repo, head, base, title, body string) (ghpkg.CreatePRResult, error) {
	c.called = true
	return ghpkg.CreatePRResult{URL: "https://example.invalid/pr/1"}, c.err
}

func sandboxPathsSpec(workdir string) SandboxKickSpec {
	return SandboxKickSpec{
		Agent:        "scanner",
		AgentConfig:  configSnapshot{Backend: "claude"},
		Message:      "do the thing",
		Org:          "kubestellar",
		Repo:         "hive",
		WorkspaceDir: workdir,
		Timeout:      30 * time.Second,
	}
}

// assertRunFails asserts the (res, err) contract every executor failure must
// satisfy: non-nil error, res.Error carrying the same message.
func assertRunFails(t *testing.T, res SandboxKickResult, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %v, want substring %q", err, wantSubstr)
	}
	if res.Error == "" || !strings.Contains(res.Error, wantSubstr) {
		t.Errorf("res.Error = %q, want substring %q (the dashboard renders this field)", res.Error, wantSubstr)
	}
}

func TestSandboxExecutorRun_PrepareWorkspaceValidation(t *testing.T) {
	e := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: sandboxFakeLauncher{}, Logger: discardLogger()}

	t.Run("missing org", func(t *testing.T) {
		spec := sandboxPathsSpec(t.TempDir())
		spec.Org = "  "
		res, err := e.Run(context.Background(), spec)
		assertRunFails(t, res, err, "requires org and repo")
	})
	t.Run("missing repo", func(t *testing.T) {
		spec := sandboxPathsSpec(t.TempDir())
		spec.Repo = ""
		res, err := e.Run(context.Background(), spec)
		assertRunFails(t, res, err, "requires org and repo")
	})
	t.Run("missing workspace root", func(t *testing.T) {
		spec := sandboxPathsSpec("")
		res, err := e.Run(context.Background(), spec)
		assertRunFails(t, res, err, "workspace root is required")
	})
}

func TestSandboxExecutorRun_GitFailuresSurface(t *testing.T) {
	cases := []struct {
		name    string
		failOn  string
		nth     int
		wantErr string
	}{
		{"clone fails", "clone", 0, "git clone"},
		{"fetch fails", "fetch origin", 0, "git fetch"},
		{"checkout fails", "checkout -B", 0, "git checkout"},
		{"base rev-parse fails", "rev-parse HEAD", 1, "git rev-parse HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &SandboxExecutor{
				Runner:   &failingRunner{inner: &sandboxFakeRunner{}, failOn: tc.failOn, failOnNth: tc.nth},
				Launcher: sandboxFakeLauncher{},
				Logger:   discardLogger(),
			}
			res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
			assertRunFails(t, res, err, tc.wantErr)
		})
	}
}

func TestSandboxExecutorRun_CommitsSinceErrorsSurface(t *testing.T) {
	// The launch itself succeeds; the post-run head rev-parse (2nd rev-parse
	// call) fails, so the executor cannot tell whether the agent committed.
	t.Run("head rev-parse fails", func(t *testing.T) {
		e := &SandboxExecutor{
			Runner:   &failingRunner{inner: &sandboxFakeRunner{}, failOn: "rev-parse HEAD", failOnNth: 2},
			Launcher: sandboxFakeLauncher{},
			Logger:   discardLogger(),
		}
		res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
		assertRunFails(t, res, err, "rev-parse HEAD failed")
	})
	t.Run("rev-list fails", func(t *testing.T) {
		e := &SandboxExecutor{
			Runner:   &failingRunner{inner: &sandboxFakeRunner{commits: 1}, failOn: "rev-list --count", failOnNth: 0},
			Launcher: sandboxFakeLauncher{},
			Logger:   discardLogger(),
		}
		res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
		assertRunFails(t, res, err, "rev-list --count failed")
	})
}

func TestSandboxExecutorRun_PushEnabledWithoutMinterFails(t *testing.T) {
	e := &SandboxExecutor{
		Runner:      &sandboxFakeRunner{commits: 1},
		Launcher:    sandboxFakeLauncher{},
		CloneMinter: sandboxFakeMinter{},
		PushEnabled: true, // commits exist, push wanted, but no Minter wired
		Logger:      discardLogger(),
	}
	res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
	assertRunFails(t, res, err, "push broker minter is not configured")
	if res.Broker != nil {
		t.Error("broker must not run when the minter is missing")
	}
}

func TestSandboxExecutorRun_BrokerFailureSurfaces(t *testing.T) {
	e := &SandboxExecutor{
		Runner:      &failingRunner{inner: &sandboxFakeRunner{commits: 1}, failOn: "push "},
		Launcher:    sandboxFakeLauncher{},
		Minter:      sandboxFakeMinter{},
		PushEnabled: true,
		Logger:      discardLogger(),
	}
	res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
	if err == nil {
		t.Fatal("expected broker error")
	}
	if res.Error == "" {
		t.Error("res.Error must carry the broker failure")
	}
	if res.Broker == nil {
		t.Error("broker result must be attached even on failure")
	}
	if res.PR != nil {
		t.Error("no PR may be opened when the push failed")
	}
}

func TestSandboxExecutorRun_PRFailureSurfaces(t *testing.T) {
	prc := &fakePRClient{err: errors.New("pr create denied")}
	e := &SandboxExecutor{
		Runner:      &sandboxFakeRunner{commits: 1},
		Launcher:    sandboxFakeLauncher{},
		Minter:      sandboxFakeMinter{},
		PushEnabled: true,
		PRClient:    prc,
		Logger:      discardLogger(),
	}
	res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
	assertRunFails(t, res, err, "pr create denied")
	if !prc.called {
		t.Fatal("PR client was never invoked (positive control: push succeeded so a PR must be attempted)")
	}
	if res.PR == nil {
		t.Error("PR result must be attached even on failure")
	}
}

func TestSandboxExecutorRun_PRSuccessAttachesResult(t *testing.T) {
	prc := &fakePRClient{}
	e := &SandboxExecutor{
		Runner:      &sandboxFakeRunner{commits: 1},
		Launcher:    sandboxFakeLauncher{},
		Minter:      sandboxFakeMinter{},
		PushEnabled: true,
		PRClient:    prc,
		Logger:      discardLogger(),
	}
	res, err := e.Run(context.Background(), sandboxPathsSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PR == nil || res.PR.URL == "" {
		t.Fatalf("PR result missing: %+v", res.PR)
	}
	if res.Error != "" {
		t.Errorf("res.Error = %q, want empty on success", res.Error)
	}
}

// ---------------------------------------------------------------------------
// cloneAuthArgs — minter failure modes
// ---------------------------------------------------------------------------

func TestSandboxExecutorCloneAuthArgs(t *testing.T) {
	t.Run("no minter at all is anonymous", func(t *testing.T) {
		e := &SandboxExecutor{}
		args, cleanup, err := e.cloneAuthArgs(context.Background(), "kubestellar/hive", t.TempDir())
		if err != nil {
			t.Fatalf("cloneAuthArgs: %v", err)
		}
		defer cleanup()
		if len(args) != 0 {
			t.Errorf("anonymous clone must add no git args, got %v", args)
		}
	})
	t.Run("mint error propagates", func(t *testing.T) {
		e := &SandboxExecutor{CloneMinter: errMinter{err: errors.New("mint offline")}}
		_, _, err := e.cloneAuthArgs(context.Background(), "kubestellar/hive", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "minting clone token") {
			t.Fatalf("error = %v, want minting clone token wrap", err)
		}
	})
	t.Run("empty token rejected", func(t *testing.T) {
		e := &SandboxExecutor{CloneMinter: errMinter{}}
		_, _, err := e.cloneAuthArgs(context.Background(), "kubestellar/hive", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "empty clone token") {
			t.Fatalf("error = %v, want empty clone token rejection", err)
		}
	})
}

// ---------------------------------------------------------------------------
// sandboxCommand / sandboxBackendBinary — pure mapping helpers
// ---------------------------------------------------------------------------

func TestSandboxCommand_BackendInvocations(t *testing.T) {
	cases := []struct {
		name string
		cfg  configSnapshot
		want string
	}{
		{"empty backend defaults to claude", configSnapshot{}, "claude --dangerously-skip-permissions"},
		{"claude with model", configSnapshot{Backend: "claude", Model: "opus"}, "claude --model 'opus' --dangerously-skip-permissions"},
		{"goose without model", configSnapshot{Backend: "goose"}, "goose run -s"},
		{"goose with model", configSnapshot{Backend: "goose", Model: "gpt"}, "goose run -s --model 'gpt'"},
		{"pi uses goose binary", configSnapshot{Backend: "pi"}, "goose run -s"},
		{"copilot without model", configSnapshot{Backend: "copilot"}, "copilot --no-auto-update --allow-all"},
		{"copilot with model", configSnapshot{Backend: "copilot", Model: "gpt-5"}, "copilot --no-auto-update --allow-all --model 'gpt-5'"},
		{"launch_cmd wins over backend", configSnapshot{Backend: "copilot", LaunchCmd: "/usr/bin/custom --flag"}, "/usr/bin/custom --flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := sandboxCommand(tc.cfg, sandboxPromptRelPath)
			if err != nil {
				t.Fatalf("sandboxCommand: %v", err)
			}
			if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-lc" {
				t.Fatalf("command = %v, want sh -lc wrapper", cmd)
			}
			if !strings.HasPrefix(cmd[2], tc.want+" ") && cmd[2] != tc.want {
				t.Errorf("shell command = %q, want prefix %q", cmd[2], tc.want)
			}
			if !strings.Contains(cmd[2], "< '"+sandboxPromptRelPath+"'") {
				t.Errorf("shell command %q must feed the prompt file on stdin", cmd[2])
			}
		})
	}
}

func TestSandboxBackendBinary(t *testing.T) {
	cases := map[string]string{
		"copilot": "copilot",
		"gemini":  "gemini",
		"goose":   "goose",
		"pi":      "goose",
		"bob":     "bob",
		"claude":  "claude",
		"litellm": "claude", // inference backends run through the claude CLI
		"":        "claude",
	}
	for backend, want := range cases {
		if got := sandboxBackendBinary(backend); got != want {
			t.Errorf("sandboxBackendBinary(%q) = %q, want %q", backend, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// collectAgentReport — prefixed-report discovery and invalid-schema fallback
// ---------------------------------------------------------------------------

func TestCollectAgentReport_FindsPrefixedReportFile(t *testing.T) {
	ws := t.TempDir()
	// A report named with the conventional prefix anywhere in the tree (not at
	// the two fixed candidate paths) must be discovered by the walk.
	sub := filepath.Join(ws, "deep", "dir")
	if err := os.MkdirAll(sub, 0o770); err != nil {
		t.Fatal(err)
	}
	valid := `{"lane":"scanner","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok"}`
	path := filepath.Join(sub, "agent-report-x.json")
	if err := os.WriteFile(path, []byte(valid), 0o660); err != nil {
		t.Fatal(err)
	}
	gotPath, report := collectAgentReport(ws)
	if gotPath != path {
		t.Errorf("report path = %q, want %q", gotPath, path)
	}
	if report == nil || report.Summary != "ok" {
		t.Errorf("report = %+v, want validated summary ok", report)
	}
}

func TestCollectAgentReport_InvalidSchemaStillReturnsPath(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".hive")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	// Valid JSON, invalid schema: the path is surfaced (operator can inspect)
	// but no parsed report is returned.
	if err := os.WriteFile(filepath.Join(dir, "agent-report.json"), []byte(`{"unexpected":"shape"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	gotPath, report := collectAgentReport(ws)
	if gotPath == "" {
		t.Error("path must be surfaced for a JSON report that fails schema validation")
	}
	if report != nil {
		t.Errorf("report = %+v, want nil for schema-invalid JSON", report)
	}
}

func TestCollectAgentReport_NothingFound(t *testing.T) {
	gotPath, report := collectAgentReport(t.TempDir())
	if gotPath != "" || report != nil {
		t.Errorf("empty workspace: got (%q, %+v), want (\"\", nil)", gotPath, report)
	}
}

// ---------------------------------------------------------------------------
// Zero-value accessors and the real exec runner
// ---------------------------------------------------------------------------

func TestSandboxExecutorZeroValueAccessors(t *testing.T) {
	e := &SandboxExecutor{}
	if _, ok := e.launcher().(sandbox.PodmanLauncher); !ok {
		t.Errorf("default launcher = %T, want sandbox.PodmanLauncher", e.launcher())
	}
	if _, ok := e.runner().(sandboxExecRunner); !ok {
		t.Errorf("default runner = %T, want sandboxExecRunner", e.runner())
	}
	if e.now().IsZero() {
		t.Error("default now() must return the current time")
	}
	fixed := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return fixed }
	if !e.now().Equal(fixed) {
		t.Errorf("injected now() = %v, want %v", e.now(), fixed)
	}
	if got := e.branchName("My Agent"); got != "hive/my-agent/20260817000000" {
		t.Errorf("branchName = %q, want deterministic timestamped branch", got)
	}
}

func TestSandboxExecRunnerRunsRealCommand(t *testing.T) {
	dir := t.TempDir()
	out, err := sandboxExecRunner{}.Run(context.Background(), dir, []string{"PATH=" + os.Getenv("PATH")}, "pwd")
	if err != nil {
		t.Fatalf("exec runner: %v", err)
	}
	// cmd.Dir must be honored: pwd prints the requested working directory.
	got := strings.TrimSpace(string(out))
	want, _ := filepath.EvalSymlinks(dir)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Errorf("pwd in runner = %q (resolved %q), want %q", got, gotResolved, want)
	}
}

// ---------------------------------------------------------------------------
// tieredSandboxMinterLocked — tier stamping across minter shapes
// ---------------------------------------------------------------------------

func TestTieredSandboxMinterLocked(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	t.Run("nil minter passes through", func(t *testing.T) {
		m.SetSandboxPushMinter(nil)
		if got := m.tieredSandboxMinterLocked("trusted"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("value GitHubAppMinter gets tier stamped", func(t *testing.T) {
		m.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Tier: "old"})
		got, ok := m.tieredSandboxMinterLocked("trusted").(pushbroker.GitHubAppMinter)
		if !ok || got.Tier != "trusted" {
			t.Errorf("got %+v (ok=%v), want value minter with tier trusted", got, ok)
		}
	})
	t.Run("pointer GitHubAppMinter copied with tier, original untouched", func(t *testing.T) {
		orig := &pushbroker.GitHubAppMinter{Tier: "old"}
		m.SetSandboxPushMinter(orig)
		got, ok := m.tieredSandboxMinterLocked("newcomer").(pushbroker.GitHubAppMinter)
		if !ok || got.Tier != "newcomer" {
			t.Errorf("got %+v (ok=%v), want copied minter with tier newcomer", got, ok)
		}
		if orig.Tier != "old" {
			t.Errorf("original minter mutated to tier %q; must be copied", orig.Tier)
		}
	})
	t.Run("nil pointer GitHubAppMinter yields nil", func(t *testing.T) {
		var p *pushbroker.GitHubAppMinter
		m.SetSandboxPushMinter(p)
		if got := m.tieredSandboxMinterLocked("trusted"); got != nil {
			t.Errorf("got %v, want nil for nil *GitHubAppMinter", got)
		}
	})
	t.Run("foreign minter passes through unchanged", func(t *testing.T) {
		m.SetSandboxPushMinter(sandboxFakeMinter{})
		if _, ok := m.tieredSandboxMinterLocked("trusted").(sandboxFakeMinter); !ok {
			t.Error("non-GitHubApp minter must pass through unchanged")
		}
	})
}

// ---------------------------------------------------------------------------
// SetSandboxAuditCallback / auditSandbox
// ---------------------------------------------------------------------------

func TestSandboxAuditCallback(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	// No callback wired: must be a silent no-op.
	m.auditSandbox("scanner", "kick", "detail")

	var gotAgent, gotAction, gotDetail string
	m.SetSandboxAuditCallback(func(agent, action, detail string) {
		gotAgent, gotAction, gotDetail = agent, action, detail
	})
	m.auditSandbox("scanner", "kick", "detail")
	if gotAgent != "scanner" || gotAction != "kick" || gotDetail != "detail" {
		t.Errorf("callback got (%q,%q,%q), want (scanner,kick,detail)", gotAgent, gotAction, gotDetail)
	}

	// Clearing the callback restores the no-op.
	m.SetSandboxAuditCallback(nil)
	gotAgent = ""
	m.auditSandbox("scanner", "kick", "detail")
	if gotAgent != "" {
		t.Error("cleared callback must not fire")
	}
}
