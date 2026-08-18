package repair

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitialRepairProtectsConfiguredVisualBaselineWithoutBlockingPublicAssets(t *testing.T) {
	repository := seedBaselineProtectionRepository(t, "public/visual-reference")
	protection, err := InspectVisualBaselineProtection(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !protection.Enforced || !containsFoldedPath(protection.Roots, "public/visual-reference") || !containsFoldedPath(protection.Files, "public/visual-reference/home.png") {
		t.Fatalf("configured baseline protection was not derived exactly: %+v", protection)
	}

	worker := Worker{Config: Config{
		AllowedRepairPaths: []string{"public/**", "docs/**", "e2e/**"},
		BaselineProtection: protection,
	}}
	for _, file := range []string{"public/visual-reference/home.png", "docs/reference-baseline.json", "e2e/dashboard.spec.ts-snapshots/home.png"} {
		if err := worker.validateRepairPaths(context.Background(), repository, []string{file}); err == nil {
			t.Fatalf("initial repair allowed protected baseline path %s", file)
		}
	}
	if err := worker.validateRepairPaths(context.Background(), repository, []string{"public/logo.png"}); err != nil {
		t.Fatalf("ordinary public PNG was broadly treated as a baseline: %v", err)
	}
}

func TestResumedRepairUnionsTrustedAndCandidateVisualBaselineRoots(t *testing.T) {
	repository := seedBaselineProtectionRepository(t, "public/old-reference")
	trusted, err := InspectVisualBaselineProtection(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	config := "visual:\n  snapshotDir: public/new-reference\n"
	if err := os.WriteFile(filepath.Join(repository, "visual-hive.config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := Worker{Config: Config{
		AllowedRepairPaths: []string{"public/**"},
		BaselineProtection: trusted,
	}}

	for _, file := range []string{"public/old-reference/home.png", "public/new-reference/home.png"} {
		err := worker.validateRepairPaths(context.Background(), repository, []string{file})
		if err == nil || !strings.Contains(err.Error(), "protected visual baseline") {
			t.Fatalf("resumed repair did not preserve both baseline roots for %s: %v", file, err)
		}
	}
	if err := worker.validateRepairPaths(context.Background(), repository, []string{"public/logo.png"}); err != nil {
		t.Fatalf("resumed baseline protection blocked unrelated public PNG: %v", err)
	}
}

func TestVisualBaselineProtectionAtCommitIgnoresMutableWorktreeConfig(t *testing.T) {
	repository := seedBaselineProtectionRepository(t, "public/reviewed-reference")
	reviewedCommit := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	writeBaselineProtectionFile(t, repository, "visual-hive.config.yaml", "visual:\n  snapshotDir: public/unreviewed-reference\n")
	writeBaselineProtectionFile(t, repository, "public/unreviewed-reference/home.png", "unreviewed baseline\n")

	protection, err := InspectVisualBaselineProtectionAtCommit(context.Background(), repository, reviewedCommit)
	if err != nil {
		t.Fatal(err)
	}
	if !containsFoldedPath(protection.Roots, "public/reviewed-reference") || containsFoldedPath(protection.Roots, "public/unreviewed-reference") {
		t.Fatalf("exact commit protection consulted mutable worktree config: %+v", protection)
	}
	if !containsFoldedPath(protection.Files, "public/reviewed-reference/home.png") || containsFoldedPath(protection.Files, "public/unreviewed-reference/home.png") {
		t.Fatalf("exact commit protection consulted mutable worktree files: %+v", protection)
	}
}

func seedBaselineProtectionRepository(t *testing.T, snapshotDir string) string {
	t.Helper()
	repository := t.TempDir()
	runCommand(t, repository, "git", "init", "-b", "main")
	writeBaselineProtectionFile(t, repository, "visual-hive.config.yaml", "visual:\n  snapshotDir: "+snapshotDir+"\n")
	writeBaselineProtectionFile(t, repository, snapshotDir+"/home.png", "reviewed baseline\n")
	writeBaselineProtectionFile(t, repository, "public/logo.png", "ordinary asset\n")
	writeBaselineProtectionFile(t, repository, "docs/reference-baseline.json", "{}\n")
	runCommand(t, repository, "git", "add", ".")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "seed visual baselines")
	return repository
}

func writeBaselineProtectionFile(t *testing.T, root, relative, value string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func containsFoldedPath(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(value, expected) {
			return true
		}
	}
	return false
}
