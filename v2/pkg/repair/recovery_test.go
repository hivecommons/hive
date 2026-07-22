package repair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestRepairLocalGitFilterHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_REPAIR_FILTER_HELPER") != "1" {
		return
	}
	leaked := false
	for _, pair := range os.Environ() {
		name, value, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_CONFIG_VALUE_") && strings.Contains(strings.ToUpper(value), "AUTHORIZATION: BASIC ") {
			leaked = true
		}
	}
	result := "credentialless\n"
	if leaked {
		result = "transport-authority-leaked\n"
	}
	if err := os.WriteFile(os.Getenv("HIVE_TEST_FILTER_SENTINEL"), []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

type healthFailureProvider struct {
	fail bool
	runs int
}

type runFailureProvider struct {
	launched bool
	runs     int
}

func (p *runFailureProvider) Name() string                 { return "run-failure-model" }
func (p *runFailureProvider) Health(context.Context) error { return nil }
func (p *runFailureProvider) Run(context.Context, string, string) (ProviderResult, error) {
	p.runs++
	return ProviderResult{Summary: "provider returned no conclusive result"}, &ProviderRunError{Launched: p.launched, Cause: errors.New("provider exit status 9")}
}

func (p *healthFailureProvider) Name() string { return "health-failure-model" }
func (p *healthFailureProvider) Health(context.Context) error {
	if p.fail {
		return errors.New("provider transport unavailable")
	}
	return nil
}
func (p *healthFailureProvider) Run(_ context.Context, worktree, _ string) (ProviderResult, error) {
	p.runs++
	return ProviderResult{Summary: "fixed after infrastructure recovery"}, os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("fixed\n"), 0o600)
}

func TestInfrastructureFailureDoesNotCountModelAttempt(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	provider := &healthFailureProvider{fail: true}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:infra", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}

	if _, err := worker.Run(context.Background(), finding); !IsResumableFailureError(err) {
		t.Fatalf("infrastructure failure was not durably paused: %v", err)
	}
	failed, ok := state.Get(finding.RepositoryFingerprint)
	if !ok || failed.Stage != StageFailed || failed.LastFailureClass != FailureInfrastructure || failed.AttemptCounted || failed.Attempt != 1 || failed.CountedModelAttempts() != 0 || lifecycle.starts != 0 || provider.runs != 0 {
		t.Fatalf("infrastructure failure consumed a model attempt: attempt=%+v lifecycle=%+v runs=%d", failed, lifecycle, provider.runs)
	}

	resumed, err := state.ResumeRetry(RetryRequest{
		RepositoryFingerprint: finding.RepositoryFingerprint, ExpectedAttempt: 1,
		ExpectedFailureClass: FailureInfrastructure, ExpectedFailureID: failed.LastFailureID,
		Actor: "test-operator", Reason: "provider transport restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stage != StagePrepared || resumed.Attempt != 1 || resumed.AttemptCounted {
		t.Fatalf("infrastructure retry did not resume the same uncounted ordinal: %+v", resumed)
	}
	provider.fail = false
	if _, err := worker.Run(context.Background(), finding); err != nil {
		t.Fatal(err)
	}
	completed, _ := state.Get(finding.RepositoryFingerprint)
	if completed.Attempt != 1 || !completed.AttemptCounted || completed.CountedModelAttempts() != 1 || lifecycle.starts != 1 || provider.runs != 1 {
		t.Fatalf("recovered model attempt was not counted exactly once: attempt=%+v lifecycle=%+v runs=%d", completed, lifecycle, provider.runs)
	}
}

func TestPostLaunchProviderFailureCountsExactlyOneAmbiguousAttempt(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &runFailureProvider{launched: true}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:post-launch", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); !IsRetryableAttemptError(err) || !strings.Contains(err.Error(), "ended ambiguously after launch") {
		t.Fatalf("post-launch provider failure was not charged as an ambiguous model attempt: %v", err)
	}
	saved, _ := state.Get(finding.RepositoryFingerprint)
	if provider.runs != 1 || saved.Stage != StageNoChange || !saved.AttemptCounted || saved.CountedModelAttempts() != 1 || lifecycle.starts != 1 {
		t.Fatalf("post-launch failure was not counted exactly once: attempt=%+v runs=%d starts=%d", saved, provider.runs, lifecycle.starts)
	}
}

func TestProviderLaunchFailureResumesSameUncountedAttempt(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &runFailureProvider{launched: false}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:pre-launch", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); !IsResumableFailureError(err) {
		t.Fatalf("provider launch failure was not paused as infrastructure: %v", err)
	}
	saved, _ := state.Get(finding.RepositoryFingerprint)
	if provider.runs != 1 || saved.Stage != StageFailed || saved.ResumeStage != StagePrepared || saved.AttemptCounted || saved.CountedModelAttempts() != 0 || lifecycle.starts != 0 {
		t.Fatalf("provider launch failure consumed a model attempt: attempt=%+v runs=%d starts=%d", saved, provider.runs, lifecycle.starts)
	}
}

func TestValidationLaunchFailureResumesCountedPatchCheckpoint(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &healthFailureProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"},
			ValidationCommands: []Command{{Name: "hive-validation-command-that-does-not-exist"}},
			ModelTimeout:       time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:validation-infra", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); !IsResumableFailureError(err) {
		t.Fatalf("validation launch failure was not durably paused: %v", err)
	}
	saved, _ := state.Get(finding.RepositoryFingerprint)
	if saved.Stage != StageFailed || saved.ResumeStage != StageModelComplete || saved.LastFailureClass != FailurePatchEngine ||
		!saved.AttemptCounted || saved.CountedModelAttempts() != 1 || lifecycle.starts != 1 || provider.runs != 1 {
		t.Fatalf("validation infrastructure failure spent the wrong model budget: attempt=%+v starts=%d runs=%d", saved, lifecycle.starts, provider.runs)
	}
}

func TestRepairCommandFailureClassificationSeparatesInfrastructureFromTestFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := classifyRepairCommandFailure(ctx, context.DeadlineExceeded, "", errors.New("validation timed out")); !isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("validation timeout was not infrastructure: %v", err)
	}
	if err := classifyRepairCommandFailure(context.Background(), errors.New("Access is denied"), "", errors.New("validation permission failure")); !isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("validation permission failure was not infrastructure: %v", err)
	}
	if err := classifyRepairCommandFailure(context.Background(), errors.New("exit status 1"), "assertion failed", errors.New("tests failed")); isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("deterministic test failure was misclassified as infrastructure: %v", err)
	}
	if err := validateAttemptPatchSemantics(context.Background(), visualhive.FindingLifecycle{}, Attempt{Worktree: filepath.Join(t.TempDir(), "missing-worktree")}, []string{"src/value.txt"}); !isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("git diff launch/runtime failure was charged to the model: %v", err)
	}
}

func TestWorktreeInfrastructureFailureResumesProvisioning(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &healthFailureProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "missing-base", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:worktree-infra", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); !IsResumableFailureError(err) {
		t.Fatalf("worktree infrastructure failure was not paused: %v", err)
	}
	failed, _ := state.Get(finding.RepositoryFingerprint)
	if failed.Stage != StageFailed || failed.ResumeStage != StagePreparing || failed.AttemptCounted || failed.CountedModelAttempts() != 0 {
		t.Fatalf("worktree failure checkpoint is not safely resumable: %+v", failed)
	}
	if _, err := state.ResumeRetry(RetryRequest{
		RepositoryFingerprint: finding.RepositoryFingerprint, ExpectedAttempt: failed.Attempt,
		ExpectedFailureClass: failed.LastFailureClass, ExpectedFailureID: failed.LastFailureID,
		Actor: "test-operator", Reason: "base ref corrected",
	}); err != nil {
		t.Fatal(err)
	}
	worker.Config.BaseBranch = "main"
	if _, err := worker.Run(context.Background(), finding); err != nil {
		t.Fatal(err)
	}
	completed, _ := state.Get(finding.RepositoryFingerprint)
	if completed.Attempt != 1 || !completed.AttemptCounted || lifecycle.starts != 1 || provider.runs != 1 {
		t.Fatalf("resumed provisioning did not complete the same model ordinal: %+v starts=%d runs=%d", completed, lifecycle.starts, provider.runs)
	}
}

func TestModelAttemptCountReconcilesExactlyOnceAcrossReload(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:crash"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
		Branch: "hive/repair-crash-a1", Worktree: t.TempDir(), Stage: StageModelComplete,
		Provider: "test-model", ModelSummary: "durable successful model result", StartedAt: time.Now().UTC(),
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}

	// Simulate a stop after the lifecycle increment committed but before the
	// repair-state AttemptCounted write. The reloaded finding is authoritative.
	lifecycle := &fakeLifecycle{}
	if err := lifecycle.MarkRepairStarted(fingerprint, attempt.Branch); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := reloaded.Get(fingerprint)
	worker := &Worker{State: reloaded, Lifecycle: lifecycle}
	finding := visualhive.FindingLifecycle{
		RepositoryFingerprint: fingerprint, RepairAttempts: 1, Branch: attempt.Branch,
		Status: visualhive.StatusRepairRunning,
	}
	if err := worker.ensureAttemptCounted(finding, &saved); err != nil {
		t.Fatal(err)
	}
	if err := worker.ensureAttemptCounted(finding, &saved); err != nil {
		t.Fatal(err)
	}
	finalStore, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	final, _ := finalStore.Get(fingerprint)
	if !final.AttemptCounted || final.Attempt != 1 || lifecycle.starts != 1 {
		t.Fatalf("crash recovery double-counted the model attempt: attempt=%+v starts=%d", final, lifecycle.starts)
	}
}

func TestStaleModelCheckpointCannotSpendNewRecurrenceBudget(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	worktree := filepath.Join(t.TempDir(), "worktrees", "stale")
	if err := prepareWorktree(context.Background(), repository, worktree, "hive/repair-stale-a1", "main", "", remote); err != nil {
		t.Fatal(err)
	}
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:stale-recurrence"
	if err := state.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Recurrence: 0, Attempt: 1,
		Branch: "hive/repair-stale-a1", Worktree: worktree, Stage: StageModelComplete,
		Provider: "test-model", ModelSummary: "stale result", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	provider := &healthFailureProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Dir(worktree), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Recurrences: 1, Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); err != nil {
		t.Fatal(err)
	}
	current, _ := state.Get(fingerprint)
	if current.Recurrence != 1 || current.Attempt != 1 || !current.AttemptCounted || lifecycle.starts != 1 || provider.runs != 1 {
		t.Fatalf("stale model checkpoint affected the new recurrence: %+v starts=%d runs=%d", current, lifecycle.starts, provider.runs)
	}
}

func TestAmbiguousModelInvocationIsCountedWithoutReexecution(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktree := filepath.Join(t.TempDir(), "worktrees", "ambiguous")
	branch := "hive/repair-ambiguous-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:ambiguous-model"
	if err := state.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
		Branch: branch, Worktree: worktree, Stage: StageModelRunning, Provider: "test-model",
		ModelInvocationID: "invocation-before-crash", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	provider := &healthFailureProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Dir(worktree), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); !IsRetryableAttemptError(err) || !strings.Contains(err.Error(), "invocation-before-crash") {
		t.Fatalf("ambiguous invocation did not fail closed as a counted model attempt: %v", err)
	}
	saved, _ := state.Get(fingerprint)
	if saved.Stage != StageNoChange || !saved.AttemptCounted || saved.CountedModelAttempts() != 1 || provider.runs != 0 || lifecycle.starts != 1 {
		t.Fatalf("ambiguous invocation was reexecuted or not counted exactly once: %+v starts=%d runs=%d", saved, lifecycle.starts, provider.runs)
	}
}

func TestAttemptCounterDriftFailsClosed(t *testing.T) {
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{State: state, Lifecycle: lifecycle}
	attempt := Attempt{RepositoryFingerprint: "finding", Recurrence: 2, Attempt: 3, Stage: StageModelComplete}
	if err := worker.ensureAttemptCounted(visualhive.FindingLifecycle{
		RepositoryFingerprint: "finding", Recurrences: 2, RepairAttempts: 1,
	}, &attempt); err == nil || !strings.Contains(err.Error(), "counter drift") {
		t.Fatalf("counter gap was not rejected: %v", err)
	}
	if lifecycle.starts != 0 || lifecycle.retries != 0 {
		t.Fatalf("counter drift mutated lifecycle: %+v", lifecycle)
	}
	counted := Attempt{RepositoryFingerprint: "finding", Recurrence: 2, Attempt: 3, AttemptCounted: true, Branch: "hive/repair-exact", Stage: StageModelComplete}
	if err := worker.ensureAttemptCounted(visualhive.FindingLifecycle{
		RepositoryFingerprint: "finding", Recurrences: 2, RepairAttempts: 3,
		Status: visualhive.StatusRepairRunning, Branch: "hive/repair-other",
	}, &counted); err == nil || !strings.Contains(err.Error(), "branch/status") {
		t.Fatalf("counted attempt with a different lifecycle branch was accepted: %v", err)
	}
}

func TestCommitAndPushCrashRecoveryAreIdempotent(t *testing.T) {
	repository, expectedRemote := seedGitRepository(t)
	worktree := filepath.Join(t.TempDir(), "worktrees", "commit-recovery")
	branch := "hive/repair-commit-recovery-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", expectedRemote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := commitRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
	if err != nil {
		t.Fatal(err)
	}
	recoveredSHA, recovered, err := recoverCommittedRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
	if err != nil || !recovered || recoveredSHA != sha {
		t.Fatalf("committed repair was not recovered exactly: sha=%s recovered=%s ok=%t err=%v", sha, recoveredSHA, recovered, err)
	}
	if _, err := runGit(context.Background(), worktree, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	remote, err := remoteRepairBranchHead(context.Background(), worktree, expectedRemote, branch)
	if err != nil || remote != sha {
		t.Fatalf("pushed exact head was not recoverable: remote=%s sha=%s err=%v", remote, sha, err)
	}
}

func TestCommittedCheckpointRejectsMovedLocalHeadBeforePush(t *testing.T) {
	repository, expectedRemote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "moved-head")
	branch := "hive/repair-moved-head-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", expectedRemote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := commitRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), worktree, "switch", "--detach", "HEAD^"); err != nil {
		t.Fatal(err)
	}
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:moved-head"
	if err := state.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true,
		Branch: branch, Worktree: worktree, Stage: StageCommitted, Provider: "test-model",
		CommitSHA: sha, ChangedFiles: []string{"src/value.txt"}, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: expectedRemote,
			Policy: automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
		},
		Provider: &healthFailureProvider{}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, RepairAttempts: 1, Status: visualhive.StatusRepairRunning,
		Branch: branch, Title: "Repair value", Body: "broken", IssueKind: "functional", Severity: "high",
		IssueNumber: 9, IssueURL: "https://example.test/issues/9", OwningAgentHint: "quality",
	}
	if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "does not match checkpoint commit") {
		t.Fatalf("moved local head was not rejected before push: %v", err)
	}
	remote, err := remoteRepairBranchHead(context.Background(), worktree, expectedRemote, branch)
	if err != nil || remote != "" || pulls.calls != 0 {
		t.Fatalf("moved local head mutated remote state: remote=%s pulls=%d err=%v", remote, pulls.calls, err)
	}
}

func TestResumedSideEffectStagesRequireExactLifecycleBinding(t *testing.T) {
	for _, test := range []struct {
		name   string
		stage  Stage
		mutate func(*visualhive.FindingLifecycle)
	}{
		{name: "validated status", stage: StageValidated, mutate: func(f *visualhive.FindingLifecycle) { f.Status = visualhive.StatusIssueOpen }},
		{name: "committed count", stage: StageCommitted, mutate: func(f *visualhive.FindingLifecycle) { f.RepairAttempts = 2 }},
		{name: "pushed branch", stage: StagePushed, mutate: func(f *visualhive.FindingLifecycle) { f.Branch = "hive/repair-other" }},
		{name: "repository identity", stage: StagePushed, mutate: func(f *visualhive.FindingLifecycle) { f.Repository = "owner/other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, remote := seedGitRepository(t)
			state, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			fingerprint := "owner/repo:resume-binding"
			branch := "hive/repair-resume-a1"
			attempt := Attempt{
				Repository: "owner/repo", RepositoryFingerprint: fingerprint, Recurrence: 3, Attempt: 1,
				AttemptCounted: true, LifecycleStarted: true, Branch: branch, Worktree: t.TempDir(), Stage: test.stage,
				Provider: "test-model", CommitSHA: strings.Repeat("a", 40), ChangedFiles: []string{"src/value.txt"}, StartedAt: time.Now().UTC(),
			}
			if err := state.Put(attempt); err != nil {
				t.Fatal(err)
			}
			lifecycle := &fakeLifecycle{}
			pulls := &fakePRClient{state: state}
			worker := &Worker{
				Config: Config{
					RepositoryDir: repository, WorktreeRoot: t.TempDir(), BaseBranch: "main", ExpectedRemoteURL: remote,
					Policy: automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo", "owner/other"}, MaxRepairAttempts: 3},
				},
				Provider: &healthFailureProvider{}, State: state, Lifecycle: lifecycle, GitHub: pulls,
			}
			finding := visualhive.FindingLifecycle{
				Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Recurrences: 3, RepairAttempts: 1,
				Status: visualhive.StatusRepairRunning, Branch: branch, Title: "Repair value", Body: "broken", IssueKind: "functional",
				Severity: "high", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
			}
			test.mutate(&finding)
			if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "resumed repair checkpoint") {
				t.Fatalf("stale %s checkpoint was accepted: %v", test.stage, err)
			}
			if len(lifecycle.decisions) != 0 || pulls.calls != 0 {
				t.Fatalf("stale checkpoint performed a side effect: decisions=%v pulls=%d", lifecycle.decisions, pulls.calls)
			}
			persisted, _ := state.Get(fingerprint)
			if persisted.Stage != test.stage {
				t.Fatalf("stale checkpoint mutated durable stage: got %s want %s", persisted.Stage, test.stage)
			}
		})
	}
}

func TestPushedCheckpointRejectsPullRequestWithoutExactHeadSHA(t *testing.T) {
	repository, remote := seedGitRepository(t)
	branch := "hive/repair-empty-head-a1"
	runCommand(t, repository, "git", "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "src/value.txt")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "fix: value\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	commitSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	candidateParent := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^"))
	candidateTree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^{tree}"))
	runCommand(t, repository, "git", "push", "origin", commitSHA+":refs/heads/"+branch)
	runCommand(t, repository, "git", "checkout", "main")
	state, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:empty-pr-head"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true,
		Branch: branch, Worktree: t.TempDir(), Stage: StagePushed, Provider: "test-model",
		CommitSHA: commitSHA, CandidateParent: candidateParent, CandidateTree: candidateTree,
		ChangedFiles: []string{"src/value.txt"}, StartedAt: time.Now().UTC(),
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: t.TempDir(), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy: automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
		},
		Provider: &healthFailureProvider{}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, RepairAttempts: 1,
		Status: visualhive.StatusRepairRunning, Branch: branch, Title: "Repair value", Body: "broken", IssueKind: "functional",
		Severity: "high", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "PR head") {
		t.Fatalf("empty GitHub PR head was accepted: %v", err)
	}
	persisted, _ := state.Get(fingerprint)
	if pulls.calls != 1 || persisted.Stage != StagePushed || persisted.PRNumber != 0 {
		t.Fatalf("unbound PR response mutated durable readiness: attempt=%+v calls=%d", persisted, pulls.calls)
	}
}

func TestPushedCheckpointProtectsBaselineRootDeclaredOnlyByRemoteCommit(t *testing.T) {
	repository, remote := seedGitRepository(t)
	writeBaselineProtectionFile(t, repository, "visual-hive.config.yaml", "visual:\n  snapshotDir: public/reviewed-reference\n")
	writeBaselineProtectionFile(t, repository, "public/reviewed-reference/home.png", "reviewed\n")
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "seed reviewed baseline")
	runCommand(t, repository, "git", "push", "origin", "main")
	trusted, err := InspectVisualBaselineProtection(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	branch := "hive/repair-remote-baseline-a1"
	runCommand(t, repository, "git", "checkout", "-b", branch)
	writeBaselineProtectionFile(t, repository, "visual-hive.config.yaml", "visual:\n  snapshotDir: public/new-reference\n")
	writeBaselineProtectionFile(t, repository, "public/new-reference/home.png", "unreviewed\n")
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "fix: substitute baseline\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	commitSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	candidateParent := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^"))
	candidateTree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^{tree}"))
	runCommand(t, repository, "git", "push", "origin", commitSHA+":refs/heads/"+branch)
	runCommand(t, repository, "git", "checkout", "main")

	state, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:remote-baseline"
	if err := state.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true,
		Branch: branch, Worktree: t.TempDir(), Stage: StagePushed, Provider: "test-model", CommitSHA: commitSHA,
		CandidateParent: candidateParent, CandidateTree: candidateTree,
		ChangedFiles: []string{"public/new-reference/home.png", "visual-hive.config.yaml"}, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	pulls := &fakePRClient{state: state}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: t.TempDir(), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"**"}, BaselineProtection: trusted,
		},
		Provider: &healthFailureProvider{}, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, RepairAttempts: 1,
		Status: visualhive.StatusRepairRunning, Branch: branch, Title: "Repair value", Body: "broken", IssueKind: "functional",
		Severity: "high", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "protected visual baseline") {
		t.Fatalf("pushed checkpoint accepted baseline root declared only by exact remote commit: %v", err)
	}
	if pulls.calls != 0 || len(lifecycle.decisions) != 0 {
		t.Fatalf("rejected pushed baseline performed side effects: pulls=%d decisions=%v", pulls.calls, lifecycle.decisions)
	}
}

func TestRepairPushRejectsRepositoryControlledPrePushHook(t *testing.T) {
	repository, remote := seedGitRepository(t)
	branch := "hive/repair-hook-proof-a1"
	runCommand(t, repository, "git", "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "fix: value\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	commitSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	hooks := filepath.Join(repository, "attacker-hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ntouch hook-ran\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "config", "--local", "core.hooksPath", hooks)
	if err := pushRepairBranchExact(context.Background(), repository, remote, branch, commitSHA, "123", "repair"); err == nil || !strings.Contains(err.Error(), "core.hookspath") {
		t.Fatalf("controller-owned push did not reject repository hook config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repository, "hook-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository-controlled pre-push hook executed: %v", err)
	}
}

func TestExpectedRemoteRejectsUnsafeWorktreeScopedGitConfiguration(t *testing.T) {
	repository, remote := seedGitRepository(t)
	runCommand(t, repository, "git", "config", "--local", "extensions.worktreeConfig", "true")
	runCommand(t, repository, "git", "config", "--worktree", "filter.hiveprobe.process", "malicious-filter")
	if _, err := validateExpectedRemoteURL(context.Background(), repository, remote); err == nil || !strings.Contains(err.Error(), "worktree") || !strings.Contains(err.Error(), "filter.hiveprobe.process") {
		t.Fatalf("unsafe worktree-scoped Git configuration was not rejected: %v", err)
	}
}

func TestExpectedRemoteRejectsBareWorktreeConfiguration(t *testing.T) {
	repository, remote := seedGitRepository(t)
	runCommand(t, repository, "git", "config", "--local", "core.bare", "true")
	if _, err := validateExpectedRemoteURL(context.Background(), repository, remote); err == nil || !strings.Contains(err.Error(), "core.bare") {
		t.Fatalf("core.bare=true was not rejected before controller Git: %v", err)
	}
}

func TestCredentiallessLocalTransportDisablesTargetUploadPackHook(t *testing.T) {
	repository, _ := seedGitRepository(t)
	runCommand(t, repository, "git", "config", "--local", "uploadpack.packObjectsHook", "hive-malicious-upload-pack-hook-that-does-not-exist")
	transport := filepath.Join(t.TempDir(), "transport.git")
	if _, err := runGitCommand(context.Background(), t.TempDir(), "", "init", "--bare", transport); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	if _, err := runGitCommand(context.Background(), transport, "", "fetch", "--no-tags", "--force", repository, "+"+head+":refs/hive/test/import"); err != nil {
		t.Fatalf("target upload-pack hook was not disabled by command-scoped controller config: %v", err)
	}
	if imported, err := runGitCommand(context.Background(), transport, "", "rev-parse", "--verify", "refs/hive/test/import^{commit}"); err != nil || !strings.EqualFold(strings.TrimSpace(imported), head) {
		t.Fatalf("credentialless local transport imported wrong commit: %q err=%v", imported, err)
	}
}

func TestLocalGitFilterCannotReceiveControllerTransportAuthority(t *testing.T) {
	repository, remote := seedGitRepository(t)
	sentinel := filepath.Join(t.TempDir(), "filter-environment")
	t.Setenv("GO_WANT_REPAIR_FILTER_HELPER", "1")
	t.Setenv("HIVE_TEST_FILTER_SENTINEL", sentinel)
	executable := os.Args[0]
	if runtime.GOOS == "windows" {
		executable = filepath.ToSlash(executable)
	}
	filter := fmt.Sprintf("\"%s\" -test.run=^TestRepairLocalGitFilterHelperProcess$", executable)
	runCommand(t, repository, "git", "config", "--local", "filter.hiveprobe.clean", filter)
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("src/value.txt filter=hiveprobe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("filter probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := gittransport.WithControllerToken(context.Background(), "private-controller-token")
	if _, err := runGit(ctx, repository, "add", ".gitattributes", "src/value.txt"); err != nil {
		t.Fatalf("credentialless local Git probe: %v", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || strings.TrimSpace(string(data)) != "credentialless" {
		t.Fatalf("repository filter observed controller transport authority: %q err=%v", data, err)
	}
	if _, err := validateExpectedRemoteURL(ctx, repository, remote); err == nil || !strings.Contains(err.Error(), "filter.hiveprobe.clean") {
		t.Fatalf("repository executable filter config was not rejected before transport: %v", err)
	}
}

func TestRepairForceLeaseBindsObservedRemoteSHA(t *testing.T) {
	branch := "hive/repair-proof-a1"
	head := strings.Repeat("a", 40)
	if got, want := repairForceLease(branch, head), "--force-with-lease=refs/heads/"+branch+":"+head; got != want {
		t.Fatalf("force-with-lease is not bound to the observed remote SHA: got %q want %q", got, want)
	}
	if got, want := repairForceLease(branch, ""), "--force-with-lease=refs/heads/"+branch+":"; got != want {
		t.Fatalf("new branch lease is not bound to ref absence: got %q want %q", got, want)
	}
}

func TestPushRepairBranchRejectsRemoteURLRedirection(t *testing.T) {
	for _, test := range []struct {
		name      string
		wantError string
		configure func(context.Context, string, string, string) error
	}{
		{
			name:      "foreign fetch URL",
			wantError: "fetch URL does not match",
			configure: func(ctx context.Context, repository, expected, foreign string) error {
				if _, err := runGit(ctx, repository, "config", "--replace-all", "remote.origin.url", foreign); err != nil {
					return err
				}
				_, err := runGit(ctx, repository, "config", "--add", "remote.origin.pushurl", expected)
				return err
			},
		},
		{
			name:      "foreign push URL",
			wantError: "push URL does not match",
			configure: func(ctx context.Context, repository, _, foreign string) error {
				_, err := runGit(ctx, repository, "config", "--add", "remote.origin.pushurl", foreign)
				return err
			},
		},
		{
			name:      "multiple push URLs",
			wantError: "exactly one configured push URL",
			configure: func(ctx context.Context, repository, expected, foreign string) error {
				if _, err := runGit(ctx, repository, "config", "--add", "remote.origin.pushurl", expected); err != nil {
					return err
				}
				_, err := runGit(ctx, repository, "config", "--add", "remote.origin.pushurl", foreign)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, expected := seedGitRepository(t)
			foreign := filepath.Join(t.TempDir(), "foreign.git")
			if _, err := runGit(ctx, filepath.Dir(foreign), "init", "--bare", foreign); err != nil {
				t.Fatal(err)
			}
			branch := "hive/repair-remote-binding-a1"
			worktree := filepath.Join(t.TempDir(), "worktrees", "remote-binding")
			if err := prepareWorktree(ctx, repository, worktree, branch, "main", "", expected); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("remote-bound\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sha, err := commitRepair(ctx, worktree, []string{"src/value.txt"}, "Bind remote", 9, "123")
			if err != nil {
				t.Fatal(err)
			}
			if err := test.configure(ctx, repository, expected, foreign); err != nil {
				t.Fatal(err)
			}
			err = pushRepairBranchExact(ctx, worktree, expected, branch, sha, "123", "repair")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("remote redirection was not rejected before push: %v", err)
			}
			ref := "refs/heads/" + branch
			for label, remote := range map[string]string{"expected": expected, "foreign": foreign} {
				if _, refErr := runGit(ctx, remote, "show-ref", "--verify", "--quiet", ref); refErr == nil {
					t.Fatalf("%s remote received unauthorized ref %s", label, ref)
				}
			}
		})
	}
}

func TestPushRepairBranchRefusesUnownedRemoteTip(t *testing.T) {
	repository, remote := seedGitRepository(t)
	branch := "hive/repair-ownership-a1"
	if _, err := runGit(context.Background(), repository, "push", "origin", "main:refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	remoteBefore := strings.TrimSpace(gitOutput(t, remote, "rev-parse", branch))
	worktree := filepath.Join(t.TempDir(), "worktrees", "ownership")
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := commitRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
	if err != nil {
		t.Fatal(err)
	}
	err = pushRepairBranchExact(context.Background(), worktree, remote, branch, sha, "123", "repair")
	if err == nil || !strings.Contains(err.Error(), "lacks exact Hive ownership") {
		t.Fatalf("unowned remote tip was overwritten: %v", err)
	}
	if remoteAfter := strings.TrimSpace(gitOutput(t, remote, "rev-parse", branch)); remoteAfter != remoteBefore {
		t.Fatalf("refusal moved remote branch: before=%s after=%s", remoteBefore, remoteAfter)
	}
}

func TestPushRepairBranchRefusesWrongRepositoryOrDivergentOwnedTip(t *testing.T) {
	for _, test := range []struct {
		name             string
		remoteRepository string
		wantError        string
	}{
		{name: "wrong repository", remoteRepository: "456", wantError: "repository ID 123"},
		{name: "divergent owned history", remoteRepository: "123", wantError: "is not an ancestor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, remote := seedGitRepository(t)
			branch := "hive/repair-divergent-a1"
			attacker := filepath.Join(t.TempDir(), "remote-writer")
			if _, err := runGit(context.Background(), t.TempDir(), "clone", remote, attacker); err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(context.Background(), attacker, "switch", "-c", branch, "origin/main"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(attacker, "remote.txt"), []byte("remote\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(context.Background(), attacker, "add", "remote.txt"); err != nil {
				t.Fatal(err)
			}
			trailers := fmt.Sprintf("Hive-Repository-ID: %s\nHive-Operation: repair", test.remoteRepository)
			if _, err := runGit(context.Background(), attacker, "-c", "user.name=Hive", "-c", "user.email=hive@example.test", "commit", "-m", "remote owned commit", "-m", trailers); err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(context.Background(), attacker, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
				t.Fatal(err)
			}
			remoteBefore := strings.TrimSpace(gitOutput(t, remote, "rev-parse", branch))

			worktree := filepath.Join(t.TempDir(), "worktrees", "local")
			if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sha, err := commitRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
			if err != nil {
				t.Fatal(err)
			}
			err = pushRepairBranchExact(context.Background(), worktree, remote, branch, sha, "123", "repair")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("unsafe remote tip was accepted: %v", err)
			}
			if remoteAfter := strings.TrimSpace(gitOutput(t, remote, "rev-parse", branch)); remoteAfter != remoteBefore {
				t.Fatalf("refusal moved remote branch: before=%s after=%s", remoteBefore, remoteAfter)
			}
		})
	}
}

func TestPushRepairBranchAdvancesOwnedAncestorWithExactLease(t *testing.T) {
	repository, remote := seedGitRepository(t)
	branch := "hive/repair-owned-a1"
	worktree := filepath.Join(t.TempDir(), "worktrees", "owned")
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := commitRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
	if err == nil {
		err = pushRepairBranchExact(context.Background(), worktree, remote, branch, first, "123", "repair")
	}
	if err != nil {
		t.Fatalf("initial owned push failed: sha=%s err=%v", first, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := commitRepair(context.Background(), worktree, []string{"src/value.txt"}, "Repair value", 9, "123")
	if err != nil {
		t.Fatal(err)
	}
	if err := pushRepairBranchExact(context.Background(), worktree, remote, branch, second, "123", "repair"); err != nil {
		t.Fatalf("owned descendant push failed: %v", err)
	}
	if remoteHead := strings.TrimSpace(gitOutput(t, remote, "rev-parse", branch)); remoteHead != second {
		t.Fatalf("remote head=%s want=%s", remoteHead, second)
	}
}

func TestPROpenLifecycleCheckpointReconcilesAfterCrash(t *testing.T) {
	repository, remote := seedGitRepository(t)
	branch := "hive/repair-pr-a1"
	runCommand(t, repository, "git", "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "src/value.txt")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "fix: value\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	commitSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	candidateParent := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^"))
	candidateTree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^{tree}"))
	runCommand(t, repository, "git", "push", "origin", commitSHA+":refs/heads/"+branch)
	runCommand(t, repository, "git", "checkout", "main")
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:pr-lifecycle-crash"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true,
		Branch: branch, Worktree: t.TempDir(), Stage: StagePROpen, Provider: "test-model",
		CommitSHA: commitSHA, CandidateParent: candidateParent, CandidateTree: candidateTree, ChangedFiles: []string{"src/value.txt"},
		PRNumber: 17, PRURL: "https://example.test/pull/17", StartedAt: time.Now().UTC(),
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config:   Config{RepositoryDir: repository, WorktreeRoot: t.TempDir(), BaseBranch: "main", ExpectedRemoteURL: remote},
		Provider: &healthFailureProvider{}, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, RepairAttempts: 1,
		Status: visualhive.StatusPROpen, Branch: attempt.Branch, RepairCommitSHA: attempt.CommitSHA,
		PRNumber: attempt.PRNumber, PRURL: attempt.PRURL, Title: "Repair value", Body: "broken", IssueKind: "functional",
		Severity: "high", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, _ := state.Get(fingerprint)
	if !reconciled.LifecyclePROpen || lifecycle.prOpens != 0 || result.PRNumber != 17 {
		t.Fatalf("already-recorded PR lifecycle was duplicated or not checkpointed: %+v opens=%d result=%+v", reconciled, lifecycle.prOpens, result)
	}
}

func TestPROpenCheckpointRejectsLiveHeadDriftWithBaselineProtectionDisabled(t *testing.T) {
	repository, remote := seedGitRepository(t)
	branch := "hive/repair-pr-drift-a1"
	runCommand(t, repository, "git", "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "src/value.txt")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "fix: value\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	commitSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	candidateParent := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^"))
	candidateTree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", commitSHA+"^{tree}"))
	runCommand(t, repository, "git", "push", "origin", commitSHA+":refs/heads/"+branch)
	runCommand(t, repository, "git", "checkout", "main")

	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:pr-live-drift"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecyclePROpen: true,
		Branch: branch, Worktree: t.TempDir(), Stage: StagePROpen, Provider: "test-model", CommitSHA: commitSHA,
		CandidateParent: candidateParent, CandidateTree: candidateTree, ChangedFiles: []string{"src/value.txt"},
		PRNumber: 17, PRURL: "https://example.test/pull/17", StartedAt: time.Now().UTC(),
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	pulls := &fakePRClient{state: state, inspect: &hivegithub.ManagedPullRequestSnapshot{
		Number: 17, URL: attempt.PRURL, State: "open", HeadBranch: branch, HeadSHA: strings.Repeat("f", 40), BaseBranch: "main", ChangedFiles: attempt.ChangedFiles,
	}}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: t.TempDir(), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"},
		},
		Provider: &healthFailureProvider{}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, RepairAttempts: 1,
		Status: visualhive.StatusPROpen, Branch: branch, RepairCommitSHA: commitSHA, PRNumber: 17, PRURL: attempt.PRURL,
		Title: "Repair value", Body: "broken", IssueKind: "functional", Severity: "high", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "managed repair pull request") {
		t.Fatalf("disabled-baseline open PR accepted live head drift: %v", err)
	}
	if pulls.inspectCalls != 1 {
		t.Fatalf("disabled-baseline open PR was not rebound exactly: inspect calls=%d", pulls.inspectCalls)
	}
}

func TestResumeRetryIsExactDurableAndAudited(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:patch-engine", Attempt: 2,
		Branch: "hive/repair-patch-a2", Worktree: t.TempDir(), Stage: StageFailed, Provider: "test-model",
		AttemptCounted: true, LifecycleStarted: true, ModelPatch: "diff --git a/src/a b/src/a\n",
		LastFailureClass: FailurePatchEngine, LastFailureID: "patch-failure-1", LastFailure: "git apply unavailable", ResumeStage: StageModelComplete,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.ResumeRetry(RetryRequest{
		RepositoryFingerprint: attempt.RepositoryFingerprint, ExpectedAttempt: attempt.Attempt,
		ExpectedFailureClass: FailurePatchEngine, ExpectedFailureID: attempt.LastFailureID,
		Actor: "authenticated-operator", Reason: "git runtime restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stage != StageModelComplete || resumed.Attempt != 2 || !resumed.AttemptCounted || resumed.ModelPatch != attempt.ModelPatch || resumed.LastFailureClass != FailurePatchEngine || resumed.RetryAuthorizedAt.IsZero() {
		t.Fatalf("safe retry did not preserve the exact counted model result: %+v", resumed)
	}
	reloaded, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := reloaded.Get(attempt.RepositoryFingerprint)
	if persisted.Stage != StageModelComplete || persisted.Attempt != 2 || persisted.RetryActor != "authenticated-operator" {
		t.Fatalf("retry transition was not durable: %+v", persisted)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), `"phase":"intent"`) || !strings.Contains(string(audit), `"phase":"commit"`) || !strings.Contains(string(audit), "authenticated-operator") || !strings.Contains(string(audit), "git runtime restored") {
		t.Fatalf("retry audit is incomplete: %q err=%v", audit, err)
	}
}

func TestRetryAuditReconcilesCommittedStateAfterCrash(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	authorizedAt := time.Now().UTC()
	request := RetryRequest{
		RepositoryFingerprint: "owner/repo:audit-recovery", ExpectedAttempt: 1,
		ExpectedFailureClass: FailureInfrastructure, ExpectedFailureID: "failure-audit", Actor: "operator", Reason: "runtime restored",
	}
	attempt := Attempt{
		RepositoryFingerprint: request.RepositoryFingerprint, Attempt: 1, Stage: StagePrepared,
		LastFailureClass: request.ExpectedFailureClass, LastFailureID: request.ExpectedFailureID, RetryTransactionID: retryTransactionID(request, authorizedAt, StagePrepared),
		RetryAuthorizedAt: authorizedAt, RetryActor: request.Actor, RetryReason: request.Reason, RetryResumeStage: StagePrepared, StartedAt: time.Now().UTC(),
	}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	intent := RetryAuditEntry{
		Timestamp: authorizedAt, Action: "resume_retry", Phase: "intent", TransactionID: attempt.RetryTransactionID,
		Allowed: true, RepositoryFingerprint: attempt.RepositoryFingerprint, ExpectedAttempt: 1,
		ExpectedFailureClass: FailureInfrastructure, ExpectedFailureID: attempt.LastFailureID, ResumeStage: StagePrepared,
		Actor: request.Actor, Reason: request.Reason,
	}
	data, _ := json.Marshal(intent)
	tornAudit := append(append(data, '\n'), []byte(`{"phase":"comm`)...)
	if err := os.WriteFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"), tornAudit, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(stateDir); err != nil {
		t.Fatal(err)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"))
	if err != nil || strings.Count(string(audit), `"phase":"commit"`) != 1 || !strings.Contains(string(audit), "recovered durable retry transition") {
		t.Fatalf("missing retry commit was not reconciled: %q err=%v", audit, err)
	}
	quarantines, err := filepath.Glob(filepath.Join(stateDir, "repair-retry-audit.jsonl.torn-*"))
	if err != nil || len(quarantines) != 1 {
		t.Fatalf("torn retry audit tail was not quarantined: %v err=%v", quarantines, err)
	}
}

func TestRetryAuditRejectsMalformedCompleteRecord(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(stateDir); err == nil || !strings.Contains(err.Error(), "parse repair retry audit line") {
		t.Fatalf("complete malformed audit record did not fail closed: %v", err)
	}
}

func TestResumeRetryRejectsInvalidOrStaleCheckpoint(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{
		RepositoryFingerprint: "owner/repo:model", Attempt: 3, Stage: StageNoChange,
		AttemptCounted: true, LastFailureClass: FailureModel, LastFailureID: "model-failure-3", LastFailure: "invalid model output",
		StartedAt: time.Now().UTC(),
	}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	requests := []RetryRequest{
		{RepositoryFingerprint: attempt.RepositoryFingerprint, ExpectedAttempt: 2, ExpectedFailureClass: FailureModel, ExpectedFailureID: attempt.LastFailureID, Actor: "operator", Reason: "stale"},
		{RepositoryFingerprint: attempt.RepositoryFingerprint, ExpectedAttempt: 3, ExpectedFailureClass: FailureModel, ExpectedFailureID: "superseded-failure", Actor: "operator", Reason: "stale failure ID"},
		{RepositoryFingerprint: attempt.RepositoryFingerprint, ExpectedAttempt: 3, ExpectedFailureClass: FailureModel, ExpectedFailureID: attempt.LastFailureID, Actor: "operator", Reason: "must spend a new model ordinal"},
		{RepositoryFingerprint: "missing", ExpectedAttempt: 1, ExpectedFailureClass: FailureInfrastructure, ExpectedFailureID: "missing-failure", Actor: "operator", Reason: "missing"},
	}
	for _, request := range requests {
		if _, err := store.ResumeRetry(request); err == nil {
			t.Fatalf("invalid retry was accepted: %+v", request)
		}
	}
	saved, _ := store.Get(attempt.RepositoryFingerprint)
	if saved.Stage != StageNoChange || saved.Attempt != 3 {
		t.Fatalf("denied retry mutated state: %+v", saved)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"))
	if err != nil || strings.Count(string(audit), `"allowed":false`) != len(requests) {
		t.Fatalf("denied retries were not fully audited: %q err=%v", audit, err)
	}
}

func TestStateV3MigrationInfersPreviouslyCountedAttempt(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"schema_version":"hive.repair-worker-state.v3","attempts":{"finding":{"repository":"owner/repo","repository_fingerprint":"finding","attempt":2,"branch":"hive/repair-a2","worktree":"worktree","stage":"model_complete","provider":"codex","lifecycle_started":true,"started_at":"2026-07-10T00:00:00Z","updated_at":"2026-07-10T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(stateDir, "repair-worker-state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	attempt, ok := store.Get("finding")
	if !ok || !attempt.AttemptCounted || store.Snapshot().SchemaVersion != StateSchema {
		t.Fatalf("v3 migration lost counted attempt state: %+v state=%+v", attempt, store.Snapshot())
	}
}

func TestMigratedCountedPreparedAttemptCanResumeInfrastructureFailure(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"schema_version":"hive.repair-worker-state.v3","attempts":{"finding":{"repository":"owner/repo","repository_fingerprint":"finding","attempt":2,"branch":"hive/repair-a2","worktree":"worktree","stage":"prepared","provider":"codex","lifecycle_started":true,"started_at":"2026-07-10T00:00:00Z","updated_at":"2026-07-10T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(stateDir, "repair-worker-state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	attempt, _ := store.Get("finding")
	if !attempt.AttemptCounted || attempt.CountedModelAttempts() != 2 {
		t.Fatalf("legacy counted prepared attempt was not preserved: %+v", attempt)
	}
	if err := checkpointResumableFailure(store, &attempt, FailureInfrastructure, StagePrepared, errors.New("provider unavailable")); !IsResumableFailureError(err) {
		t.Fatalf("legacy infrastructure failure was not checkpointed: %v", err)
	}
	resumed, err := store.ResumeRetry(RetryRequest{
		RepositoryFingerprint: "finding", ExpectedAttempt: 2, ExpectedFailureClass: FailureInfrastructure,
		ExpectedFailureID: attempt.LastFailureID, Actor: "operator", Reason: "provider restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stage != StagePrepared || !resumed.AttemptCounted || resumed.CountedModelAttempts() != 2 {
		t.Fatalf("legacy retry changed its already-counted model budget: %+v", resumed)
	}
}

func TestStateStoreReloadsDiskAfterIndeterminateRenameFailure(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{RepositoryFingerprint: "finding", Attempt: 1, Stage: StagePrepared, StartedAt: time.Now().UTC()}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	realRename := store.renameFile
	failedOnce := false
	store.renameFile = func(source, destination string) error {
		if failedOnce {
			return realRename(source, destination)
		}
		failedOnce = true
		if err := os.Rename(source, destination); err != nil {
			return err
		}
		return errors.New("directory flush failed after rename")
	}
	attempt.Stage = StageModelRunning
	if err := store.Put(attempt); err != nil {
		t.Fatalf("indeterminate replacement was not reconciled by a confirmed durable rewrite: %v", err)
	}
	reloaded, ok := store.Get(attempt.RepositoryFingerprint)
	if !ok || reloaded.Stage != StageModelRunning || store.Err() != nil {
		t.Fatalf("store did not adopt the bytes actually renamed to disk: %+v poison=%v", reloaded, store.Err())
	}
	store.renameFile = realRename
	attempt.Stage = StageModelComplete
	if err := store.Put(attempt); err != nil {
		t.Fatalf("reconciled store could not continue with a fresh durable write: %v", err)
	}
}

func TestStateStorePoisonsWhenIndeterminateRenameCannotReload(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{RepositoryFingerprint: "finding", Attempt: 1, Stage: StagePrepared, StartedAt: time.Now().UTC()}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	store.renameFile = func(source, destination string) error {
		_ = os.Remove(source)
		if err := os.WriteFile(destination, []byte("not-json\n"), 0o600); err != nil {
			return err
		}
		return errors.New("replacement result unknown")
	}
	attempt.Stage = StageModelRunning
	if err := store.Put(attempt); err == nil || store.Err() == nil || !strings.Contains(err.Error(), "poisoned") {
		t.Fatalf("unrecoverable replacement did not poison the store: err=%v poison=%v", err, store.Err())
	}
	if err := store.Put(attempt); err == nil || !strings.Contains(err.Error(), "poisoned") {
		t.Fatalf("poisoned store accepted another mutation: %v", err)
	}
}

func TestStateStoreRetriesRequestedStateAfterPreRenameFailure(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{RepositoryFingerprint: "finding", Attempt: 1, Stage: StagePrepared, StartedAt: time.Now().UTC()}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	realRename := store.renameFile
	failedOnce := false
	store.renameFile = func(source, destination string) error {
		if !failedOnce {
			failedOnce = true
			return errors.New("rename denied before replacement")
		}
		return realRename(source, destination)
	}
	attempt.Stage = StageModelRunning
	if err := store.Put(attempt); err != nil {
		t.Fatalf("requested state was not retried durably: %v", err)
	}
	persisted, _ := store.Get("finding")
	if persisted.Stage != StageModelRunning {
		t.Fatalf("prior state was falsely reported as the successful update: %+v", persisted)
	}
}

func TestRetryAuditRejectsSemanticallyUnboundIntent(t *testing.T) {
	stateDir := t.TempDir()
	request := RetryRequest{
		RepositoryFingerprint: "finding", ExpectedAttempt: 1, ExpectedFailureClass: FailureInfrastructure,
		ExpectedFailureID: "infra-1", Actor: "operator", Reason: "runtime repaired",
	}
	at := time.Now().UTC()
	entry := RetryAuditEntry{
		Timestamp: at, Action: "resume_retry", Phase: "intent", TransactionID: strings.Repeat("f", 32), Allowed: true,
		RepositoryFingerprint: request.RepositoryFingerprint, ExpectedAttempt: request.ExpectedAttempt,
		ExpectedFailureClass: request.ExpectedFailureClass, ExpectedFailureID: request.ExpectedFailureID, ResumeStage: StagePrepared, Actor: request.Actor, Reason: request.Reason,
	}
	data, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(stateDir); err == nil || !strings.Contains(err.Error(), "does not bind") {
		t.Fatalf("forged retry WAL intent was accepted: %v", err)
	}
}

func TestRetryAuditRejectsStateAuthorityMismatch(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	request := RetryRequest{
		RepositoryFingerprint: "finding", ExpectedAttempt: 1, ExpectedFailureClass: FailureInfrastructure,
		ExpectedFailureID: "infra-1", Actor: "operator", Reason: "runtime repaired",
	}
	at := time.Now().UTC()
	transactionID := retryTransactionID(request, at, StagePrepared)
	attempt := Attempt{
		RepositoryFingerprint: request.RepositoryFingerprint, Attempt: 1, Stage: StagePrepared,
		LastFailureClass: request.ExpectedFailureClass, LastFailureID: request.ExpectedFailureID,
		RetryTransactionID: transactionID, RetryAuthorizedAt: at, RetryActor: "different-actor", RetryReason: request.Reason, RetryResumeStage: StagePrepared,
		StartedAt: time.Now().UTC(),
	}
	if err := store.Put(attempt); err != nil {
		t.Fatal(err)
	}
	entry := RetryAuditEntry{
		Timestamp: at, Action: "resume_retry", Phase: "intent", TransactionID: transactionID, Allowed: true,
		RepositoryFingerprint: request.RepositoryFingerprint, ExpectedAttempt: request.ExpectedAttempt,
		ExpectedFailureClass: request.ExpectedFailureClass, ExpectedFailureID: request.ExpectedFailureID, ResumeStage: StagePrepared, Actor: request.Actor, Reason: request.Reason,
	}
	data, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(stateDir); err == nil || !strings.Contains(err.Error(), "persisted retry authority") {
		t.Fatalf("retry state/WAL authority mismatch was accepted: %v", err)
	}
}

func TestRetryAuditRejectsCommitBeforeIntent(t *testing.T) {
	stateDir := t.TempDir()
	request := RetryRequest{
		RepositoryFingerprint: "finding", ExpectedAttempt: 1, ExpectedFailureClass: FailureInfrastructure,
		ExpectedFailureID: "infra-1", Actor: "operator", Reason: "runtime repaired",
	}
	at := time.Now().UTC()
	intent := RetryAuditEntry{
		Timestamp: at, Action: "resume_retry", Phase: "intent", TransactionID: retryTransactionID(request, at, StagePrepared), Allowed: true,
		RepositoryFingerprint: request.RepositoryFingerprint, ExpectedAttempt: request.ExpectedAttempt,
		ExpectedFailureClass: request.ExpectedFailureClass, ExpectedFailureID: request.ExpectedFailureID,
		ResumeStage: StagePrepared, Actor: request.Actor, Reason: request.Reason,
	}
	commit := intent
	commit.Timestamp = at.Add(time.Second)
	commit.Phase = "commit"
	intentData, _ := json.Marshal(intent)
	commitData, _ := json.Marshal(commit)
	content := append(append(append([]byte(nil), commitData...), '\n'), intentData...)
	content = append(content, '\n')
	if err := os.WriteFile(filepath.Join(stateDir, "repair-retry-audit.jsonl"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(stateDir); err == nil || !strings.Contains(err.Error(), "precedes") {
		t.Fatalf("out-of-order retry WAL commit was accepted: %v", err)
	}
}
