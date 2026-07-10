package integrated

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectCheckoutBuildsRepositorySpecificSignals(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test":"vitest","typecheck":"tsc --noEmit"},"dependencies":{"react":"19.0.0"},"devDependencies":{"@playwright/test":"1.50.0"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)
	writeFixture(t, root, ".github/workflows/ci.yml", "name: ci")
	writeFixture(t, root, "src/auth/Login.tsx", "export const Login = 1")
	writeFixture(t, root, "visual-hive.baselines/linux/home.png", "png")
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(inspection.Languages, "TypeScript/JavaScript") || !contains(inspection.Frameworks, "React") || !contains(inspection.Frameworks, "Playwright") || !contains(inspection.PackageManagers, "npm") {
		t.Fatalf("missing detection signals: %+v", inspection)
	}
	if len(inspection.TestCommands) != 2 || len(inspection.CIFiles) != 1 || len(inspection.BaselineFiles) != 1 || len(inspection.HighRiskPaths) == 0 {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
}

func TestBuildSetupPlanKeepsCoverageAndAuthoritySeparate(t *testing.T) {
	plan := buildSetupPlan(SetupOptions{Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", VisualHive: true}, RepositoryInspection{DefaultBranch: "main"})
	if plan.Coverage != CoverageComprehensive || plan.Automation != AutomationIssues || plan.ACMMLevel != 4 {
		t.Fatalf("dimensions were conflated: %+v", plan)
	}
	if len(plan.TestingLayers) < 10 || !plan.ReadOnly {
		t.Fatalf("comprehensive read-only plan incomplete: %+v", plan)
	}
}

func TestWorkflowUsesTwoArtifactProvenanceAndPinnedActions(t *testing.T) {
	value := workflow(Config{DefaultBranch: "main", VisualHiveRepo: "owner/visual-hive", VisualHiveRef: "0123456789012345678901234567890123456789"})
	for _, required := range []string{checkoutActionSHA, setupNodeActionSHA, uploadArtifactActionSHA, "steps.evidence.outputs.artifact-id", "visual-hive-bundle-${{ github.run_id }}"} {
		if !containsString(value, required) {
			t.Fatalf("workflow missing %q", required)
		}
	}
	if !containsString(value, "pipeline-exit-code.txt") || !containsString(value, "set +e") {
		t.Fatal("workflow must publish evidence after a deterministic red verdict")
	}
	if strings.Count(value, "include-hidden-files: true") != 2 {
		t.Fatal("both hidden .visual-hive artifact uploads must opt in explicitly")
	}
	if !containsString(value, "hive integration-smoke") {
		t.Fatal("workflow must validate Hive import artifacts before finalizing a lifecycle bundle")
	}
	if containsString(value, "pull_request_target") || containsString(value, "issues: write") || containsString(value, "pull-requests: write") {
		t.Fatalf("workflow has an unsafe write lane:\n%s", value)
	}
}

func TestManagedRepositoryConfigExcludesLocalPaths(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".github/workflows/visual-hive-issue-lifecycle.yml", "issues: write")
	writeFixture(t, root, ".github/workflows/visual-hive-trusted-publisher.yml", "issues: write")
	config := Config{
		Repository: "owner/repo", DefaultBranch: "main", Coverage: CoverageStandard, Automation: AutomationIssues,
		Provider: "codex", ACMMLevel: 4, VisualHive: true, VisualHiveRepo: "owner/visual-hive",
		VisualHiveRef: "0123456789012345678901234567890123456789", CheckoutDir: `C:\private\checkout`, StateDir: `C:\private\state`,
	}
	if err := writeManagedFiles(root, config, RepositoryInspection{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".hive", "integrated.json"))
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if strings.Contains(value, "private") || strings.Contains(value, "checkout_dir") || strings.Contains(value, "state_dir") {
		t.Fatalf("local paths leaked into repository config: %s", value)
	}
	for _, relative := range []string{".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("standalone writer %s must be removed in integrated mode", relative)
		}
	}
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsString(value, target string) bool {
	return len(target) > 0 && strings.Contains(value, target)
}
