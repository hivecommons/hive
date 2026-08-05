package repair

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type fakeProvider struct {
	runs           int
	requiredMarker string
}

func (p *fakeProvider) Name() string                 { return "test-model" }
func (p *fakeProvider) Health(context.Context) error { return nil }
func (p *fakeProvider) Run(_ context.Context, worktree, _ string) (ProviderResult, error) {
	if p.requiredMarker != "" {
		if _, err := os.Stat(p.requiredMarker); err != nil {
			return ProviderResult{}, fmt.Errorf("repair preparation marker is missing: %w", err)
		}
	}
	p.runs++
	value := "fixed\n"
	if p.runs > 1 {
		value = "fixed again\n"
	}
	return ProviderResult{Summary: "updated src/value.txt"}, os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte(value), 0o600)
}

type noChangeThenFixProvider struct{ runs int }

func (p *noChangeThenFixProvider) Name() string                 { return "test-model" }
func (p *noChangeThenFixProvider) Health(context.Context) error { return nil }
func (p *noChangeThenFixProvider) Run(_ context.Context, worktree, prompt string) (ProviderResult, error) {
	p.runs++
	if p.runs == 1 {
		return ProviderResult{Summary: "I could not identify the concrete failure."}, nil
	}
	if !strings.Contains(prompt, "Prior bounded model response") || !strings.Contains(prompt, "key=playwright.console_error.deploy-preview-smoke") {
		return ProviderResult{}, fmt.Errorf("retry prompt did not include prior response and verified evidence")
	}
	return ProviderResult{Summary: "fixed from verified evidence"}, os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("fixed after retry\n"), 0o600)
}

type patchProvider struct{}

func (p *patchProvider) Name() string                 { return "patch-model" }
func (p *patchProvider) Health(context.Context) error { return nil }
func (p *patchProvider) Run(_ context.Context, _ string, prompt string) (ProviderResult, error) {
	if !strings.Contains(prompt, modelPatchBegin) || !strings.Contains(prompt, "filesystem-denied") {
		return ProviderResult{}, fmt.Errorf("repair prompt did not require the read-only patch contract")
	}
	output := `HIVE_PATCH_BEGIN
diff --git a/src/value.txt b/src/value.txt
--- a/src/value.txt
+++ b/src/value.txt
@@ -1 +1 @@
-broken
+fixed by model patch
HIVE_PATCH_END`
	return ProviderResult{Summary: "proposed bounded patch", Output: output}, nil
}

type unsafeOutputProvider struct{ value string }

func (p *unsafeOutputProvider) Name() string                 { return "unsafe-test-model" }
func (p *unsafeOutputProvider) Health(context.Context) error { return nil }
func (p *unsafeOutputProvider) Run(context.Context, string, string) (ProviderResult, error) {
	return ProviderResult{Summary: p.value, Output: p.value}, nil
}

func TestWorkerRejectsUnsafeProviderOutputBeforeStatePersistence(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Join([]string{"AKIA", "ABCD", "EFGH", "IJKL", "MNOP"}, "")
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:unsafe-provider-output", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high", OwningAgentHint: "quality", IssueNumber: 44, IssueURL: "https://example.test/issues/44",
	}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: &unsafeOutputProvider{value: `const id = "` + secret + `"`}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{state: state},
	}
	_, runErr := worker.Run(context.Background(), finding)
	if !IsRetryableAttemptError(runErr) || !strings.Contains(runErr.Error(), "aws-access-key-id-v1") || strings.Contains(runErr.Error(), secret) {
		t.Fatalf("unsafe output did not produce a non-disclosing charged rejection: %v", runErr)
	}
	attempt, ok := state.Get(finding.RepositoryFingerprint)
	if !ok || !attempt.AttemptCounted || attempt.ModelPatch != "" || strings.Contains(attempt.ModelSummary, secret) || strings.Contains(attempt.LastFailure, secret) {
		t.Fatalf("unsafe provider bytes reached durable state: %+v", attempt)
	}
}

func TestRepairPreparationHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_REPAIR_PREPARATION_HELPER") != "1" {
		return
	}
	marker := os.Args[len(os.Args)-1]
	if err := os.WriteFile(marker, []byte("prepared\n"), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestWorkerPreparesIsolatedWorktreeBeforeModel(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "prepared")
	t.Setenv("GO_WANT_REPAIR_PREPARATION_HELPER", "1")
	provider := &fakeProvider{requiredMarker: marker}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:              automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths:  []string{"src/**"},
			PreparationCommands: []Command{{Name: os.Args[0], Args: []string{"-test.run=^TestRepairPreparationHelperProcess$", "--", marker}}},
			ValidationCommands:  []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout:        time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:prepared", Fingerprint: "prepared",
		Status: visualhive.StatusIssueOpen, Title: "Repair prepared value", Body: "Value should be fixed.",
		IssueKind: "functional", Severity: "medium", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}

	if _, err := worker.Run(context.Background(), finding); err != nil {
		t.Fatal(err)
	}
	if provider.runs != 1 {
		t.Fatalf("provider runs = %d, want 1", provider.runs)
	}
}

type fakeLifecycle struct {
	branch, sha string
	pr          int
	starts      int
	retries     int
	prOpens     int
	decisions   []string
}

func (f *fakeLifecycle) MarkRepairStarted(_ string, branch string) error {
	f.branch = branch
	f.starts++
	return nil
}
func (f *fakeLifecycle) MarkRepairRetry(_ string, branch string) error {
	f.branch = branch
	f.retries++
	return nil
}
func (f *fakeLifecycle) MarkPROpen(_ string, sha string, number int, _ string) error {
	f.sha, f.pr = sha, number
	f.prOpens++
	return nil
}
func (f *fakeLifecycle) RecordAuthorization(_ string, action string, allowed bool, _ string) {
	f.decisions = append(f.decisions, fmt.Sprintf("%s:%t", action, allowed))
}

type fakePRClient struct {
	calls        int
	inspectCalls int
	state        *Store
	inspect      *hivegithub.ManagedPullRequestSnapshot
	inspectErr   error
}

func (f *fakePRClient) UpsertRepairPullRequest(_ context.Context, _, branch, _, _, _, _, _ string) (hivegithub.RepairPullRequest, error) {
	f.calls++
	head := ""
	if f.state != nil {
		for _, attempt := range f.state.Snapshot().Attempts {
			if attempt != nil && attempt.Branch == branch {
				head = attempt.CommitSHA
				break
			}
		}
	}
	return hivegithub.RepairPullRequest{Number: 17, URL: "https://example.test/pull/17", HeadSHA: head}, nil
}

func (f *fakePRClient) InspectManagedPullRequestExact(_ context.Context, _, _ string, number int, _, branch, headSHA, base string) (hivegithub.ManagedPullRequestSnapshot, error) {
	f.inspectCalls++
	if f.inspectErr != nil {
		return hivegithub.ManagedPullRequestSnapshot{}, f.inspectErr
	}
	if f.inspect != nil {
		return *f.inspect, nil
	}
	files := []string(nil)
	url := "https://example.test/pull/17"
	if f.state != nil {
		for _, attempt := range f.state.Snapshot().Attempts {
			if attempt != nil && attempt.Branch == branch {
				files = append(files, attempt.ChangedFiles...)
				if attempt.PRURL != "" {
					url = attempt.PRURL
				}
				break
			}
		}
	}
	return hivegithub.ManagedPullRequestSnapshot{
		Number: number, URL: url, State: "open", HeadBranch: branch, HeadSHA: headSHA, BaseBranch: base, ChangedFiles: files,
	}, nil
}

func TestWorkerCreatesRealBranchCommitPushAndPRAndResumes(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:stable-finding", Fingerprint: "stable-finding",
		Status: visualhive.StatusIssueOpen, Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional",
		Severity: "medium", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		attempt, _ := state.Get(finding.RepositoryFingerprint)
		data, readErr := os.ReadFile(filepath.Join(attempt.Worktree, "src", "value.txt"))
		t.Fatalf("%v; attempt=%+v content=%q readErr=%v", err, attempt, data, readErr)
	}
	if provider.runs != 1 || pulls.calls != 1 || result.PRNumber != 17 || result.CommitSHA == "" || lifecycle.sha != result.CommitSHA {
		t.Fatalf("unexpected result=%+v provider=%d pulls=%d lifecycle=%+v", result, provider.runs, pulls.calls, lifecycle)
	}
	remoteContent := gitOutput(t, remote, "show", result.Branch+":src/value.txt")
	if strings.TrimSpace(remoteContent) != "fixed" {
		t.Fatalf("remote branch was not pushed: %q", remoteContent)
	}
	finding.Status = visualhive.StatusPROpen
	finding.RepairAttempts = 1
	finding.Branch = result.Branch
	finding.RepairCommitSHA = result.CommitSHA
	finding.PRNumber = result.PRNumber
	finding.PRURL = result.PRURL
	resumed, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || provider.runs != 1 || pulls.calls != 1 {
		t.Fatalf("restart should reuse completed attempt: %+v runs=%d pulls=%d", resumed, provider.runs, pulls.calls)
	}
}

func TestWorkerStartsFreshBoundedCycleForRecurrence(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:recurrence", Status: visualhive.StatusIssueOpen,
		Title: "Repair recurring value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	finding.Recurrences = 1
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	if first.Branch == second.Branch || !strings.Contains(second.Branch, "-r1-a1") || provider.runs != 2 || pulls.calls != 2 || lifecycle.starts != 2 || attempt.Recurrence != 1 || attempt.Attempt != 1 {
		t.Fatalf("recurrence did not start a fresh bounded repair cycle: first=%+v second=%+v attempt=%+v runs=%d pulls=%d starts=%d", first, second, attempt, provider.runs, pulls.calls, lifecycle.starts)
	}
}

func TestRepairStateMigratesV2WithInitialRecurrence(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"schema_version":"hive.repair-worker-state.v2","attempts":{"finding":{"repository":"owner/repo","repository_fingerprint":"finding","attempt":2,"branch":"hive/repair-proof-a2","worktree":"worktree","stage":"pr_open","provider":"codex","started_at":"2026-07-10T00:00:00Z","updated_at":"2026-07-10T00:00:00Z"}}}`
	path := filepath.Join(dir, "repair-worker-state.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	attempt, exists := store.Get("finding")
	if !exists || attempt.Recurrence != 0 || store.Snapshot().SchemaVersion != StateSchema {
		t.Fatalf("v2 repair state was not migrated: exists=%t attempt=%+v state=%+v", exists, attempt, store.Snapshot())
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), StateSchema) {
		t.Fatalf("migrated repair schema was not persisted: %q err=%v", data, err)
	}
}

func TestWorkerDeniesModelAtLowerACMMBeforeRun(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config:   Config{RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote, Policy: automation.Policy{ACMMLevel: 2, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}}},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	_, err := worker.Run(context.Background(), visualhive.FindingLifecycle{Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "fp", IssueNumber: 1, IssueURL: "https://example.test/1"})
	if err == nil || !strings.Contains(err.Error(), "denied") || provider.runs != 0 {
		t.Fatalf("expected pre-provider denial, got %v runs=%d", err, provider.runs)
	}
}

func TestWorkerRevisesTheSameBranchAndPullRequest(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:revision", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	finding.Status, finding.RepairAttempts = visualhive.StatusNeedsRevision, 1
	finding.Branch, finding.PRNumber, finding.RepairCommitSHA = first.Branch, first.PRNumber, first.CommitSHA
	checkpoint, _ := state.Get(finding.RepositoryFingerprint)
	if err := validateRevisionCheckpoint(context.Background(), finding, checkpoint); err != nil {
		t.Fatalf("exact revision checkpoint was rejected: %v", err)
	}
	drifted := finding
	drifted.RepairCommitSHA = strings.Repeat("a", 40)
	if err := validateRevisionCheckpoint(context.Background(), drifted, checkpoint); err == nil {
		t.Fatal("drifted revision head was accepted")
	}
	anchor := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	checkpoint.SpecialistRevisionAttempt = 2
	checkpoint.SpecialistRevisionStartedAt = anchor
	checkpoint.SpecialistRevisionBaseSHA = first.CommitSHA
	checkpoint.SpecialistRevisionBranch = first.Branch
	checkpoint.SpecialistRevisionPRNumber = first.PRNumber
	if err := state.Put(checkpoint); err != nil {
		t.Fatal(err)
	}
	worker.Config.AttemptStartedAt = anchor
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch != second.Branch || first.PRNumber != second.PRNumber || pulls.calls != 2 || provider.runs != 2 || lifecycle.starts != 1 || lifecycle.retries != 1 {
		t.Fatalf("revision created duplicate lifecycle objects: first=%+v second=%+v pulls=%d runs=%d", first, second, pulls.calls, provider.runs)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	if attempt.Attempt != 2 || attempt.Stage != StagePROpen || !attempt.StartedAt.Equal(anchor) || attempt.SpecialistRevisionAttempt != 0 {
		t.Fatalf("revision attempt was not persisted: %+v", attempt)
	}
}

func TestWorkerNoChangeRetryPreservesOpenBranchAndPullRequest(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:no-change-open", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	attempt.Stage = StageNoChange
	attempt.ModelSummary = "no incremental patch"
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	finding.Status, finding.RepairAttempts, finding.Branch, finding.PRNumber = visualhive.StatusRepairRunning, 1, first.Branch, first.PRNumber
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if second.Branch != first.Branch || second.PRNumber != first.PRNumber || pulls.calls != 2 || lifecycle.retries != 1 {
		t.Fatalf("no-change retry duplicated repair objects: first=%+v second=%+v pulls=%d lifecycle=%+v", first, second, pulls.calls, lifecycle)
	}
}

func TestWorkerStartsFreshAttemptAfterExactRepairRetirement(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:retired-pr", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.RetirePullRequestForVerification(PullRequestRetirement{
		RepositoryFingerprint: finding.RepositoryFingerprint, PRNumber: first.PRNumber, PRURL: first.PRURL,
		Branch: first.Branch, CommitSHA: first.CommitSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), remote, "update-ref", "-d", "refs/heads/"+first.Branch); err != nil {
		t.Fatal(err)
	}
	finding.RepairAttempts = 1
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if second.Branch == first.Branch || pulls.calls != 2 || provider.runs != 2 || lifecycle.starts != 2 {
		t.Fatalf("retired PR did not produce one fresh attempt: first=%+v second=%+v pulls=%d runs=%d lifecycle=%+v", first, second, pulls.calls, provider.runs, lifecycle)
	}
	current, exists := state.Get(finding.RepositoryFingerprint)
	if !exists || current.Stage != StagePROpen || !current.LifecyclePROpen || current.Attempt != 2 || current.Branch != second.Branch {
		t.Fatalf("fresh post-retirement checkpoint = %+v, exists=%t", current, exists)
	}
}

func TestWorkerStartsFreshBranchAfterMergedFixNeedsRevision(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:post-merge", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	finding.Status, finding.RepairAttempts, finding.MergeSHA = visualhive.StatusNeedsRevision, 1, "merged-first-fix"
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch == second.Branch || provider.runs != 2 || pulls.calls != 2 || lifecycle.starts != 2 {
		t.Fatalf("post-merge retry did not start a fresh bounded attempt: first=%+v second=%+v starts=%d pulls=%d runs=%d", first, second, lifecycle.starts, pulls.calls, provider.runs)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	if attempt.Attempt != 2 || !attempt.LifecycleStarted || attempt.PriorModelSummary == "" {
		t.Fatalf("post-merge attempt did not retain retry context: %+v", attempt)
	}
}

func TestWorkerRetriesNoChangeCheckpointOnCleanNewAttempt(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &noChangeThenFixProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			EvidenceSummary: "- key=playwright.console_error.deploy-preview-smoke source=playwright kind=console_error status=failed contract=deploy-preview-smoke target=deployPreview reason=404",
			ModelTimeout:    time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:no-change", Status: visualhive.StatusIssueOpen,
		Title: "Repair deploy-preview-smoke: console_error", Body: "Evidence-backed failure.", IssueKind: "functional", Severity: "high",
		AffectedContracts: []string{"deploy-preview-smoke"}, OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "without a source or test change") || !IsRetryableAttemptError(err) {
		t.Fatalf("expected bounded no-change failure, got %v", err)
	}
	first, _ := state.Get(finding.RepositoryFingerprint)
	if first.Stage != StageNoChange || first.ModelSummary == "" || first.Attempt != 1 {
		t.Fatalf("no-change checkpoint was not durable: %+v", first)
	}
	finding.Status, finding.RepairAttempts = visualhive.StatusRepairRunning, 1
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := state.Get(finding.RepositoryFingerprint)
	if provider.runs != 2 || pulls.calls != 1 || second.Attempt != 2 || second.Stage != StagePROpen || second.Branch == first.Branch || result.PRNumber != 17 {
		t.Fatalf("retry did not produce exactly one PR on a clean new attempt: first=%+v second=%+v result=%+v runs=%d pulls=%d", first, second, result, provider.runs, pulls.calls)
	}
}

func TestWorkerAppliesAuthorizedReadOnlyModelPatch(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: &patchProvider{}, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "owner/repo:patch", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if content := strings.TrimSpace(gitOutput(t, remote, "show", result.Branch+":src/value.txt")); content != "fixed by model patch" {
		t.Fatalf("authorized model patch was not committed and pushed: %q", content)
	}
	if !containsDecision(lifecycle.decisions, string(automation.ActionApplyPatch)+":true") {
		t.Fatalf("patch application authority was not audited: %v", lifecycle.decisions)
	}
}

func TestWorkerAppliesCorrectivePatchOverDirtyFailedAttempt(t *testing.T) {
	t.Parallel()
	repository, remote := seedGitRepository(t)
	configPath := filepath.Join(repository, "visual-hive.config.yaml")
	if err := os.WriteFile(configPath, []byte("serve: npm run preview\ntextMustNotExist: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "visual-hive.config.yaml")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add visual config")
	runCommand(t, repository, "git", "push", "origin", "main")

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "api-500")
	branch := "hive/repair-api-500-a2"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	failedPatch := "serve: node scripts/testing/start-lhci-server.mjs\ntextMustNotExist:\n  - visual-hive api-500 mutation\n"
	if err := os.WriteFile(filepath.Join(worktree, "visual-hive.config.yaml"), []byte(failedPatch), 0o600); err != nil {
		t.Fatal(err)
	}
	correction := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n--- a/visual-hive.config.yaml\n+++ b/visual-hive.config.yaml\n@@ -1,3 +1,3 @@\n-serve: node scripts/testing/start-lhci-server.mjs\n+serve: npm run preview\n textMustNotExist:\n   - visual-hive api-500 mutation\n"
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:api-500"
	if err := state.Put(Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 2,
		Branch: branch, Worktree: worktree, Stage: StageModelComplete, Provider: "patch-model",
		LifecycleStarted: true, ModelSummary: "restore nominal serve command", ModelPatch: correction,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"visual-hive.config.yaml"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: &patchProvider{}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Status: visualhive.StatusRepairRunning,
		Title: "Repair api-500 mutation survivor", Body: "The api-500 operator survived.", IssueKind: "mutation_survivor",
		Severity: "high", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9", RepairAttempts: 1,
	}
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if result.PRNumber != 17 || !result.Resumed {
		t.Fatalf("corrective revision did not finish the existing attempt: %+v", result)
	}
	content := gitOutput(t, remote, "show", result.Branch+":visual-hive.config.yaml")
	if strings.Contains(content, "start-lhci-server.mjs") || !strings.Contains(content, "visual-hive api-500 mutation") {
		t.Fatalf("corrective revision did not preserve only the valid cumulative change: %q", content)
	}
}

func containsDecision(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestValidateChangedFilesRejectsSensitivePaths(t *testing.T) {
	for _, file := range []string{
		".github/workflows/test.yml",
		"visual-hive.baselines/a.png",
		"src/auth/token.ts",
		"deploy/app.yml",
		"src/components/Button.snapshot.tsx",
		"e2e/dashboard.spec.ts-snapshots/home.png",
		"tests/foo.snap",
		"tests/fixtures/render.png",
		"testdata/expected/home.webp",
	} {
		if err := validateChangedFiles([]string{file}, []string{"**"}); err == nil {
			t.Fatalf("expected %s to require review", file)
		}
	}
	for _, file := range []string{"public/logo.png", "src/assets/product-photo.jpg", "src/snapshot-service.ts", "src/snapshot/serializer.go", "src/screenshots/service.ts", "artifacts/screenshots/failure.txt"} {
		if err := validateChangedFiles([]string{file}, []string{"**"}); err != nil {
			t.Fatalf("ordinary public raster asset %s should remain repair-eligible: %v", file, err)
		}
	}
}

func TestTestAdequacyScopeIsCentrallyTestOnly(t *testing.T) {
	finding := visualhive.FindingLifecycle{IssueKind: "test_adequacy_gap"}
	if err := validateFindingScope(finding, []string{"tests/safe-default.test.js"}); err != nil {
		t.Fatalf("focused test file should be allowed: %v", err)
	}
	for _, files := range [][]string{{"src/App.tsx"}, {"package.json"}, {"visual-hive.config.yaml"}, {"tests/safe.test.js", "src/App.tsx"}} {
		if err := validateFindingScope(finding, files); err == nil {
			t.Fatalf("test adequacy scope allowed non-test files: %v", files)
		}
	}
	prompt := repairPrompt(finding, "verified evidence", "", "", "")
	if !strings.Contains(prompt, "test-adequacy repair") || !strings.Contains(prompt, "Change only focused files") || !strings.Contains(prompt, "fileURLToPath") {
		t.Fatalf("test-only constraint missing from repair prompt: %s", prompt)
	}
}

func TestRepositoryTestRepairPromptAllowsOnlyReviewedDependencyMetadata(t *testing.T) {
	finding := visualhive.FindingLifecycle{
		IssueKind:         visualhive.RepositoryTestFailureKind,
		Title:             "[Hive] Repository test failed: npm --prefix dashboard run test:all",
		ValidationCommand: "npm --prefix dashboard run test:all",
	}
	prompt := repairPrompt(finding, "- source=repository_test_plan status=failed", "", "", "")
	for _, required := range []string{
		"Dependency manifest and lockfile changes are permitted only when they are the smallest fix",
		"Never delete, rename, skip, or weaken the failing script",
		"held for exact-head human approval",
		"do not add install hooks, registries, credentials, or ignore rules",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("repository-test repair prompt lost guardrail %q: %s", required, prompt)
		}
	}
	if strings.Contains(prompt, "infrastructure, dependencies, or visual baselines") {
		t.Fatal("repository-test prompt retained the unconditional dependency-edit prohibition")
	}
}

func TestAPI500RepairPromptUsesFirstPartyMutationMarker(t *testing.T) {
	finding := visualhive.FindingLifecycle{Title: "Strengthen tests for surviving mutation api-500", IssueKind: "missing_visual_coverage"}
	prompt := repairPrompt(finding, "verified evidence", "", "", "")
	for _, expected := range []string{"visual-hive api-500 mutation", "textMustNotExist", "Do not change the nominal server/data harness", "remove any added `[data-testid='api-data-area']`", "matching `mustExist`"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("api-500 repair guidance missing %q: %s", expected, prompt)
		}
	}
}

func TestRepairPromptIncludesCumulativeRevisionDiff(t *testing.T) {
	finding := visualhive.FindingLifecycle{Title: "Strengthen tests for surviving mutation api-500", IssueKind: "missing_visual_coverage"}
	diff := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n--- a/visual-hive.config.yaml\n+++ b/visual-hive.config.yaml\n@@ -1 +1 @@\n-serve: npm run preview\n+serve: node scripts/testing/start-lhci-server.mjs\n"
	prompt := repairPrompt(finding, "verified evidence", "prior response", diff, "")
	for _, expected := range []string{"Current cumulative uncommitted repair diff", "start-lhci-server.mjs", "applied on top of this exact state", "do not repeat changes already present", "removal of an unsafe prior change"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("cumulative revision guidance missing %q: %s", expected, prompt)
		}
	}
}

func TestAPI500PatchSemanticsRejectHarnessAndSelectorWorkarounds(t *testing.T) {
	finding := visualhive.FindingLifecycle{Title: "Strengthen tests for surviving mutation api-500"}
	for _, patch := range []string{
		"diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n--- a/visual-hive.config.yaml\n+++ b/visual-hive.config.yaml\n@@ -1 +1 @@\n-serve: npm run preview\n+serve: node scripts/testing/start-lhci-server.mjs\n",
		"diff --git a/src/App.tsx b/src/App.tsx\n--- a/src/App.tsx\n+++ b/src/App.tsx\n@@ -1 +1 @@\n-<main>\n+<main data-testid=\"api-data-area\">\n",
	} {
		if err := validateFindingPatchSemantics(finding, patch); err == nil {
			t.Fatalf("unsafe api-500 workaround was accepted: %s", patch)
		}
	}
	correctiveServeRevert := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n--- a/visual-hive.config.yaml\n+++ b/visual-hive.config.yaml\n@@ -1 +1 @@\n-serve: node scripts/testing/start-lhci-server.mjs\n+serve: npm --prefix dashboard run preview -- --port 4173 --strictPort\n"
	if err := validateFindingPatchSemantics(finding, correctiveServeRevert); err != nil {
		t.Fatalf("corrective nominal serve restoration was rejected: %v", err)
	}
	safe := "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n--- a/visual-hive.config.yaml\n+++ b/visual-hive.config.yaml\n@@ -1 +1,2 @@\n-textMustNotExist: []\n+textMustNotExist:\n+  - visual-hive api-500 mutation\n"
	if err := validateFindingPatchSemantics(finding, safe); err != nil {
		t.Fatalf("marker assertion was rejected: %v", err)
	}
}

func TestResumedModelCompleteAttemptRevalidatesSavedPatchSemantics(t *testing.T) {
	finding := visualhive.FindingLifecycle{Title: "Strengthen tests for surviving mutation api-500"}
	attempt := Attempt{Stage: StageModelComplete, ModelPatch: "diff --git a/visual-hive.config.yaml b/visual-hive.config.yaml\n--- a/visual-hive.config.yaml\n+++ b/visual-hive.config.yaml\n@@ -1 +1 @@\n-serve: npm run preview\n+serve: node scripts/testing/start-lhci-server.mjs\n"}
	if err := validateAttemptPatchSemantics(context.Background(), finding, attempt, []string{"visual-hive.config.yaml"}); err == nil || !strings.Contains(err.Error(), "must not change") {
		t.Fatalf("resumed already-applied patch bypassed semantic validation: %v", err)
	}
}

func TestNestedWorkspaceRepairPatternsMatchRecursively(t *testing.T) {
	for _, test := range []struct {
		pattern string
		file    string
	}{
		{"**/src/**", "dashboard/src/components/ProjectDashboardView.tsx"},
		{"**/tests/**", "packages/api/tests/contract_test.py"},
		{"**/*.test.*", "dashboard/src/App.test.tsx"},
		{"src/**", "src/main.ts"},
		{"**", "dashboard/src/main.ts"},
	} {
		if !matchPathPattern(test.pattern, test.file) {
			t.Fatalf("pattern %q did not match %q", test.pattern, test.file)
		}
	}
	for _, file := range []string{"dashboard/source/main.ts", "src-other/main.ts"} {
		if matchPathPattern("**/src/**", file) {
			t.Fatalf("nested source pattern unexpectedly matched %q", file)
		}
	}
}

func TestCheckpointRetryableFailureIncludesRejectionFeedback(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{RepositoryFingerprint: "owner/repo:retry", Stage: StageModelComplete, ModelSummary: "first patch", ModelPatch: "diff"}
	err = checkpointRetryableFailure(store, &attempt, fmt.Errorf("dashboard/src/App.tsx is outside the configured repair allowlist"))
	if !IsRetryableAttemptError(err) {
		t.Fatalf("failure should remain explicitly retryable: %v", err)
	}
	saved, ok := store.Get(attempt.RepositoryFingerprint)
	if !ok || saved.Stage != StageNoChange || saved.ModelPatch != "" || !strings.Contains(saved.ModelSummary, "outside the configured repair allowlist") || !strings.Contains(saved.ModelSummary, "Do not repeat") {
		t.Fatalf("repair rejection feedback was not persisted: %+v", saved)
	}
}

func TestCheckpointLocalValidationFailureCreatesBoundedRetryState(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{
		RepositoryFingerprint: "owner/repo:test-gap", Stage: StageModelComplete,
		ModelSummary: "first patch", ModelPatch: "diff --git a/test/x.test.js b/test/x.test.js",
	}
	if err := checkpointLocalValidationFailure(store, &attempt, fmt.Errorf("expected 2, got 1")); err != nil {
		t.Fatal(err)
	}
	saved, ok := store.Get(attempt.RepositoryFingerprint)
	if !ok || saved.Stage != StageNoChange || saved.ModelPatch != "" || !strings.Contains(saved.ModelSummary, "expected 2, got 1") {
		t.Fatalf("local validation failure was not persisted for a bounded retry: %+v", saved)
	}
}

func TestLimitedBufferPreservesValidationFailureTail(t *testing.T) {
	var output limitedBuffer
	_, _ = output.Write([]byte("command-start\n" + strings.Repeat("progress\n", 3_000)))
	_, _ = output.Write([]byte("assertion failure: expected ready, got loading\n"))
	value := output.String()
	if !strings.Contains(value, "command-start") || !strings.Contains(value, "assertion failure: expected ready, got loading") || !strings.Contains(value, "bounded output omitted") {
		t.Fatalf("bounded output did not preserve diagnostic head and tail: %q", value)
	}
}

func TestPrepareWorktreeCleansOnlyPersistedFailedAttemptBranch(t *testing.T) {
	repository, remote := seedGitRepository(t)
	runCommand(t, repository, "git", "config", "--local", "core.autocrlf", "true")
	worktree := filepath.Join(t.TempDir(), "worktrees", "attempt")
	if err := prepareWorktree(context.Background(), repository, worktree, "hive/repair-test-a1", "main", "", remote); err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(worktree, "test", "failed.test.js")
	if err := os.MkdirAll(filepath.Dir(dirty), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("failed patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorktree(context.Background(), repository, worktree, "hive/repair-test-a2", "main", "wrong-branch", remote); err == nil {
		t.Fatal("dirty worktree must not be cleaned without the exact persisted failed branch")
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("unauthorized cleanup changed the worktree: %v", err)
	}
	if err := prepareWorktree(context.Background(), repository, worktree, "hive/repair-test-a2", "main", "hive/repair-test-a1", remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dirty); !os.IsNotExist(err) {
		t.Fatalf("failed attempt patch survived scoped cleanup: %v", err)
	}
	if branch := strings.TrimSpace(gitOutput(t, worktree, "branch", "--show-current")); branch != "hive/repair-test-a2" {
		t.Fatalf("worktree branch = %q, want attempt 2", branch)
	}
	if autocrlf := strings.TrimSpace(gitOutput(t, worktree, "config", "--get", "core.autocrlf")); autocrlf != "true" {
		t.Fatalf("repair controller persisted a line-ending config mutation: got %q want original true", autocrlf)
	}
}

func TestNormalizeTrackedLineEndingsPreservesIndexAndPatchSemantics(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktree := filepath.Join(t.TempDir(), "worktrees", "line-endings")
	if err := prepareWorktree(context.Background(), repository, worktree, "hive/repair-eol-a1", "main", "", remote); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(worktree, "src", "value.txt")
	if err := os.WriteFile(file, []byte("broken\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := normalizeTrackedLineEndings(context.Background(), worktree); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "broken\n" {
		t.Fatalf("tracked text was not normalized to its LF index representation: %q", data)
	}
	if status := strings.TrimSpace(gitOutput(t, worktree, "status", "--short")); status != "" {
		t.Fatalf("line-ending normalization changed repository semantics: %s", status)
	}
}

func seedGitRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runCommand(t, root, "git", "init", "--bare", remote)
	seed := filepath.Join(root, "seed")
	runCommand(t, root, "git", "init", "-b", "main", seed)
	if err := os.MkdirAll(filepath.Join(seed, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "src", "value.txt"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, seed, "git", "add", ".")
	runCommand(t, seed, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runCommand(t, seed, "git", "remote", "add", "origin", remote)
	runCommand(t, seed, "git", "push", "-u", "origin", "main")
	clone := filepath.Join(root, "clone")
	// Test fixtures must not inherit the operator's global core.autocrlf value:
	// contained repair Git commands intentionally ignore user configuration.
	runCommand(t, root, "git", "-c", "core.autocrlf=false", "clone", "--branch", "main", remote, clone)
	return clone, remote
}

func runCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
