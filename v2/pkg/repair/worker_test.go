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

type fakeProvider struct{ runs int }

func (p *fakeProvider) Name() string                 { return "test-model" }
func (p *fakeProvider) Health(context.Context) error { return nil }
func (p *fakeProvider) Run(_ context.Context, worktree, _ string) error {
	p.runs++
	value := "fixed\n"
	if p.runs > 1 {
		value = "fixed again\n"
	}
	return os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte(value), 0o600)
}

type fakeLifecycle struct {
	branch, sha string
	pr          int
	decisions   []string
}

func (f *fakeLifecycle) MarkRepairStarted(_ string, branch string) error {
	f.branch = branch
	return nil
}
func (f *fakeLifecycle) MarkPROpen(_ string, sha string, number int, _ string) error {
	f.sha, f.pr = sha, number
	return nil
}
func (f *fakeLifecycle) RecordAuthorization(_ string, action string, allowed bool, _ string) {
	f.decisions = append(f.decisions, fmt.Sprintf("%s:%t", action, allowed))
}

type fakePRClient struct{ calls int }

func (f *fakePRClient) UpsertRepairPullRequest(_ context.Context, _, _, _, _, _, _ string) (hivegithub.RepairPullRequest, error) {
	f.calls++
	return hivegithub.RepairPullRequest{Number: 17, URL: "https://example.test/pull/17"}, nil
}

func TestWorkerCreatesRealBranchCommitPushAndPRAndResumes(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:stable-finding", Fingerprint: "stable-finding",
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
	resumed, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || provider.runs != 1 || pulls.calls != 1 {
		t.Fatalf("restart should reuse completed attempt: %+v runs=%d pulls=%d", resumed, provider.runs, pulls.calls)
	}
}

func TestWorkerDeniesModelAtLowerACMMBeforeRun(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config:   Config{RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", Policy: automation.Policy{ACMMLevel: 2, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}}},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{},
	}
	_, err := worker.Run(context.Background(), visualhive.FindingLifecycle{Repository: "owner/repo", RepositoryFingerprint: "fp", IssueNumber: 1, IssueURL: "https://example.test/1"})
	if err == nil || !strings.Contains(err.Error(), "denied") || provider.runs != 0 {
		t.Fatalf("expected pre-provider denial, got %v runs=%d", err, provider.runs)
	}
}

func TestWorkerRevisesTheSameBranchAndPullRequest(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:revision", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	finding.Status, finding.RepairAttempts = visualhive.StatusNeedsRevision, 1
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch != second.Branch || first.PRNumber != second.PRNumber || pulls.calls != 2 || provider.runs != 2 {
		t.Fatalf("revision created duplicate lifecycle objects: first=%+v second=%+v pulls=%d runs=%d", first, second, pulls.calls, provider.runs)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	if attempt.Attempt != 2 || attempt.Stage != StagePROpen {
		t.Fatalf("revision attempt was not persisted: %+v", attempt)
	}
}

func TestValidateChangedFilesRejectsSensitivePaths(t *testing.T) {
	for _, file := range []string{".github/workflows/test.yml", "visual-hive.baselines/a.png", "src/auth/token.ts", "deploy/app.yml"} {
		if err := validateChangedFiles([]string{file}, []string{"**"}); err == nil {
			t.Fatalf("expected %s to require review", file)
		}
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
	runCommand(t, root, "git", "clone", "--branch", "main", remote, clone)
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
