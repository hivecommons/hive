package repair

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutonomousTestAdequacyAcceptsOnlyExactAdditiveRunnableCommit(t *testing.T) {
	repository, _ := seedGitRepository(t)
	name := "tests/value.test.ts"
	writeAutonomousTestFixture(t, repository, name, `import { strict as assert } from "node:assert";
test("value remains stable", () => {
  assert.equal("value".toUpperCase(), "VALUE");
});
`)
	if err := validateAdditiveTestFilesInWorktree(context.Background(), repository, []string{name}); err != nil {
		t.Fatalf("valid additive test was rejected before commit: %v", err)
	}
	runCommand(t, repository, "git", "add", "--", name)
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "test: add value coverage")
	commit := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	if err := ValidateAutonomousTestAdequacyCommit(context.Background(), repository, commit, []string{name}); err != nil {
		t.Fatalf("valid exact additive test commit was rejected: %v", err)
	}
}

func TestAutonomousTestAdequacyRejectsExistingTestChanges(t *testing.T) {
	repository, _ := seedGitRepository(t)
	name := "tests/existing.test.ts"
	writeAutonomousTestFixture(t, repository, name, `test("existing", () => { expect("a").toBe("a"); });`)
	runCommand(t, repository, "git", "add", "--", name)
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "test: seed existing test")
	writeAutonomousTestFixture(t, repository, name, `test.skip("existing", () => { expect("a").toBe("a"); });`)
	if err := validateAdditiveTestFilesInWorktree(context.Background(), repository, []string{name}); err == nil || !strings.Contains(err.Error(), "must be additive") {
		t.Fatalf("existing test modification was not rejected: %v", err)
	}
	runCommand(t, repository, "git", "add", "--", name)
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "test: weaken existing test")
	commit := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	if err := ValidateAutonomousTestAdequacyCommit(context.Background(), repository, commit, []string{name}); err == nil || !strings.Contains(err.Error(), "must only add") {
		t.Fatalf("non-additive exact commit was not rejected at merge boundary: %v", err)
	}
}

func TestAutonomousTestAdequacyRejectsSkippedAndTrivialAdditions(t *testing.T) {
	for testName, content := range map[string]string{
		"skipped": `test.skip("not run", () => { expect(loadValue()).toBe("value"); });`,
		"focused": `it.only("one case", () => { expect(loadValue()).toBe("value"); });`,
		"trivial": `test("tautology", () => { expect(true).toBe(true); });`,
		"empty":   `test("empty", () => {});`,
	} {
		t.Run(testName, func(t *testing.T) {
			repository, _ := seedGitRepository(t)
			name := "tests/" + testName + ".test.ts"
			writeAutonomousTestFixture(t, repository, name, content)
			if err := validateAdditiveTestFilesInWorktree(context.Background(), repository, []string{name}); err == nil {
				t.Fatal("non-runnable or trivial additive test was accepted")
			}
		})
	}
}

func writeAutonomousTestFixture(t *testing.T, repository, name, content string) {
	t.Helper()
	path := filepath.Join(repository, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
