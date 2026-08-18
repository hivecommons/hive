package integrated

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyManagedCheckoutMigrationAcceptsRealStandardCloneAndIsIdempotent(t *testing.T) {
	stateDir, checkout, proof := createLegacyManagedCheckout(t)
	legacyTestGit(t, checkout, "branch", "hive/setup")
	legacyTestGit(t, checkout, "config", "branch.hive/setup.remote", "origin")
	legacyTestGit(t, checkout, "config", "branch.hive/setup.merge", "refs/heads/main")
	legacyTestGit(t, checkout, "branch", "hive/repair-332456fc7a61-a6")
	legacyTestGit(t, checkout, "config", "branch.hive/repair-332456fc7a61-a6.remote", "origin")
	legacyTestGit(t, checkout, "config", "branch.hive/repair-332456fc7a61-a6.merge", "refs/heads/main")

	exists, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof)
	if err != nil || !exists {
		t.Fatalf("real standard legacy clone was not migrated: exists=%t err=%v", exists, err)
	}
	marker := filepath.Join(checkout, ".git", managedCheckoutOwnerFile)
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), `"repository": "owner/repo"`) {
		t.Fatalf("exact ownership marker was not written: %q err=%v", data, err)
	}
	if exists, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo"); err != nil || !exists {
		t.Fatalf("migrated checkout was not idempotently accepted: exists=%t err=%v", exists, err)
	}
	if proof.Config.StateDir != stateDir {
		t.Fatalf("test proof changed state root: %s", proof.Config.StateDir)
	}
}

func TestLegacyManagedCheckoutMigrationRejectsGitExecutionControls(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "include",
			mutate: func(t *testing.T, checkout string) {
				legacyTestGit(t, checkout, "config", "include.path", filepath.Join(t.TempDir(), "attacker.conf"))
			},
		},
		{
			name: "hooks-path",
			mutate: func(t *testing.T, checkout string) {
				legacyTestGit(t, checkout, "config", "core.hooksPath", t.TempDir())
			},
		},
		{
			name: "remote-helper",
			mutate: func(t *testing.T, checkout string) {
				legacyTestGit(t, checkout, "remote", "set-url", "origin", "ext::sh -c attacker")
			},
		},
		{
			name: "active-hook",
			mutate: func(t *testing.T, checkout string) {
				if err := os.WriteFile(filepath.Join(checkout, ".git", "hooks", "pre-fetch"), []byte("attacker"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, checkout, proof := createLegacyManagedCheckout(t)
			test.mutate(t, checkout)
			if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof); err == nil {
				t.Fatal("legacy checkout with an execution control was migrated")
			}
			if _, err := os.Lstat(filepath.Join(checkout, ".git", managedCheckoutOwnerFile)); !os.IsNotExist(err) {
				t.Fatalf("refused checkout received an ownership marker: %v", err)
			}
		})
	}
}

func TestLegacyManagedCheckoutMigrationRequiresExactDurableAndLiveBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*legacyManagedCheckoutProof)
	}{
		{name: "repository", mutate: func(proof *legacyManagedCheckoutProof) { proof.Config.Repository = "other/repo" }},
		{name: "repository-id", mutate: func(proof *legacyManagedCheckoutProof) { proof.Config.RepositoryID = "124" }},
		{name: "live-id", mutate: func(proof *legacyManagedCheckoutProof) { proof.LiveRepositoryID = "124" }},
		{name: "state-dir", mutate: func(proof *legacyManagedCheckoutProof) { proof.Config.StateDir += "-other" }},
		{name: "checkout-dir", mutate: func(proof *legacyManagedCheckoutProof) { proof.Config.CheckoutDir += "-other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, checkout, proof := createLegacyManagedCheckout(t)
			test.mutate(&proof)
			if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof); err == nil {
				t.Fatal("legacy checkout with a mismatched proof was migrated")
			}
			if _, err := os.Lstat(filepath.Join(checkout, ".git", managedCheckoutOwnerFile)); !os.IsNotExist(err) {
				t.Fatalf("mismatched checkout received an ownership marker: %v", err)
			}
		})
	}
}

func TestLegacyManagedCheckoutMigrationAcceptsOnlyExactHiveRepairWorktreeControl(t *testing.T) {
	t.Run("state-owned", func(t *testing.T) {
		stateDir, checkout, proof := createLegacyManagedCheckout(t)
		id := "332456fc7a61"
		worktree := filepath.Join(stateDir, "repair", "worktrees", id)
		if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
			t.Fatal(err)
		}
		legacyTestGit(t, checkout, "worktree", "add", "-b", "hive/repair-"+id+"-a6", worktree, "main")
		if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof); err != nil {
			t.Fatalf("exact state-owned Hive repair worktree was rejected: %v", err)
		}
	})

	t.Run("external-gitdir", func(t *testing.T) {
		stateDir, checkout, proof := createLegacyManagedCheckout(t)
		id := "332456fc7a61"
		worktree := filepath.Join(stateDir, "repair", "worktrees", id)
		if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
			t.Fatal(err)
		}
		legacyTestGit(t, checkout, "worktree", "add", "-b", "hive/repair-"+id+"-a6", worktree, "main")
		metadataGitdir := filepath.Join(checkout, ".git", "worktrees", id, "gitdir")
		if err := os.WriteFile(metadataGitdir, []byte(filepath.Join(t.TempDir(), ".git")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof); err == nil {
			t.Fatal("legacy repair metadata pointing outside exact state was migrated")
		}
	})
}

func createLegacyManagedCheckout(t *testing.T) (string, string, legacyManagedCheckoutProof) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyTestGit(t, source, "init", "-b", "main")
	legacyTestGit(t, source, "config", "user.name", "Hive Test")
	legacyTestGit(t, source, "config", "user.email", "hive-test@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyTestGit(t, source, "add", "README.md")
	legacyTestGit(t, source, "commit", "-m", "initial")

	stateDir := filepath.Join(root, "hive-state")
	checkout := filepath.Join(stateDir, "integrated", "checkouts", "owner-repo")
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyTestGit(t, filepath.Dir(checkout), "clone", source, checkout)
	legacyTestGit(t, checkout, "config", "core.autocrlf", "false")
	legacyTestGit(t, checkout, "remote", "set-url", "origin", "https://github.com/owner/repo.git")
	proof := legacyManagedCheckoutProof{
		Config: Config{
			SchemaVersion: ConfigSchema,
			Repository:    "owner/repo",
			RepositoryID:  "123",
			DefaultBranch: "main",
			StateDir:      stateDir,
			CheckoutDir:   checkout,
		},
		LiveRepositoryID: "123",
	}
	return stateDir, checkout, proof
}

func legacyTestGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}
