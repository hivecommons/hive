package integrated

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestManagedBranchNamesAndPullHeadsBindRepositoryIdentity(t *testing.T) {
	if got := managedOperationBranch("setup", "12345"); got != "hive/setup-12345" {
		t.Fatalf("managed branch = %q", got)
	}
	sha := "0123456789012345678901234567890123456789"
	if err := verifyManagedPullHead("setup", hivegithub.RepairPullRequest{HeadSHA: sha}, sha); err != nil {
		t.Fatal(err)
	}
	for name, head := range map[string]string{"empty": "", "moved": "1123456789012345678901234567890123456789"} {
		t.Run(name, func(t *testing.T) {
			if err := verifyManagedPullHead("setup", hivegithub.RepairPullRequest{HeadSHA: head}, sha); err == nil {
				t.Fatal("non-exact PR head was accepted")
			}
		})
	}
	trailers := managedCommitTrailers("12345", "setup")
	if !hasExactCommitTrailer(trailers, "Hive-Repository-ID", "12345") || !hasExactCommitTrailer(trailers, "Hive-Operation", "setup") {
		t.Fatal("managed ownership trailers are incomplete")
	}
}

func TestPushManagedBranchRefusesUnownedRemoteBranch(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runIntegratedGit(t, root, "init", "--bare", remote)
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "seed")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Other", "-c", "user.email=other@example.com", "commit", "-m", "unrelated")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	branch := managedOperationBranch("setup", "123")
	runIntegratedGit(t, seed, "switch", "-c", branch)
	writeFixture(t, seed, "unrelated.txt", "not Hive owned")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Other", "-c", "user.email=other@example.com", "commit", "-m", "unrelated branch")
	runIntegratedGit(t, seed, "push", "origin", branch)
	runIntegratedGit(t, seed, "fetch", "origin")
	localSHA := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", "HEAD"))
	err := pushManagedBranch(context.Background(), seed, "owner/repo", branch, "123", "setup", localSHA)
	if err == nil || !strings.Contains(err.Error(), "lacks exact Hive ownership") {
		t.Fatalf("unowned remote managed-name branch was overwritten: %v", err)
	}
}
