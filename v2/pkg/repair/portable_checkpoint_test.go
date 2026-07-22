package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/checkpoint"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestValidatedPortableCheckpointResumesOnFreshRunner(t *testing.T) {
	ctx := context.Background()
	repository, remote := seedGitRepository(t)
	stateDir := filepath.Join(t.TempDir(), "durable-state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:portable-validated"
	branch := repairBranchName(fingerprint, 1, 1)
	originalRoot := filepath.Join(t.TempDir(), "original-worktrees")
	originalWorktree := filepath.Join(originalRoot, shortFingerprint(fingerprint))
	if err := prepareWorktree(ctx, repository, originalWorktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originalWorktree, "src", "value.txt"), []byte("fixed on validated runner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Recurrence: 1, Attempt: 1,
		AttemptCounted: true, LifecycleStarted: true, Branch: branch, Worktree: originalWorktree,
		Stage: StageValidated, Provider: "test-model", ChangedFiles: []string{"src/value.txt"}, StartedAt: time.Now().UTC(),
	}
	if err := sealCandidateFromWorktree(ctx, &attempt); err != nil {
		t.Fatal(err)
	}
	sealingWorker := &Worker{State: state}
	if err := sealingWorker.guardCandidateTree(ctx, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := sealingWorker.persistPortableRepairBundle(ctx, &attempt, StageValidated); err != nil {
		t.Fatal(err)
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	bundlePath, err := state.portableRepairBundlePath(attempt.PortableBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(bundlePath); err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > portableRepairBundleMaxBytes {
		t.Fatalf("validated portable bundle is not bounded: info=%v err=%v", info, err)
	}

	freshRepository := cloneFreshRepairRepository(t, repository, originalWorktree, remote)
	freshRoot := filepath.Join(t.TempDir(), "fresh-worktrees")
	state, err = NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config: Config{
			RepositoryDir: freshRepository, WorktreeRoot: freshRoot, BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, CommandTimeout: time.Minute,
		},
		Provider: &healthFailureProvider{fail: true}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: pulls,
	}
	finding := portableRecoveryFinding(fingerprint, branch)
	result, err := worker.Run(ctx, finding)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := state.Get(fingerprint)
	if !ok || persisted.Stage != StagePROpen || result.PRNumber != 17 || pulls.calls != 1 {
		t.Fatalf("validated checkpoint did not complete on the fresh runner: result=%+v attempt=%+v calls=%d", result, persisted, pulls.calls)
	}
	if persisted.Worktree != filepath.Join(freshRoot, shortFingerprint(fingerprint)) || hasPortableRepairBundle(persisted) {
		t.Fatalf("validated checkpoint did not rebind and retire its portable artifact: %+v", persisted)
	}
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatalf("validated portable bundle was not deleted after push: %v", err)
	}
}

func TestCommittedPortableCheckpointResumesBeforePushOnFreshRunner(t *testing.T) {
	repository, remote := seedGitRepository(t)
	stateDir := filepath.Join(t.TempDir(), "durable-state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	originalRoot := filepath.Join(t.TempDir(), "original-worktrees")
	fingerprint := "owner/repo:portable-committed"
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: originalRoot, BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: &fakeProvider{}, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Recurrences: 1,
		Status: visualhive.StatusIssueOpen, Title: "Repair portable committed value", Body: "Value is broken.",
		IssueKind: "functional", Severity: "high", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	stopBeforePush := errors.New("simulated hosted runner stop before push")
	interrupted := checkpoint.WithCallback(context.Background(), func(_ context.Context, detail string) error {
		if detail == "repair Git push" {
			return stopBeforePush
		}
		return nil
	})
	if _, err := worker.Run(interrupted, finding); err == nil || !strings.Contains(err.Error(), stopBeforePush.Error()) {
		t.Fatalf("repair did not stop at the committed-before-push boundary: %v", err)
	}
	attempt, ok := state.Get(fingerprint)
	if !ok || attempt.Stage != StageCommitted || !validGitCommitSHA(attempt.CommitSHA) || !hasPortableRepairBundle(attempt) {
		t.Fatalf("committed interruption was not durably portable: %+v", attempt)
	}
	bundlePath, err := state.portableRepairBundlePath(attempt.PortableBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	originalWorktree := attempt.Worktree
	freshRepository := cloneFreshRepairRepository(t, repository, originalWorktree, remote)
	freshRoot := filepath.Join(t.TempDir(), "fresh-worktrees")
	state, err = NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	pulls := &fakePRClient{state: state}
	resumedWorker := &Worker{
		Config: Config{
			RepositoryDir: freshRepository, WorktreeRoot: freshRoot, BaseBranch: "main", ExpectedRemoteURL: remote,
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, CommandTimeout: time.Minute,
		},
		Provider: &healthFailureProvider{fail: true}, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding.RepairAttempts = 1
	finding.Status = visualhive.StatusRepairRunning
	finding.Branch = attempt.Branch
	result, err := resumedWorker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := state.Get(fingerprint)
	if !ok || persisted.Stage != StagePROpen || result.CommitSHA != attempt.CommitSHA || pulls.calls != 1 {
		t.Fatalf("committed checkpoint did not finish exactly on the fresh runner: result=%+v attempt=%+v calls=%d", result, persisted, pulls.calls)
	}
	remoteHead, err := remoteRepairBranchHead(context.Background(), filepath.Join(freshRoot, shortFingerprint(fingerprint)), remote, attempt.Branch)
	if err != nil || remoteHead != attempt.CommitSHA {
		t.Fatalf("fresh runner did not push the exact committed checkpoint: remote=%s want=%s err=%v", remoteHead, attempt.CommitSHA, err)
	}
	if hasPortableRepairBundle(persisted) {
		t.Fatalf("committed portable bundle metadata survived the push: %+v", persisted)
	}
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatalf("committed portable bundle was not deleted after push: %v", err)
	}
}

func portableRecoveryFinding(fingerprint, branch string) visualhive.FindingLifecycle {
	return visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Recurrences: 1, RepairAttempts: 1,
		Status: visualhive.StatusRepairRunning, Branch: branch, Title: "Repair portable validated value", Body: "Value is broken.",
		IssueKind: "functional", Severity: "high", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
}

func cloneFreshRepairRepository(t *testing.T, repository, worktree, remote string) string {
	t.Helper()
	if _, err := runGit(context.Background(), repository, "worktree", "remove", "--force", worktree); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repository); err != nil {
		t.Fatal(err)
	}
	freshRepository := filepath.Join(t.TempDir(), "fresh-repository")
	if _, err := runGit(context.Background(), filepath.Dir(freshRepository), "clone", remote, freshRepository); err != nil {
		t.Fatal(err)
	}
	return freshRepository
}
