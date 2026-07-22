package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractAndApplyModelPatch(t *testing.T) {
	repository, _ := seedGitRepository(t)
	response := `Patch follows.
HIVE_PATCH_BEGIN
diff --git a/src/value.txt b/src/value.txt
--- a/src/value.txt
+++ b/src/value.txt
@@ -1 +1 @@
-broken
+fixed by patch
HIVE_PATCH_END`
	patch, err := extractModelPatch(response)
	if err != nil {
		t.Fatal(err)
	}
	files, err := patchChangedFiles(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "src/value.txt" {
		t.Fatalf("unexpected patch files: %v", files)
	}
	if err := applyModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repository, "src", "value.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "fixed by patch" {
		t.Fatalf("patch did not apply: %q err=%v", data, err)
	}
}

func TestModelPatchRejectsSecretRulesWithoutEchoingValues(t *testing.T) {
	tests := []struct {
		name, added, rule, secret string
	}{
		{name: "AWS", added: `const id = "` + "AK" + "IA" + `ABCDEFGHIJKLMNOP"`, rule: "aws-access-key-id-v1", secret: "AK" + "IA" + "ABCDEFGHIJKLMNOP"},
		{name: "Slack", added: `const token = "` + "xo" + "xb-1234567890-ABCDEFGHIJ" + `"`, rule: "slack-token-v1", secret: "xo" + "xb-1234567890-ABCDEFGHIJ"},
		{name: "typed assignment", added: `var clientSecret string = "actual-operator-value-must-not-leave"`, rule: "sensitive-assignment-v2", secret: "actual-operator-value-must-not-leave"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := "HIVE_PATCH_BEGIN\ndiff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+" + test.added + "\nHIVE_PATCH_END"
			_, err := extractModelPatch(response)
			if err == nil || !strings.Contains(err.Error(), test.rule) || strings.Contains(err.Error(), test.secret) {
				t.Fatalf("unsafe patch was not rejected without disclosure: %v", err)
			}
		})
	}
}

func TestModelPatchAllowsExplicitNonCredentialFixture(t *testing.T) {
	response := "HIVE_PATCH_BEGIN\ndiff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+const token = \"test-token\"\nHIVE_PATCH_END"
	if _, err := extractModelPatch(response); err != nil {
		t.Fatalf("safe explicit fixture was rejected: %v", err)
	}
}

func TestModelPatchAlreadyAppliedDetectsCrashResume(t *testing.T) {
	repository, _ := seedGitRepository(t)
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n"
	applied, err := modelPatchAlreadyApplied(context.Background(), repository, patch)
	if err != nil || applied {
		t.Fatalf("fresh patch reported already applied: applied=%t err=%v", applied, err)
	}
	if err := applyModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	applied, err = modelPatchAlreadyApplied(context.Background(), repository, patch)
	if err != nil || !applied {
		t.Fatalf("applied patch was not recognized after resume: applied=%t err=%v", applied, err)
	}
}

func TestRejectedModelPatchIsNotInfrastructureFailure(t *testing.T) {
	repository, _ := seedGitRepository(t)
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-not-the-current-value\n+fixed\n"
	err := applyModelPatch(context.Background(), repository, patch)
	if err == nil {
		t.Fatal("expected deterministic patch rejection")
	}
	if isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("invalid model output was misclassified as patch-engine infrastructure: %v", err)
	}
}

func TestPatchCommandFailureClassifiesFatalRepositoryErrorsAsInfrastructure(t *testing.T) {
	exitErr := errors.New("exit status 128")
	failure := errors.New("git apply failed")
	for _, output := range []string{
		"fatal: Unable to create '.git/index.lock': Permission denied",
		"fatal: not a git repository",
		"error: unable to write new index file: No space left on device",
	} {
		classified := classifyPatchCommandFailure(context.Background(), exitErr, output, failure)
		if !isPatchEngineInfrastructureFailure(classified) {
			t.Fatalf("fatal repository failure was charged to the model: %q => %v", output, classified)
		}
	}
	classified := classifyPatchCommandFailure(context.Background(), exitErr, "error: patch failed: src/value.txt:1\nerror: src/value.txt: patch does not apply", failure)
	if isPatchEngineInfrastructureFailure(classified) {
		t.Fatalf("deterministic patch rejection was classified as infrastructure: %v", classified)
	}
}

func TestApplyIncrementalModelPatchSkipsAlreadyAppliedHunk(t *testing.T) {
	repository, _ := seedGitRepository(t)
	path := filepath.Join(repository, "src", "value.txt")
	current := "target:\n  kind: command\n  build: npm run build\n  serve: node scripts/testing/start-lhci-server.mjs\n  url: http://127.0.0.1:4173\nselectors:\n  textMustExist: []\n  textMustNotExist:\n    - visual-hive api-500 mutation\nscreenshots: []\n"
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1,5 +1,5 @@\n target:\n   kind: command\n   build: npm run build\n-  serve: node scripts/testing/start-lhci-server.mjs\n+  serve: npm run preview\n   url: http://127.0.0.1:4173\n@@ -5,5 +5,6 @@\n   url: http://127.0.0.1:4173\n selectors:\n   textMustExist: []\n-  textMustNotExist: []\n+  textMustNotExist:\n+    - visual-hive api-500 mutation\n screenshots: []\n"
	if err := applyIncrementalModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(current, "serve: node scripts/testing/start-lhci-server.mjs", "serve: npm run preview", 1)
	if strings.ReplaceAll(string(data), "\r\n", "\n") != expected {
		t.Fatalf("incremental patch did not apply only the missing hunk: %q", data)
	}
}

func TestApplyIncrementalModelPatchCreatesProductionNewFileAndResumes(t *testing.T) {
	repository, _ := seedGitRepository(t)
	valuePath := filepath.Join(repository, "src", "value.txt")
	if err := os.WriteFile(valuePath, []byte("cumulative repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexBefore := gitOutput(t, repository, "write-tree")
	patch := `diff --git a/src/metrics.test.ts b/src/metrics.test.ts
new file mode 100644
--- /dev/null
+++ b/src/metrics.test.ts
@@ -0,0 +1,30 @@
+import { describe, expect, it } from "vitest";
+
+import { fallbackMetrics, percentage } from "./metrics";
+
+describe("fallbackMetrics", () => {
+  it("matches the local dashboard contract", () => {
+    expect(fallbackMetrics).toEqual({
+      activeJobs: 12,
+      gpuUtilization: 87,
+      queueDepth: 4,
+      energyEfficiency: 91
+    });
+  });
+});
+
+describe("percentage", () => {
+  it.each([
+    [0, "0%"],
+    [42.5, "42.5%"],
+    [100, "100%"]
+  ])("formats %s as %s", (value, expected) => {
+    expect(percentage(value)).toBe(expected);
+  });
+
+  it.each([-1, 101, Number.NaN, Number.POSITIVE_INFINITY])(
+    "rejects an invalid percentage value of %s",
+    (value) => {
+      expect(() => percentage(value)).toThrow(
+        new RangeError("percentage must be between 0 and 100")
+      );
+    }
+  );
+});
`
	alreadyApplied, err := modelPatchAlreadyApplied(context.Background(), repository, patch)
	if err != nil || alreadyApplied {
		t.Fatalf("fresh production new-file patch reported already applied: applied=%t err=%v", alreadyApplied, err)
	}
	if err := applyIncrementalModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repository, "src", "metrics.test.ts"))
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.Contains(normalized, `import { fallbackMetrics, percentage } from "./metrics";`) ||
		!strings.Contains(normalized, `new RangeError("percentage must be between 0 and 100")`) {
		t.Fatalf("new-file patch produced unexpected content: %q", data)
	}
	if value, err := os.ReadFile(valuePath); err != nil || strings.ReplaceAll(string(value), "\r\n", "\n") != "cumulative repair\n" {
		t.Fatalf("preexisting cumulative repair state changed: %q err=%v", value, err)
	}
	if indexAfter := gitOutput(t, repository, "write-tree"); indexAfter != indexBefore {
		t.Fatalf("incremental new-file patch changed the real index: before=%s after=%s", indexBefore, indexAfter)
	}
	// Worker.Run performs this exact check before applying a persisted model
	// patch after a crash. A true result skips a second application/model call.
	alreadyApplied, err = modelPatchAlreadyApplied(context.Background(), repository, patch)
	if err != nil || !alreadyApplied {
		t.Fatalf("applied production new-file patch was not resumable: applied=%t err=%v", alreadyApplied, err)
	}
}

func TestApplyIncrementalModelPatchMixedPatchPreservesStagedIndex(t *testing.T) {
	repository, _ := seedGitRepository(t)
	valuePath := filepath.Join(repository, "src", "value.txt")
	if err := os.WriteFile(valuePath, []byte("staged repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "--", "src/value.txt")
	indexBefore := gitOutput(t, repository, "write-tree")
	cachedBefore := gitOutput(t, repository, "diff", "--cached", "--", "src/value.txt")
	if err := os.WriteFile(valuePath, []byte("cumulative repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/metrics.test.ts b/src/metrics.test.ts\nnew file mode 100644\n--- /dev/null\n+++ b/src/metrics.test.ts\n@@ -0,0 +1 @@\n+export const covered = true;\n" +
		"diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-cumulative repair\n+repaired cumulatively\n"
	if err := applyIncrementalModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(valuePath); err != nil || strings.ReplaceAll(string(value), "\r\n", "\n") != "repaired cumulatively\n" {
		t.Fatalf("mixed patch did not update the cumulative worktree: %q err=%v", value, err)
	}
	if value, err := os.ReadFile(filepath.Join(repository, "src", "metrics.test.ts")); err != nil || strings.ReplaceAll(string(value), "\r\n", "\n") != "export const covered = true;\n" {
		t.Fatalf("mixed patch did not create its new file: %q err=%v", value, err)
	}
	if indexAfter := gitOutput(t, repository, "write-tree"); indexAfter != indexBefore {
		t.Fatalf("mixed patch changed the caller's staged index: before=%s after=%s", indexBefore, indexAfter)
	}
	if cachedAfter := gitOutput(t, repository, "diff", "--cached", "--", "src/value.txt"); cachedAfter != cachedBefore {
		t.Fatalf("mixed patch changed the caller's staged content:\n--- before ---\n%s\n--- after ---\n%s", cachedBefore, cachedAfter)
	}
}

func TestApplyIncrementalModelPatchDeletesFileWithoutChangingIndex(t *testing.T) {
	repository, _ := seedGitRepository(t)
	obsoletePath := filepath.Join(repository, "src", "obsolete.txt")
	if err := os.WriteFile(obsoletePath, []byte("obsolete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "--", "src/obsolete.txt")
	runCommand(t, repository, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add obsolete fixture")
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("cumulative repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexBefore := gitOutput(t, repository, "write-tree")
	patch := "diff --git a/src/obsolete.txt b/src/obsolete.txt\ndeleted file mode 100644\n--- a/src/obsolete.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-obsolete\n"
	if err := applyIncrementalModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(obsoletePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted-file patch left its target behind: %v", err)
	}
	if value, err := os.ReadFile(filepath.Join(repository, "src", "value.txt")); err != nil || strings.ReplaceAll(string(value), "\r\n", "\n") != "cumulative repair\n" {
		t.Fatalf("deleted-file patch changed unrelated cumulative state: %q err=%v", value, err)
	}
	if indexAfter := gitOutput(t, repository, "write-tree"); indexAfter != indexBefore {
		t.Fatalf("deleted-file patch changed the real index: before=%s after=%s", indexBefore, indexAfter)
	}
}

func TestApplyIncrementalModelPatchRejectionPreservesIndexAndWorktree(t *testing.T) {
	repository, _ := seedGitRepository(t)
	valuePath := filepath.Join(repository, "src", "value.txt")
	if err := os.WriteFile(valuePath, []byte("staged repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "add", "--", "src/value.txt")
	indexBefore := gitOutput(t, repository, "write-tree")
	cachedBefore := gitOutput(t, repository, "diff", "--cached", "--", "src/value.txt")
	if err := os.WriteFile(valuePath, []byte("cumulative repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-not-the-cumulative-value\n+repaired\n"
	err := applyIncrementalModelPatch(context.Background(), repository, patch)
	if err == nil {
		t.Fatal("expected deterministic incremental patch rejection")
	}
	if isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("deterministic rejection was classified as infrastructure: %v", err)
	}
	if value, readErr := os.ReadFile(valuePath); readErr != nil || strings.ReplaceAll(string(value), "\r\n", "\n") != "cumulative repair\n" {
		t.Fatalf("rejected patch changed the cumulative worktree: %q err=%v", value, readErr)
	}
	if indexAfter := gitOutput(t, repository, "write-tree"); indexAfter != indexBefore {
		t.Fatalf("rejected patch changed the real index: before=%s after=%s", indexBefore, indexAfter)
	}
	if cachedAfter := gitOutput(t, repository, "diff", "--cached", "--", "src/value.txt"); cachedAfter != cachedBefore {
		t.Fatalf("rejected patch changed staged content:\n--- before ---\n%s\n--- after ---\n%s", cachedBefore, cachedAfter)
	}
}

func TestPatchChangedFilesRejectsUnsafeMetadata(t *testing.T) {
	for name, patch := range map[string]string{
		"traversal": "diff --git a/../outside b/../outside\n--- a/../outside\n+++ b/../outside\n@@ -0,0 +1 @@\n+x\n",
		"rename":    "diff --git a/src/a b/src/b\nrename from src/a\nrename to src/b\n",
		"binary":    "diff --git a/src/a b/src/a\nGIT binary patch\n",
		"git":       "diff --git a/.git/config b/.git/config\n--- a/.git/config\n+++ b/.git/config\n@@ -0,0 +1 @@\n+x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := patchChangedFiles(patch); err == nil {
				t.Fatal("expected unsafe patch rejection")
			}
		})
	}
}
