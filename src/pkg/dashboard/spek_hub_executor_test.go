package dashboard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

func TestSpekHubExecutorClaimPreservesTriageAndSkipsRelayLease(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	runKey := "myorg/repo1#57"
	key := spekRepo + "!" + runKey + ":" + StageSpec
	admitTask := runAdmissionTaskPrefix + sanitizeReceiptSegment(runKey)
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, admitTask)] = &taskLease{identity: runAdmissionIdentity, taskID: admitTask, repo: spekRepo, number: 57, key: key, title: "Do thing", stage: StageSpec, gen: 1, triageVerdict: "spec", triageRationale: "needs design", expiresAt: now.Add(leaseTTL)}
	hub.leases[leaseKey("relay", "relay-task")] = &taskLease{identity: "relay", taskID: "relay-task", repo: spekRepo, number: 58, key: spekRepo + "!myorg/repo1#58:" + StageSpec, stage: StageSpec, gen: 1, expiresAt: now.Add(leaseTTL)}
	if err := hub.saveLeasesLocked(); err != nil {
		t.Fatal(err)
	}
	hub.leaseMu.Unlock()
	worktree := runStageWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey, StageSpec, 1)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".spektacular"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 2, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	e.Exec = func(context.Context, string, []string, string, ...string) ([]byte, error) { return []byte("ok"), nil }
	stages, err := e.unclaimedStages()
	if err != nil || len(stages) != 1 {
		t.Fatalf("unclaimed stages = %d, %v", len(stages), err)
	}
	if err := e.executeStage(context.Background(), stages[0]); err != nil {
		t.Fatalf("executeStage: %v", err)
	}
	hub.leaseMu.Lock()
	if hub.leases[leaseKey(runAdmissionIdentity, admitTask)] != nil {
		hub.leaseMu.Unlock()
		t.Fatal("admission lease still present after claim")
	}
	claimed := hub.leases[leaseKey(config.DefaultSpektacularHubExecutorIdentity, spekHubExecutorTaskPrefix+sanitizeReceiptSegment(runKey)+"-spec-1")]
	if claimed == nil {
		hub.leaseMu.Unlock()
		t.Fatal("hub executor lease not recorded")
	}
	if claimed.triageVerdict != "spec" || claimed.triageRationale != "needs design" {
		hub.leaseMu.Unlock()
		t.Fatalf("triage not preserved: %#v", claimed)
	}
	if hub.leases[leaseKey("relay", "relay-task")] == nil {
		hub.leaseMu.Unlock()
		t.Fatal("relay-held lease was touched")
	}
	hub.leaseMu.Unlock()
	var owners []string
	if err := s.VisitActiveStageLeases(func(rk, _, stage, identity, _, _ string, _ uint64, _ time.Time) {
		if rk == runKey && stage == StageSpec {
			owners = append(owners, identity)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0] != config.DefaultSpektacularHubExecutorIdentity {
		t.Fatalf("active leases for run = %v, want only executor", owners)
	}
}

func TestSpekHubExecutorPrepareWorkspaceCreatesCloneAndWorktree(t *testing.T) {
	_, s, _, _ := spekHub(t)
	remote := makeBareRepo(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	e.CloneURL = func(string) string { return remote }
	e.Exec = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if name == "git" {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Dir = dir
			cmd.Env = env
			return cmd.CombinedOutput()
		}
		if name == "spektacular" {
			return nil, os.MkdirAll(filepath.Join(dir, ".spektacular"), 0o755)
		}
		return []byte("ok"), nil
	}
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, repo: spekRepo, gen: 1}
	if _, err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentWorkspaceRoot, e.Identity, filepath.FromSlash(spekRepo), ".git")); err != nil {
		t.Fatalf("shared clone missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runStageWorktreePath(e.Identity, st.runKey, st.stage, st.gen), ".spektacular")); err != nil {
		t.Fatalf("worktree/project missing: %v", err)
	}
}

func TestSpekHubStagePromptSpecContainsRequiredInstructions(t *testing.T) {
	p := SpekHubStagePrompt(StageSpec, "kubestellar/console", 23735, "kubestellar/console#23735", "Great feature", "kubestellar-console-23735")
	for _, want := range []string{"kubestellar-console-23735", "kubestellar/console#23735", "spektacular spec new", "Do not implement code, do not commit, do not push", "Hive-Run: kubestellar/console#23735"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestSpekHubExecutorFailureRecordsAuditTimelineAndBudget(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, taskID: "task", gen: 1}
	key := e.executionKey(st)
	e.recordFailure(st, key, errors.New("boom"))
	if e.Status().LastError != "boom" {
		t.Fatalf("last error not recorded: %#v", e.Status())
	}
	found := false
	for _, ev := range s.LifecycleTimeline().ByIssue(st.runKey) {
		if ev.Kind == timeline.KindBlocked && ev.Attrs[stageAttrReason] == spekHubFailureReason {
			found = true
		}
	}
	if !found {
		t.Fatal("failure did not record blocked timeline event")
	}
	if e.failures[key] != e.maxAttempts() {
		t.Fatalf("failure budget = %d, want %d", e.failures[key], e.maxAttempts())
	}
}

func TestSpekHubExecutorEnvUsesIsolatedHomeAndAppToken(t *testing.T) {
	_, s, _, _ := spekHub(t)
	t.Setenv("GITHUB_TOKEN", "old")
	t.Setenv("GH_TOKEN", "old")
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	env, err := e.executorEnv("app-token")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		byKey[k] = v
	}
	if byKey["HOME"] != filepath.Join(agentWorkspaceRoot, e.Identity, "home") {
		t.Fatalf("HOME = %q", byKey["HOME"])
	}
	if byKey["GH_TOKEN"] != "app-token" || byKey["GITHUB_TOKEN"] != "app-token" {
		t.Fatalf("github tokens not overridden: GH=%q GITHUB=%q", byKey["GH_TOKEN"], byKey["GITHUB_TOKEN"])
	}
}

func TestSpekHubExecutorSweepRemovesStaleWorktree(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	stale := runStageWorktreePath(e.Identity, "myorg/repo1#57", StageSpec, 1)
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale worktree still exists: %v", err)
	}
}

func makeBareRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(work, "init")
	run(work, "config", "user.email", "hive@example.com")
	run(work, "config", "user.name", "Hive")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "README.md")
	run(work, "commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "remote.git")
	run(work, "clone", "--bare", work, bare)
	return bare
}
