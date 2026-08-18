package repair

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairRefreshRejectsProtectedVisualBaselinesWithoutBlockingPublicAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if _, err := runGit(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "visual-hive.config.yaml", "visual:\n  snapshotDir: public/visual-reference\n")
	writeRefreshFixture(t, worktree, "public/visual-reference/home.png", "reviewed baseline\n")
	writeRefreshFixture(t, worktree, "public/logo.png", "ordinary asset\n")
	commitRefreshFixture(t, worktree, "seed visual baselines")
	protection, err := InspectVisualBaselineProtection(ctx, worktree)
	if err != nil {
		t.Fatal(err)
	}
	head := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	request := RefreshedBranchCheckpointRequest{
		Worktree: worktree, Branch: "hive/repair-baseline-a1", RepositoryID: "123", ExpectedRemoteURL: remote,
		BaselineProtection: protection, RemoteHeadSHA: head, BaseBranch: "main", BaseSHA: head,
		ExpectedChangedFiles: []string{"public/visual-reference/home.png"},
	}
	if err := validateRefreshedBranchRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "protected visual baseline") {
		t.Fatalf("refresh accepted configured visual baseline contribution: %v", err)
	}
	request.ExpectedChangedFiles = []string{"e2e/dashboard.spec.ts-snapshots/home.png"}
	if err := validateRefreshedBranchRequest(ctx, request); err == nil || !strings.Contains(err.Error(), "restricted path") {
		t.Fatalf("refresh accepted Playwright snapshot contribution: %v", err)
	}
	request.ExpectedChangedFiles = []string{"public/logo.png"}
	if err := validateRefreshedBranchRequest(ctx, request); err != nil {
		t.Fatalf("refresh broadly blocked ordinary public PNG: %v", err)
	}
}

func TestPrepareRefreshProtectsBaselineRootDeclaredOnlyByExactRemoteHead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if _, err := runGit(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "visual-hive.config.yaml", "visual:\n  snapshotDir: public/reviewed-reference\n")
	writeRefreshFixture(t, worktree, "public/reviewed-reference/home.png", "reviewed\n")
	commitRefreshFixture(t, worktree, "seed reviewed baseline")
	base := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	trusted, err := InspectVisualBaselineProtection(ctx, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	branch := "hive/repair-new-baseline-a1"
	if _, err := runGit(ctx, worktree, "checkout", "-b", branch); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "visual-hive.config.yaml", "visual:\n  snapshotDir: public/new-reference\n")
	writeRefreshFixture(t, worktree, "public/new-reference/home.png", "unreviewed\n")
	commitRefreshFixture(t, worktree, "fix: substitute baseline\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	remoteHead := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", remoteHead+":refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	// The mutable worktree no longer contains the hostile candidate. Protection
	// must still come from the fetched exact remote commit.
	if _, err := runGit(ctx, worktree, "reset", "--hard", base); err != nil {
		t.Fatal(err)
	}
	request := RefreshedBranchCheckpointRequest{
		Worktree: worktree, Branch: branch, RepositoryID: "123", ExpectedRemoteURL: remote,
		BaselineProtection: trusted, RemoteHeadSHA: remoteHead, BaseBranch: "main", BaseSHA: base,
		ExpectedChangedFiles: []string{"public/new-reference/home.png", "visual-hive.config.yaml"},
	}
	if _, err := PrepareRefreshedRepairBranch(ctx, request); err == nil || !strings.Contains(err.Error(), "protected visual baseline") {
		t.Fatalf("refresh accepted baseline root declared only by exact remote head: %v", err)
	}
	if got, err := remoteRepairBranchHead(ctx, worktree, remote, branch); err != nil || got != remoteHead {
		t.Fatalf("rejected exact-head baseline changed remote state: head=%s err=%v", got, err)
	}
}

func TestPrepareAndPushRefreshedRepairBranchUsesExactLocalBaseAndLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if _, err := runGit(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "README.md", "base\n")
	commitRefreshFixture(t, worktree, "initial")
	if _, err := runGit(ctx, worktree, "checkout", "-b", "hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "tests/proof.test.ts", "test\n")
	commitRefreshFixture(t, worktree, "fix: proof\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	oldHead := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", oldHead+":refs/heads/hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "base.txt", "advanced\n")
	commitRefreshFixture(t, worktree, "advance base")
	if _, err := runGit(ctx, worktree, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "merge", "--no-ff", "main", "-m", "GitHub Update Branch merge"); err != nil {
		t.Fatal(err)
	}
	githubMerge := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "reset", "--hard", oldHead); err != nil {
		t.Fatal(err)
	}

	request := RefreshedBranchCheckpointRequest{
		Worktree: worktree, Branch: "hive/repair-proof-a1", RepositoryID: "123", ExpectedRemoteURL: remote, RemoteHeadSHA: oldHead,
		BaseBranch: "main", BaseSHA: refreshGitOutput(t, worktree, "rev-parse", "main"), ExpectedChangedFiles: []string{"tests/proof.test.ts"},
		ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check", "HEAD"}}},
	}
	checkpoint, err := PrepareRefreshedRepairBranch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if remoteHead, err := remoteRepairBranchHead(ctx, worktree, remote, request.Branch); err != nil || remoteHead != oldHead {
		t.Fatalf("local preparation mutated remote head: head=%s err=%v", remoteHead, err)
	}
	if err := PushRefreshedRepairBranchExact(ctx, request, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := PushRefreshedRepairBranchExact(ctx, request, checkpoint); err != nil {
		t.Fatalf("crash-resumed exact push was not idempotent: %v", err)
	}
	if err := VerifyRefreshedRepairBranch(ctx, request, checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpointSHA := checkpoint.HeadSHA
	if checkpointSHA == oldHead || checkpointSHA == githubMerge || !validGitCommitSHA(checkpointSHA) {
		t.Fatalf("invalid owned checkpoint %q", checkpointSHA)
	}
	parents := strings.Fields(refreshGitOutput(t, worktree, "rev-list", "--parents", "-n", "1", checkpointSHA))
	if len(parents) != 3 || parents[1] != oldHead || parents[2] != request.BaseSHA {
		t.Fatalf("checkpoint parents = %v, want head=%s base=%s", parents, oldHead, request.BaseSHA)
	}
	message := refreshGitOutput(t, worktree, "show", "-s", "--format=%B", checkpointSHA)
	if !hasExactRepairTrailer(message, "Hive-Repository-ID", "123") || !hasExactRepairTrailer(message, "Hive-Operation", "repair") {
		t.Fatalf("checkpoint lacks ownership trailers: %s", message)
	}
	remoteHead, err := remoteRepairBranchHead(ctx, worktree, remote, "hive/repair-proof-a1")
	if err != nil || remoteHead != checkpointSHA {
		t.Fatalf("remote head = %s, want %s: %v", remoteHead, checkpointSHA, err)
	}
	if err := pushRepairBranchExact(ctx, worktree, remote, "hive/repair-proof-a1", checkpointSHA, "123", "repair"); err != nil {
		t.Fatalf("future owned repair lease rejected refreshed tip: %v", err)
	}
}

func TestPrepareRefreshedRepairBranchConflictLeavesRemoteUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if _, err := runGit(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "shared.txt", "initial\n")
	commitRefreshFixture(t, worktree, "initial")
	if _, err := runGit(ctx, worktree, "checkout", "-b", "hive/repair-conflict-a1"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "shared.txt", "repair\n")
	commitRefreshFixture(t, worktree, "fix: repair\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	head := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", head+":refs/heads/hive/repair-conflict-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "shared.txt", "base\n")
	commitRefreshFixture(t, worktree, "advance conflicting base")
	base := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "hive/repair-conflict-a1"); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRefreshedRepairBranch(ctx, RefreshedBranchCheckpointRequest{
		Worktree: worktree, Branch: "hive/repair-conflict-a1", RepositoryID: "123", ExpectedRemoteURL: remote, RemoteHeadSHA: head,
		BaseBranch: "main", BaseSHA: base, ExpectedChangedFiles: []string{"shared.txt"},
	})
	if err == nil || !IsRepairBranchRefreshConflict(err) {
		t.Fatalf("conflicting exact-base merge was not classified: %v", err)
	}
	remoteHead, remoteErr := remoteRepairBranchHead(ctx, worktree, remote, "hive/repair-conflict-a1")
	if remoteErr != nil || remoteHead != head {
		t.Fatalf("conflict mutated remote repair head: head=%s err=%v", remoteHead, remoteErr)
	}
	if local := refreshGitOutput(t, worktree, "rev-parse", "HEAD"); local != head {
		t.Fatalf("conflict did not restore exact local head: %s", local)
	}
	if dirty := refreshGitOutput(t, worktree, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		t.Fatalf("conflict left tracked worktree state: %q", dirty)
	}
}

func TestPushRefreshedRepairBranchRejectsExactHeadLeaseRace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if _, err := runGit(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "README.md", "base\n")
	commitRefreshFixture(t, worktree, "initial")
	if _, err := runGit(ctx, worktree, "checkout", "-b", "hive/repair-race-a1"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "proof.txt", "repair\n")
	commitRefreshFixture(t, worktree, "fix: repair\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	head := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", head+":refs/heads/hive/repair-race-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "base.txt", "advance\n")
	commitRefreshFixture(t, worktree, "advance")
	base := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "hive/repair-race-a1"); err != nil {
		t.Fatal(err)
	}
	request := RefreshedBranchCheckpointRequest{Worktree: worktree, Branch: "hive/repair-race-a1", RepositoryID: "123", ExpectedRemoteURL: remote, RemoteHeadSHA: head, BaseBranch: "main", BaseSHA: base, ExpectedChangedFiles: []string{"proof.txt"}}
	checkpoint, err := PrepareRefreshedRepairBranch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "--detach", head); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "chore: competing owned checkpoint\n\nHive-Repository-ID: 123\nHive-Operation: repair"); err != nil {
		t.Fatal(err)
	}
	raceHead := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", repairForceLease(request.Branch, head), "origin", raceHead+":refs/heads/"+request.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", request.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "reset", "--hard", checkpoint.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if err := PushRefreshedRepairBranchExact(ctx, request, checkpoint); err == nil || !strings.Contains(err.Error(), "changed before exact refresh push") {
		t.Fatalf("moved exact-head lease was not rejected: %v", err)
	}
	remoteHead, _ := remoteRepairBranchHead(ctx, worktree, remote, request.Branch)
	if remoteHead != raceHead {
		t.Fatalf("lease race overwrote competing remote head: got %s want %s", remoteHead, raceHead)
	}
}

func TestPrepareRefreshedRepairBranchRejectsChangedContributionWithSamePath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktree")
	if _, err := runGit(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "proof.txt", "base\n")
	commitRefreshFixture(t, worktree, "base")
	if _, err := runGit(ctx, worktree, "checkout", "-b", "hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "proof.txt", "repair\n")
	commitRefreshFixture(t, worktree, "fix: proof\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	head := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", head+":refs/heads/hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "base.txt", "advance\n")
	commitRefreshFixture(t, worktree, "advance")
	base := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, worktree, "checkout", "hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	request := RefreshedBranchCheckpointRequest{Worktree: worktree, Branch: "hive/repair-proof-a1", RepositoryID: "123", ExpectedRemoteURL: remote, RemoteHeadSHA: head, BaseBranch: "main", BaseSHA: base, ExpectedChangedFiles: []string{"proof.txt"}}
	checkpoint, err := PrepareRefreshedRepairBranch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := ResetRefreshedRepairBranch(ctx, worktree, head); err != nil {
		t.Fatal(err)
	}
	writeRefreshFixture(t, worktree, "proof.txt", "different repair\n")
	commitRefreshFixture(t, worktree, "fix: changed proof\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	changedHead := refreshGitOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := runGit(ctx, worktree, "push", "--force", "origin", changedHead+":refs/heads/hive/repair-proof-a1"); err != nil {
		t.Fatal(err)
	}
	request.RemoteHeadSHA = changedHead
	request.ExpectedContributionPatchID = checkpoint.ContributionPatchID
	if _, err := PrepareRefreshedRepairBranch(ctx, request); err == nil || !strings.Contains(err.Error(), "patch changed") {
		t.Fatalf("same-path contribution substitution was accepted: %v", err)
	}
}

func writeRefreshFixture(t *testing.T, root, relative, value string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitRefreshFixture(t *testing.T, root, message string) {
	t.Helper()
	// Persist a repository-local identity so later Git operations that create
	// commits (for example, the merge used to model GitHub's update-branch
	// behavior) do not depend on developer or hosted-runner global config.
	if _, err := runGit(context.Background(), root, "config", "--local", "user.name", "Hive Repair Test"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), root, "config", "--local", "user.email", "hive-repair-test@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), root, "add", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), root, "commit", "-m", message); err != nil {
		t.Fatal(err)
	}
}

func refreshGitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	value, err := runGit(context.Background(), root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(value)
}
