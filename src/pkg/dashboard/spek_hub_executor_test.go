package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	e.Exec = func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		if name == "spektacular" && len(args) >= 3 && args[1] == "status" {
			return []byte(`{"error":false,"kind":"spec","name":"` + args[2] + `","artifact_id":"` + args[2] + `","document_status":"draft"}`), nil
		}
		return []byte("ok"), nil
	}
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
	for _, tc := range []struct{ name, binary, want string }{
		{"default", "", "spektacular"},
		{"custom", "/opt/custom/spek", "/opt/custom/spek"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _, _ := spekHub(t)
			remote := makeBareRepo(t)
			e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true, Binary: tc.binary}}, "copilot", "", nil, nil)
			e.CloneURL = func(string) string { return remote }
			var initArgs []string
			e.Exec = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
				if name == "git" {
					cmd := exec.CommandContext(ctx, name, args...)
					cmd.Dir = dir
					cmd.Env = env
					return cmd.CombinedOutput()
				}
				if name == tc.want {
					initArgs = append([]string{}, args...)
					return nil, os.MkdirAll(filepath.Join(dir, ".spektacular"), 0o755)
				}
				return nil, fmt.Errorf("unexpected executable %q", name)
			}
			st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, repo: spekRepo, gen: 1}
			if err := e.prepareWorkspace(context.Background(), st); err != nil {
				t.Fatalf("prepareWorkspace: %v", err)
			}
			if _, err := os.Stat(filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(spekRepo), ".git")); err != nil {
				t.Fatalf("shared clone missing: %v", err)
			}
			if _, err := os.Stat(filepath.Join(spekHubRunWorktreePath(e.Identity, st.runKey), ".spektacular")); err != nil {
				t.Fatalf("worktree/project missing: %v", err)
			}
			if got := strings.Join(initArgs, " "); got != "init codex --name repo1" {
				t.Fatalf("init args = %q", got)
			}
		})
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

func TestSpekHubStagePromptSpecUsesIDPrefixedSpecName(t *testing.T) {
	for _, source := range []string{"github", "linear"} {
		p := SpekHubStagePromptWithContext(StageSpec, "myorg/repo1", 42, "myorg/repo1#42", "", "myorg-repo1-42", worksource.WorkItemContext{SourceType: source})
		for _, want := range []string{`spektacular spec new --data '{"name":"myorg-repo1-42"}'`, "`spektacular spec status <spec-id>`", "`<id>-myorg-repo1-42` or `<id>_myorg-repo1-42`", "not `myorg-repo1-42`"} {
			if !strings.Contains(p, want) {
				t.Fatalf("%s prompt missing %q:\n%s", source, want, p)
			}
		}
		if strings.Contains(p, "spec status myorg-repo1-42") {
			t.Fatalf("%s prompt polls the bare slug:\n%s", source, p)
		}
	}
}

func TestSpekHubExecutorPlanPromptNamesPlanAfterResolvedSpec(t *testing.T) {
	e := &SpekHubExecutor{}
	st := spekHubStage{stage: StagePlan, repo: "myorg/repo1", number: 42, runKey: "myorg/repo1#42"}
	for _, specID := range []string{"20261002143509-myorg-repo1-42", "000001_myorg-repo1-42"} {
		worktree := t.TempDir()
		specPath := filepath.Join(worktree, ".spektacular", "specs", specID+".md")
		if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(specPath, []byte("spec"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := e.stagePrompt(st, worktree, "myorg-repo1-42")
		for _, want := range []string{`spektacular plan new --data '{"name":"` + specID + `"}'`, "`spektacular plan status " + specID + "`"} {
			if !strings.Contains(p, want) {
				t.Fatalf("prompt missing %q:\n%s", want, p)
			}
		}
		if strings.Contains(p, `"name":"myorg-repo1-42"`) || strings.Contains(p, `"spec":`) {
			t.Fatalf("plan prompt still names the bare slug:\n%s", p)
		}
	}
	p := e.stagePrompt(st, t.TempDir(), "myorg-repo1-42")
	if !strings.Contains(p, `plan new --data '{"name":"myorg-repo1-42"}'`) || !strings.Contains(p, "full ID-prefixed name") {
		t.Fatalf("unresolved plan prompt = %s", p)
	}
}

func TestResolveSpekArtifactFromFilesMatchesCounterIDs(t *testing.T) {
	worktree := t.TempDir()
	for _, rel := range []string{"specs/000001_myorg-repo1-42.md", "plans/000002_myorg-repo1-42/plan.md", "specs/000003_other-42.md"} {
		path := filepath.Join(worktree, ".spektacular", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("doc"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolveSpekArtifactFromFiles(worktree, StageSpec, "myorg-repo1-42"); got != "000001_myorg-repo1-42" {
		t.Fatalf("spec = %q", got)
	}
	if got := resolveSpekArtifactFromFiles(worktree, StagePlan, "myorg-repo1-42"); got != "000002_myorg-repo1-42" {
		t.Fatalf("plan = %q", got)
	}
}

func TestSpekHubStagePromptGitHubDoesNotRequireGhCLI(t *testing.T) {
	p := SpekHubStagePromptWithContext(StageSpec, "myorg/repo1", 57, "myorg/repo1#57", "Do thing", "myorg-repo1-57", worksource.WorkItemContext{
		SourceType: "github",
		Body:       "Captured issue body",
	})
	for _, want := range []string{"Issue description:\nCaptured issue body", "https://api.github.com/repos/myorg/repo1/issues/57", "myorg/repo1#57"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "gh issue view") {
		t.Fatalf("hub prompt must not ask for gh issue view; the executor env has no gh token:\n%s", p)
	}
	p = SpekHubStagePrompt(StagePlan, "myorg/repo1", 57, "myorg/repo1#57", "Do thing", "myorg-repo1-57")
	if strings.Contains(p, "gh issue view") || strings.Contains(p, "Issue description:") || !strings.Contains(p, "https://api.github.com/repos/myorg/repo1/issues/57") {
		t.Fatalf("prompt without a captured body:\n%s", p)
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
	if err := os.MkdirAll(filepath.Join(currentAgentWorkspaceRoot(), config.DefaultSpektacularHubExecutorIdentity, filepath.FromSlash(spekRepo), ".git"), 0o755); err != nil {
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
	waitSpekHubExecutorIdle(t, e)
}

func waitSpekHubExecutorIdle(t *testing.T, e *SpekHubExecutor) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if e.Status().Running == 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("executor still running: %+v", e.Status())
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
	if !e.held[e.executionKey(st)] {
		t.Fatal("non-final exit did not hold the generation")
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

func TestSpekHubExecutorCapturesStageTranscriptAndDocument(t *testing.T) {
	_, s, _, _ := spekHub(t)
	oldReceipts := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = oldReceipts })
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "gpt-test", nil, nil)
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, taskID: "task", gen: 2}
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, spekHubPromptRelPath), []byte("author the spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(worktree, ".spektacular", "specs", "20260925163042-myorg-repo1-57.md")
	if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "# Spec\n\nQuestion: What should change?\nAnswer: Add readable stage details.\n"
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	status := spekHubArtifactStatus{Name: "myorg-repo1-57", ArtifactID: "20260925163042-myorg-repo1-57", DocumentStatus: "final", CurrentStep: "finished", CompletedSteps: []string{"clarify"}, Raw: map[string]any{"instruction": "Explain the desired behavior"}}
	started := time.Now().Add(-time.Minute)
	history := []RunDetailStageStatus{{At: started.Format(time.RFC3339Nano), Step: "clarify", Instruction: "Explain the desired behavior", DocumentStatus: "draft", Raw: status.Raw}}
	if err := e.captureCompletedStage(st, worktree, "myorg-repo1-57", status, []byte("agent answered the interview"), started, history, runReceiptsDir, "session"); err != nil {
		t.Fatalf("captureCompletedStage: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(runReceiptsDir, sanitizeReceiptSegment(st.runKey), spekStageTranscriptFile(StageSpec, 2)))
	if err != nil {
		t.Fatalf("transcript sidecar missing: %v", err)
	}
	var cap RunDetailStageCapture
	if err := json.Unmarshal(raw, &cap); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	if cap.SchemaVersion != spekStageTranscriptSchema || cap.Prompt == nil || cap.AgentTranscript == nil || cap.AgentStdoutStderr == nil || len(cap.Documents) != 1 || len(cap.Interview) != 1 {
		t.Fatalf("capture incomplete: %+v", cap)
	}
	if cap.Capture != "session" || cap.Documents[0].Content == "" || cap.Documents[0].Markdown == "" || cap.StatusHistory[0].Raw["instruction"] != "Explain the desired behavior" {
		t.Fatalf("capture omitted mode, document content, or raw status: %+v", cap)
	}
	if _, err := os.Stat(filepath.Join(runReceiptsDir, sanitizeReceiptSegment(st.runKey), spekStageDocumentFile(StageSpec, 2))); err != nil {
		t.Fatalf("document sidecar missing: %v", err)
	}
}

func TestSpekHubExecutorAlreadyFinalDoesNotOverwriteSessionCapture(t *testing.T) {
	_, s, _, _ := spekHub(t)
	oldReceipts := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = oldReceipts })
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, taskID: "task", gen: 1}
	worktree := t.TempDir()
	if err := writeSpekStageCapture(st.runKey, st.stage, st.gen, RunDetailStageCapture{
		Capture:         "session",
		AgentTranscript: &RunDetailTextBlock{Text: "real session output"},
		Documents:       []RunDetailStageDocument{{Path: "artifact.md", Markdown: "real doc", Content: "real doc"}},
		StatusHistory:   []RunDetailStageStatus{{Step: "drafting", DocumentStatus: "draft"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.captureCompletedStage(st, worktree, "artifact", spekHubArtifactStatus{Name: "artifact", DocumentStatus: "final", CurrentStep: "finished"}, nil, time.Now(), nil, runReceiptsDir, "already_final"); err != nil {
		t.Fatalf("already-final capture: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(runReceiptsDir, sanitizeReceiptSegment(st.runKey), spekStageTranscriptFile(st.stage, st.gen)))
	if err != nil {
		t.Fatal(err)
	}
	var cap RunDetailStageCapture
	if err := json.Unmarshal(raw, &cap); err != nil {
		t.Fatal(err)
	}
	if cap.Capture != "session" || cap.AgentTranscript == nil || cap.AgentTranscript.Text != "real session output" || cap.Documents[0].Content != "real doc" {
		t.Fatalf("already-final capture overwrote real session: %+v", cap)
	}
	if len(cap.StatusHistory) != 2 || cap.StatusHistory[1].DocumentStatus != "final" {
		t.Fatalf("already-final status was not merged: %+v", cap.StatusHistory)
	}
}

func TestStageStatusCaptureKeepsInstructionChanges(t *testing.T) {
	history := appendDistinctStageStatus(nil,
		RunDetailStageStatus{Step: "clarify", Instruction: "Question one?", DocumentStatus: "draft", Raw: map[string]any{"instruction": "Question one?"}},
		RunDetailStageStatus{Step: "clarify", Instruction: "Question two?", DocumentStatus: "draft", Raw: map[string]any{"instruction": "Question two?"}},
	)
	if len(history) != 2 {
		t.Fatalf("instruction-only status change was deduped: %+v", history)
	}
	interview := interviewFromStatusHistory(history, []RunDetailStageDocument{{Content: "final answer"}}, time.Now())
	if len(interview) != 2 || interview[0].Question != "Question one?" || interview[1].Question != "Question two?" {
		t.Fatalf("interview did not preserve status instructions: %+v", interview)
	}
}

func TestSpekInitAgentMapsCopilotToSupportedInitAgent(t *testing.T) {
	if got := spekInitAgent("copilot"); got != "codex" {
		t.Fatalf("spekInitAgent(copilot) = %q, want codex", got)
	}
}

func TestSpekInitAgentMatchesLaunchedBinary(t *testing.T) {
	cases := map[string]string{
		"claude":        "claude",
		" Claude ":      "claude",
		"litellm":       "claude",
		"bob":           "bob",
		"codex":         "codex",
		"aider":         "codex",
		"opencode":      "codex",
		"gemini":        "codex",
		"not-a-backend": "claude",
	}
	for backend, want := range cases {
		if got := spekInitAgent(backend); got != want {
			t.Errorf("spekInitAgent(%q) = %q, want %q", backend, got, want)
		}
	}
}

func TestSpekHubExecutorBobAPIKeyReadsGovernorConfig(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "bob", "", nil, nil)
	keyFile := filepath.Join(t.TempDir(), "bob-key")
	if err := os.WriteFile(keyFile, []byte("bob-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.deps.Config == nil {
		s.deps.Config = &config.Config{}
	}
	s.deps.Config.Governor.Bob.APIKeyFile = keyFile
	if got := e.bobAPIKey(); got != "bob-secret" {
		t.Fatalf("bobAPIKey() = %q, want bob-secret", got)
	}
	env, err := e.executorEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, config.BobAPIKeyEnvVar+"=bob-secret") {
		t.Fatalf("bob key not injected: %v", env)
	}
	if got := (&SpekHubExecutor{}).bobAPIKey(); got != "" {
		t.Fatalf("bobAPIKey() without server = %q, want empty", got)
	}
}

func TestSpekHubExecutorFailureRecordsAuditTimelineAndHoldsGeneration(t *testing.T) {
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
	if !e.held[key] {
		t.Fatal("failure did not hold the generation")
	}
}

func TestSpekHubExecutorEnvUsesAllowlistedValuesWithoutCloneToken(t *testing.T) {
	_, s, _, _ := spekHub(t)
	t.Setenv("GITHUB_TOKEN", "old")
	t.Setenv("GH_TOKEN", "old")
	t.Setenv("HIVE_HUB_TOKEN", "hub-token")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "dashboard-token")
	t.Setenv("UNLISTED_VALUE", "drop-me")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("HTTPS_PROXY", "http://proxy.example")
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	env, err := e.executorEnv()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		byKey[k] = v
	}
	if byKey["HOME"] != filepath.Join(currentAgentWorkspaceRoot(), e.Identity, "home") {
		t.Fatalf("HOME = %q", byKey["HOME"])
	}
	if byKey["PATH"] != "/usr/bin" || byKey["LANG"] != "C.UTF-8" || byKey["LC_ALL"] != "C.UTF-8" || byKey["HTTPS_PROXY"] != "http://proxy.example" {
		t.Fatalf("expected allowlisted process values, got PATH=%q LANG=%q LC_ALL=%q HTTPS_PROXY=%q", byKey["PATH"], byKey["LANG"], byKey["LC_ALL"], byKey["HTTPS_PROXY"])
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "HIVE_HUB_TOKEN", "HIVE_DASHBOARD_TOKEN", "UNLISTED_VALUE"} {
		if _, ok := byKey[key]; ok {
			t.Fatalf("%s reached child env", key)
		}
	}
}

func TestSpekHubExecutorOutputIsScrubbedForStatusLogAndDetail(t *testing.T) {
	_, s, _, _ := spekHub(t)
	key := "myorg/repo1#57"
	st := spekHubStage{runKey: key, stage: StageSpec, taskID: "task", repo: spekRepo, number: 57, gen: 2}
	if err := s.contributeHub.recordLeaseForKeyStage(config.DefaultSpektacularHubExecutorIdentity, st.taskID, spekRepo, 57, spekRepo+"!"+key+":"+StageSpec, "trusted", StageSpec, st.gen, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	worktree := spekHubRunWorktreePath(e.Identity, key)
	if err := os.MkdirAll(filepath.Join(worktree, ".hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	ghp := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd"
	pat := "github_pat_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	apiKey := "sk-live-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	bearer := "Bearer abcdefghijklmnop1234567890"
	raw := "auth failed " + ghp + " " + pat + " " + apiKey + " Authorization: " + bearer + "\n"
	e.Exec = func(context.Context, string, []string, string, ...string) ([]byte, error) {
		return []byte(raw), errors.New("exit status 1")
	}
	out, _, err := e.runStageCommand(context.Background(), worktree, nil, st, []string{"agent"})
	if err == nil {
		t.Fatal("runStageCommand: expected an error")
	}
	e.recordFailure(st, e.executionKey(st), fmt.Errorf("agent CLI failed: %w: %s", err, string(out)))
	status := e.Status()
	if status.LastError == "" {
		t.Fatal("last error was not recorded")
	}
	captureStatus := spekHubArtifactStatus{Name: "myorg-repo1-57", ArtifactID: "myorg-repo1-57", DocumentStatus: "final", CurrentStep: "finished"}
	if err := e.captureCompletedStage(st, worktree, "myorg-repo1-57", captureStatus, out, time.Now(), nil, runReceiptsDir, "session"); err != nil {
		t.Fatalf("captureCompletedStage: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+url.PathEscape(key)+"/log?stage=spec&gen=2", nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("log response = %d body=%s", rec.Code, rec.Body.String())
	}
	detail, err := s.buildRunDetail(httptest.NewRequest(http.MethodGet, "/api/runs/"+url.PathEscape(key)+"/detail", nil), key)
	if err != nil {
		t.Fatalf("buildRunDetail: %v", err)
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for label, text := range map[string]string{
		"last_error": status.LastError,
		"run_log":    rec.Body.String(),
		"detail":     string(detailJSON),
	} {
		for _, rawValue := range []string{ghp, pat, apiKey, bearer} {
			if strings.Contains(text, rawValue) {
				t.Fatalf("%s retained %q in %q", label, rawValue, text)
			}
		}
	}
}

func TestSpekHubScrubWriterStreamsCompleteLinesAndFlushesPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stage.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := newSpekHubScrubWriter(file)

	token := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd"
	first := "live " + token[:18]
	if _, err := writer.Write([]byte(first)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Fatalf("partial line was written before newline: data=%q err=%v", string(data), err)
	}
	if _, err := writer.Write([]byte(token[18:] + "\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || !strings.Contains(string(data), "live <redacted:github-token>\n") {
		t.Fatalf("complete split-token line was not scrubbed live: %q", string(data))
	}

	partial := "tail sk-live-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if _, err := writer.Write([]byte(partial)); err != nil {
		t.Fatalf("partial write: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tail ") {
		t.Fatalf("trailing partial line was written before close: %q", string(data))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), partial) || !strings.Contains(string(data), "tail <redacted:api-key>") {
		t.Fatalf("trailing partial line was not scrubbed on close: %q", string(data))
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

func TestSpekProjectName(t *testing.T) {
	for repo, want := range map[string]string{
		"myorg/repo1":               "repo1",
		"myorg/hive.github.io":      "hive-github-io",
		"myorg/.github":             "github",
		"myorg/Console.UI":          "console-ui",
		"myorg/my_repo":             "my_repo",
		"myorg/_x":                  "x",
		"myorg/...":                 "project",
		"kubestellar/Kube Stellar!": "kube-stellar-",
	} {
		if got := spekProjectName(repo); got != want {
			t.Errorf("spekProjectName(%q) = %q, want %q", repo, got, want)
		}
	}
}

// A `.spektacular/` committed to the target repo may come from another
// Spektacular version; every verb then fails with upgrade_required until
// `migrate` runs, so prepareWorkspace migrates instead of skipping init.
func TestSpekHubExecutorPrepareWorkspaceMigratesExistingProject(t *testing.T) {
	for _, tc := range []struct {
		name       string
		migrateErr error
	}{
		{"migrated", nil},
		{"migrate fails", errors.New("exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _, _ := spekHub(t)
			remote := makeBareRepo(t)
			e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
			e.CloneURL = func(string) string { return remote }
			st := spekHubStage{runKey: "myorg/hive.github.io#57", stage: StageSpec, repo: "myorg/hive.github.io", gen: 1}
			worktree := spekHubRunWorktreePath(e.Identity, st.runKey)
			if err := os.MkdirAll(filepath.Join(worktree, ".spektacular"), 0o755); err != nil {
				t.Fatal(err)
			}
			var spekCalls []string
			e.Exec = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
				if name == "git" {
					cmd := exec.CommandContext(ctx, name, args...)
					cmd.Dir = dir
					cmd.Env = env
					return cmd.CombinedOutput()
				}
				if name != "spektacular" || dir != worktree {
					return nil, fmt.Errorf("unexpected invocation %q in %q", name, dir)
				}
				spekCalls = append(spekCalls, strings.Join(args, " "))
				return []byte(`{"error":true,"code":"internal_error","message":"boom"}`), tc.migrateErr
			}
			if err := e.prepareWorkspace(context.Background(), st); err != nil {
				t.Fatalf("prepareWorkspace: %v", err)
			}
			if strings.Join(spekCalls, ";") != "migrate" {
				t.Fatalf("spektacular calls = %v, want only migrate", spekCalls)
			}
		})
	}
}

func TestSpekHubExecutorPrepareWorkspaceInitSanitizesProjectName(t *testing.T) {
	_, s, _, _ := spekHub(t)
	remote := makeBareRepo(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "claude", "", nil, nil)
	e.CloneURL = func(string) string { return remote }
	var spekCalls []string
	e.Exec = func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if name == "git" {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Dir = dir
			cmd.Env = env
			return cmd.CombinedOutput()
		}
		spekCalls = append(spekCalls, strings.Join(args, " "))
		return nil, os.MkdirAll(filepath.Join(dir, ".spektacular"), 0o755)
	}
	st := spekHubStage{runKey: "myorg/Console.UI#57", stage: StageSpec, repo: "myorg/Console.UI", gen: 1}
	if err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	if strings.Join(spekCalls, ";") != "init claude --name console-ui" {
		t.Fatalf("spektacular calls = %v", spekCalls)
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
	if err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("first prepareWorkspace: %v", err)
	}
	// Simulate the sweep deleting the directory without telling git.
	if err := os.RemoveAll(spekHubRunWorktreePath(e.Identity, st.runKey)); err != nil {
		t.Fatal(err)
	}
	if err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("prepareWorkspace after sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spekHubRunWorktreePath(e.Identity, st.runKey), ".git")); err != nil {
		t.Fatalf("worktree not recreated: %v", err)
	}
}

func spekHubRealGitExec(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
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

func TestSpekHubExecutorPrepareWorkspaceRefreshesExistingRunWorktree(t *testing.T) {
	_, s, _, _ := spekHub(t)
	remote := makeBareRepo(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	e.CloneURL = func(string) string { return remote }
	e.Exec = spekHubRealGitExec
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, repo: spekRepo, gen: 1}
	if err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("spec prepareWorkspace: %v", err)
	}
	worktree := spekHubRunWorktreePath(e.Identity, st.runKey)
	artifact := filepath.Join(worktree, ".spektacular", "specs", "myorg-repo1-57.md")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("# spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The default branch advances while the spec waits for approval.
	work := filepath.Join(t.TempDir(), "push")
	for _, args := range [][]string{
		{"clone", remote, work},
		{"-C", work, "-c", "user.email=hive@example.com", "-c", "user.name=Hive", "commit", "--allow-empty", "-m", "advance"},
		{"-C", work, "push", "origin", "HEAD"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	st.stage, st.gen = StagePlan, 2
	if err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("plan prepareWorkspace: %v", err)
	}
	got, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("run worktree HEAD = %s, want the fetched %s", got, want)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("refresh lost the untracked spec artifact: %v", err)
	}
}

func TestSpekHubExecutorSharedCloneLockSerialisesLaunchAndSweep(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	repoDir := filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(spekRepo))
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	var gitCalls []string
	e.Exec = func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		if name == "git" {
			gitCalls = append(gitCalls, strings.Join(args, " "))
		}
		return []byte("ok"), nil
	}
	lock, err := acquireSpekHubFence(spekHubRepoLockPath(repoDir))
	if err != nil {
		t.Fatalf("hold shared clone lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*spekHubRepoLockPoll)
	defer cancel()
	st := spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, repo: spekRepo, gen: 1}
	if err := e.prepareWorkspace(ctx, st); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("prepareWorkspace with the clone locked = %v, want it to wait for the lock", err)
	}
	stale := runStageWorktreePath(e.Identity, "myorg/repo1#58", StageSpec, 1)
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gitCalls) != 0 {
		t.Fatalf("git ran on a locked shared clone: %v", gitCalls)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("sweep removed a worktree while the shared clone was locked: %v", err)
	}
	lock.Close()
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gitCalls, "worktree remove --force "+stale) {
		t.Fatalf("git calls after unlock = %v, want worktree remove", gitCalls)
	}
	if err := e.prepareWorkspace(context.Background(), st); err != nil {
		t.Fatalf("prepareWorkspace after unlock: %v", err)
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
	if err := os.MkdirAll(filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(spekRepo), ".git"), 0o755); err != nil {
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

func TestSpekHubExecutorSkipsHeldSpecCheckpoint(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekDesignEpic(t, store)
	taskID := spekHubExecutorTaskPrefix + sanitizeReceiptSegment(spekRunKey) + "-spec-1"
	hub.leaseMu.Lock()
	hub.leases[leaseKey(config.DefaultSpektacularHubExecutorIdentity, taskID)] = &taskLease{identity: config.DefaultSpektacularHubExecutorIdentity, taskID: taskID, repo: spekRepo, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: now.Add(leaseTTL)}
	hub.leaseMu.Unlock()
	if _, err := writeStageReceipt(spekRunKey, StageSpec, 1, []byte(`{}`)); err != nil {
		t.Fatalf("write held spec receipt: %v", err)
	}
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 2, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	stages, err := e.unclaimedStages()
	if err != nil {
		t.Fatalf("unclaimedStages: %v", err)
	}
	if len(stages) != 0 {
		t.Fatalf("held spec checkpoint offered to hub executor: %+v", stages)
	}
}

func TestSpekHubExecutorStatusUsesConfiguredBinary(t *testing.T) {
	for _, binary := range []string{"", "  ", "/opt/custom/spek", "spek-custom"} {
		for _, kind := range []string{"spec", "plan"} {
			t.Run(binary+"/"+kind, func(t *testing.T) {
				e := &SpekHubExecutor{Config: config.RunsConfig{Spektacular: config.SpektacularConfig{Binary: binary}}}
				want := strings.TrimSpace(binary)
				if want == "" {
					want = "spektacular"
				}
				e.Exec = func(_ context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
					if name != want || strings.Join(args, " ") != kind+" status my-artifact" || dir != "worktree" || strings.Join(env, " ") != "TEST=1" {
						t.Fatalf("unexpected invocation: %s %v in %s with %v", name, args, dir, env)
					}
					return []byte(`{"document_status":"final","name":"my-artifact"}`), nil
				}
				status, err := e.spekStatus(context.Background(), "worktree", []string{"TEST=1"}, kind, "my-artifact")
				if err != nil || status.DocumentStatus != "final" {
					t.Fatalf("status = %+v, err = %v", status, err)
				}
			})
		}
	}
}

func TestSpekHubExecutorPromptUsesConfiguredBinary(t *testing.T) {
	for _, tc := range []struct{ binary, command string }{
		{"", "spektacular"},
		{"/opt/custom/spek", "'/opt/custom/spek'"},
		{"/opt/custom bin/spek's", "'/opt/custom bin/spek'\"'\"'s'"},
		{"spek-custom", "'spek-custom'"},
	} {
		for _, stage := range []string{StageSpec, StagePlan} {
			for _, source := range []string{"github", "linear"} {
				t.Run(tc.binary+"/"+stage+"/"+source, func(t *testing.T) {
					e := &SpekHubExecutor{Config: config.RunsConfig{Spektacular: config.SpektacularConfig{Binary: tc.binary}}}
					st := spekHubStage{stage: stage, repo: "org/repo", number: 57, runKey: "org/repo#57", workItem: worksource.WorkItemContext{SourceType: source}}
					prompt := e.stagePrompt(st, t.TempDir(), "my-artifact")
					status := "status my-artifact"
					if stage == StageSpec {
						status = "status <spec-id>"
					}
					for _, command := range []string{"new --data", status, "file ..."} {
						if !strings.Contains(prompt, "`"+tc.command+" "+stage+" "+command) {
							t.Errorf("prompt missing configured command %s: %s", command, prompt)
						}
					}
					if tc.binary != "" && strings.Contains(prompt, "`spektacular") {
						t.Errorf("prompt still uses bare spektacular: %s", prompt)
					}
				})
			}
		}
	}
}

func TestSpekHubExecutorEnvForwardsProxyCAButNotSSLCertFile(t *testing.T) {
	_, s, _, _ := spekHub(t)
	t.Setenv("NODE_EXTRA_CA_CERTS", "/etc/hive/proxy-ca.pem")
	t.Setenv("GIT_SSL_CAINFO", "/etc/hive/proxy-ca.pem")
	t.Setenv("SSL_CERT_FILE", "/etc/hive/combined.pem")
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	env, err := e.executorEnv()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		byKey[k] = v
	}
	if byKey["NODE_EXTRA_CA_CERTS"] != "/etc/hive/proxy-ca.pem" || byKey["GIT_SSL_CAINFO"] != "/etc/hive/proxy-ca.pem" {
		t.Fatalf("proxy CA not forwarded: NODE_EXTRA_CA_CERTS=%q GIT_SSL_CAINFO=%q", byKey["NODE_EXTRA_CA_CERTS"], byKey["GIT_SSL_CAINFO"])
	}
	if _, ok := byKey["SSL_CERT_FILE"]; ok {
		t.Fatal("SSL_CERT_FILE reached child env")
	}
}

func TestSpekHubExecutorSweepRemovesStaleCloneCredentialFiles(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	ownerDir := filepath.Join(currentAgentWorkspaceRoot(), e.Identity, "myorg")
	if err := os.MkdirAll(ownerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ownerDir, SpekHubCloneCredentialFilePrefix+"myorg-repo1-1")
	fresh := filepath.Join(ownerDir, SpekHubCloneCredentialFilePrefix+"myorg-repo1-2")
	other := filepath.Join(ownerDir, "notes.txt")
	for _, path := range []string{stale, fresh, other} {
		if err := os.WriteFile(path, []byte("https://x-access-token:tok@github.com\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-spekHubCloneCredentialMaxAge - time.Minute)
	for _, path := range []string{stale, other} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale credential file still exists: %v", err)
	}
	for _, path := range []string{fresh, other} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed: %v", path, err)
		}
	}
}

func TestWriteSpekHubPromptDoesNotFollowPlantedSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	worktree := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(worktree, spekHubPromptRelPath)
	if err := os.Symlink(target, promptPath); err != nil {
		t.Fatal(err)
	}
	if err := writeSpekHubPrompt(worktree, "prompt"); err != nil {
		t.Fatalf("writeSpekHubPrompt: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Fatalf("symlink target overwritten: %q", got)
	}
	info, err := os.Lstat(promptPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("prompt is not a fresh regular file: %v %v", info, err)
	}
	if got, _ := os.ReadFile(promptPath); string(got) != "prompt" {
		t.Fatalf("prompt = %q", got)
	}

	linkedDir := t.TempDir()
	worktree2 := t.TempDir()
	if err := os.Symlink(linkedDir, filepath.Join(worktree2, ".hive")); err != nil {
		t.Fatal(err)
	}
	if err := writeSpekHubPrompt(worktree2, "prompt"); err == nil {
		t.Fatal("expected a symlinked .hive directory to be refused")
	}
	if _, err := os.Stat(filepath.Join(linkedDir, filepath.Base(spekHubPromptRelPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prompt written through symlinked .hive: %v", err)
	}
}

func TestCollectSpekArtifactFilesSkipsSymlinksAndScrubs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	worktree := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(worktree, ".spektacular", "specs", "myorg-repo1-57")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ghp := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd"
	if err := os.WriteFile(filepath.Join(artifactDir, "spec.md"), []byte("# Spec\ntoken "+ghp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(artifactDir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(artifactDir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	docs, files, _, _ := collectSpekArtifactFiles(worktree, StageSpec, "myorg-repo1-57")
	if len(files) != 1 || len(docs) != 1 || !strings.HasSuffix(files[0].Path, "spec.md") {
		t.Fatalf("expected only the regular spec file, got files=%+v", files)
	}
	for _, text := range []string{files[0].Content, docs[0].Markdown} {
		if strings.Contains(text, "outside-secret") || strings.Contains(text, ghp) {
			t.Fatalf("capture leaked symlinked or unscrubbed content: %q", text)
		}
	}

	if _, err := readSpekWorktreeFile(worktree, filepath.Join(artifactDir, "linked-dir", "secret.md")); err == nil {
		t.Fatal("expected a read through a symlinked directory to be refused")
	}
}

func TestSpekHubCappedWriterDropsOutputPastLimit(t *testing.T) {
	var sb strings.Builder
	w := &spekHubCappedWriter{dst: &sb, limit: 10}
	for _, chunk := range []string{"12345", "6789012345", "more"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want full length", chunk, n, err)
		}
	}
	if want := "1234567890" + spekHubLogTruncatedMarker; sb.String() != want {
		t.Fatalf("log = %q, want %q", sb.String(), want)
	}
}
