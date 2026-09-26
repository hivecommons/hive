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
	"github.com/hivecommons/hive/pkg/worksource"
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
	if _, err := os.Stat(filepath.Join(spekHubRunWorktreePath(e.Identity, st.runKey), ".spektacular")); err != nil {
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

func TestSpekHubStagePromptOmitsEmptyTitleQuotes(t *testing.T) {
	p := SpekHubStagePrompt(StageSpec, "kubestellar/console", 23725, "kubestellar/console#23725", "", "kubestellar-console-23725")
	if strings.Contains(p, `#23725 ""`) {
		t.Fatalf("prompt retained empty title quotes:\n%s", p)
	}

}

func TestSpekHubStagePromptNonGitHubUsesCapturedWorkItem(t *testing.T) {
	p := SpekHubStagePromptWithContext(StagePlan, "acme/widgets", 0, "acme/widgets!LIN-7", "Fallback", "acme-widgets-LIN-7", worksource.WorkItemContext{
		SourceType: "linear",
		Repo:       "acme/widgets",
		ExternalID: "LIN-7",
		Title:      "Linear title",
		Body:       "Linear description body",
		URL:        "https://linear.app/acme/issue/LIN-7",
	})
	for _, want := range []string{"Source work item: linear LIN-7", "Linear title", "Linear description body", "https://linear.app/acme/issue/LIN-7", "Target repository: acme/widgets", "spektacular plan new"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "gh issue view") {
		t.Fatalf("non-GitHub prompt must not ask for gh issue view:\n%s", p)
	}
}

func TestSpekHubExecutorTickRestartsOwnStalledLeaseAndReportsRunning(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	runKey := "myorg/repo1#57"
	taskID := spekHubExecutorTaskPrefix + sanitizeReceiptSegment(runKey) + "-spec-2"
	key := spekRepo + "!" + runKey + ":" + StageSpec
	hub.leaseMu.Lock()
	hub.leases[leaseKey(config.DefaultSpektacularHubExecutorIdentity, taskID)] = &taskLease{identity: config.DefaultSpektacularHubExecutorIdentity, taskID: taskID, repo: spekRepo, number: 57, key: key, title: "Do thing", stage: StageSpec, gen: 2, expiresAt: now.Add(leaseTTL)}
	if err := hub.saveLeasesLocked(); err != nil {
		t.Fatal(err)
	}
	hub.leaseMu.Unlock()
	worktree := spekHubRunWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey)
	if err := os.MkdirAll(filepath.Join(worktree, ".spektacular"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentWorkspaceRoot, config.DefaultSpektacularHubExecutorIdentity, filepath.FromSlash(spekRepo), ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	launched := make(chan struct{})
	release := make(chan struct{})
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 2, Spektacular: config.SpektacularConfig{Enabled: true, HubExecutor: config.SpektacularHubExecutorConfig{MaxConcurrent: 1}}}, "copilot", "", nil, nil)
	e.Exec = func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		if name == "git" || name == "spektacular" {
			if name == "spektacular" && len(args) >= 3 && args[1] == "status" {
				return []byte(`{"error":false,"kind":"spec","name":"kubestellar-console-57","document_status":"final"}`), nil
			}
			return []byte("ok"), nil
		}
		close(launched)
		<-release
		return []byte("agent done"), nil
	}
	e.Tick(context.Background(), now)
	<-launched
	if got := e.Status().Running; got != 1 {
		t.Fatalf("running = %d, want 1", got)
	}
	close(release)
}

func TestResolveRunStageWorkDirUsesSingleHubWorktreeAcrossRetryGenerations(t *testing.T) {
	_, s, _, _ := spekHub(t)
	runKey := "myorg/repo1#57"
	worktree := spekHubRunWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey)
	if err := os.MkdirAll(filepath.Join(worktree, ".spektacular", "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveRunStageWorkDir(runKey, StageSpec, config.DefaultSpektacularHubExecutorIdentity, spekRepo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != worktree {
		t.Fatalf("workdir = %q, want %q", got, worktree)
	}
}

func TestSpekHubExecutorCopiesPreviousArtifactsIntoSingleWorktree(t *testing.T) {
	_, _, _, _ = spekHub(t)
	runKey := "myorg/repo1#57"
	oldSpec := filepath.Join(runStageWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey, StageSpec, 1), ".spektacular", "specs")
	if err := os.MkdirAll(oldSpec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSpec, "20260925163042-myorg-repo1-57.md"), []byte("spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := spekHubRunWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey)
	copied, err := copyPreviousSpektacularProject(config.DefaultSpektacularHubExecutorIdentity, runKey, worktree)
	if err != nil || !copied {
		t.Fatalf("copyPreviousSpektacularProject copied=%v err=%v", copied, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".spektacular", "specs", "20260925163042-myorg-repo1-57.md")); err != nil {
		t.Fatalf("artifact not copied: %v", err)
	}
}

func TestSpekHubExecutorCLIExitNonFinalRecordsBlockedTimeline(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, taskID: "task", gen: 1}
	e.recordNonFinal(st, st.taskID, spekHubArtifactStatus{Name: "20260925163042-myorg-repo1-57", DocumentStatus: "draft"})
	found := false
	for _, ev := range s.LifecycleTimeline().ByIssue(st.runKey) {
		if ev.Kind == timeline.KindBlocked && ev.Attrs[stageAttrReason] == spekHubNonFinalReason && ev.Attrs[stageAttrDocumentStatus] == "draft" {
			found = true
		}
	}
	if !found {
		t.Fatal("non-final exit did not record blocked timeline event")
	}
	if got := e.failures[e.executionKey(st)]; got != e.maxAttempts() {
		t.Fatalf("non-final exit failure budget = %d, want %d", got, e.maxAttempts())
	}
}

func TestSpekHubExecutorRecordsProgressTimeline(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, taskID: "task", gen: 2}
	e.recordStageProgress(st, "cli_launched", map[string]string{
		"backend": "copilot",
		"pid":     "1234",
	})
	found := false
	for _, ev := range s.LifecycleTimeline().ByIssue(st.runKey) {
		if ev.Kind == timeline.KindProgress && ev.Attrs["event"] == "cli_launched" && ev.Attrs["pid"] == "1234" && ev.Attrs[stageAttrGen] == "2" {
			found = true
		}
	}
	if !found {
		t.Fatal("executor progress event not recorded")
	}
}

func TestSpekInitAgentMapsCopilotToSupportedInitAgent(t *testing.T) {
	if got := spekInitAgent("copilot"); got != "codex" {
		t.Fatalf("spekInitAgent(copilot) = %q, want codex", got)
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

func TestSpekHubExecutorSweepKeepsRunWorktreeWhileUnclaimedLeaseExists(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	runKey := "myorg/repo1#57"
	key := spekRepo + "!" + runKey + ":" + StagePlan
	// A retry generation minted after expiry: active, but nobody owns it yet.
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "run-admit-x")] = &taskLease{identity: runAdmissionIdentity, taskID: "run-admit-x", repo: spekRepo, number: 57, key: key, stage: StagePlan, gen: 3, expiresAt: now.Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	work := spekHubRunWorktreePath(e.Identity, runKey)
	artifact := filepath.Join(work, ".spektacular", "plans", "p", "plan.md")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("run worktree with artifacts was swept while its lease was active but unclaimed: %v", err)
	}
}

func TestSpekHubExecutorPrepareWorkspaceRecoversSweptButRegisteredWorktree(t *testing.T) {
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
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StagePlan, repo: spekRepo, gen: 3}
	if _, err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("first prepareWorkspace: %v", err)
	}
	// Simulate the sweep deleting the directory without telling git.
	if err := os.RemoveAll(spekHubRunWorktreePath(e.Identity, st.runKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("prepareWorkspace after sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spekHubRunWorktreePath(e.Identity, st.runKey), ".git")); err != nil {
		t.Fatalf("worktree not recreated: %v", err)
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

func TestSpekGitEnvMarksWorkspaceSafeAndReplacesExistingOverride(t *testing.T) {
	env := spekGitEnv([]string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.foo", "GIT_CONFIG_VALUE_0=bar"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "core.foo") || strings.Count(joined, "GIT_CONFIG_COUNT=") != 1 {
		t.Fatalf("stale GIT_CONFIG override retained:\n%s", joined)
	}
}

func TestSpekHubExecutorRunStageSkipsSnapshotWhoseLeaseAdvanced(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	runKey := "myorg/repo1#57"
	// Lease has already moved on to plan gen 4 …
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "run-admit-x")] = &taskLease{identity: runAdmissionIdentity, taskID: "run-admit-x", repo: spekRepo, number: 57, key: spekRepo + "!" + runKey + ":" + StagePlan, stage: StagePlan, gen: 4, expiresAt: now.Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	launched := false
	e.Exec = func(context.Context, string, []string, string, ...string) ([]byte, error) {
		launched = true
		return []byte("ok"), nil
	}
	// … but Tick's snapshot still says spec gen 4.
	stale := spekHubStage{runKey: runKey, key: spekRepo + "!" + runKey + ":" + StageSpec, stage: StageSpec, identity: runAdmissionIdentity, taskID: "run-admit-x", repo: spekRepo, number: 57, gen: 4}
	e.runStage(context.Background(), stale, e.executionKey(stale))
	if launched {
		t.Fatal("executor relaunched a stage the lease had already advanced past")
	}
	if e.Status().LastError != "" {
		t.Fatalf("unexpected failure recorded: %s", e.Status().LastError)
	}
	current := spekHubStage{runKey: runKey, stage: StagePlan, gen: 4}
	if !e.stageStillActive(current) {
		t.Fatal("current stage reported inactive")
	}
}

func TestSpekHubExecutorDoesNotRelaunchWhenDocumentAlreadyFinal(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	runKey := "myorg/repo1#57"
	taskID := spekHubExecutorTaskPrefix + sanitizeReceiptSegment(runKey) + "-spec-1"
	hub.leaseMu.Lock()
	hub.leases[leaseKey(config.DefaultSpektacularHubExecutorIdentity, taskID)] = &taskLease{identity: config.DefaultSpektacularHubExecutorIdentity, taskID: taskID, repo: spekRepo, number: 57, key: spekRepo + "!" + runKey + ":" + StageSpec, stage: StagePlan, gen: 5, expiresAt: now.Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 2, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	worktree := spekHubRunWorktreePath(e.Identity, runKey)
	planDir := filepath.Join(worktree, ".spektacular", "plans", "20260925212730-"+sanitizeRunPromptPath(runKey))
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "plan.md"), []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentWorkspaceRoot, e.Identity, filepath.FromSlash(spekRepo), ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	agentLaunched := false
	e.Exec = func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		switch name {
		case "git":
			return []byte("ok"), nil
		case "spektacular":
			if len(args) >= 3 && args[1] == "status" {
				return []byte(`{"error":false,"kind":"plan","name":"myorg-repo1-57","artifact_id":"` + args[2] + `","document_status":"final"}`), nil
			}
			return []byte("ok"), nil
		}
		agentLaunched = true
		return []byte("agent done"), nil
	}
	stages, err := e.unclaimedStages()
	if err != nil || len(stages) != 1 {
		t.Fatalf("unclaimed stages = %d, %v", len(stages), err)
	}
	if err := e.executeStage(context.Background(), stages[0]); err != nil {
		t.Fatalf("executeStage: %v", err)
	}
	if agentLaunched {
		t.Fatal("agent CLI relaunched although the plan document is already final")
	}
	e.Tick(context.Background(), now)
	if e.Status().Running != 0 {
		t.Fatal("Tick relaunched a held generation")
	}
}
