package integrated

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGeneratedHostedControllerWorkflowActionlint(t *testing.T) {
	actionlint := strings.TrimSpace(os.Getenv("HIVE_ACTIONLINT"))
	if actionlint == "" {
		t.Skip("HIVE_ACTIONLINT is set only by the pinned workflow-validation gate")
	}
	info, err := os.Lstat(actionlint)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("HIVE_ACTIONLINT is not an ordinary executable: %v", err)
	}
	generated, err := GenerateHostedControllerWorkflow(hostedWorkflowFixture())
	if err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(t.TempDir(), "hive-controller.yml")
	if err := os.WriteFile(workflow, []byte(generated), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(actionlint, workflow)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated hosted controller failed actionlint: %v\n%s", err, output)
	}
}

func hostedWorkflowFixture() HostedWorkflowConfig {
	return HostedWorkflowConfig{
		Repository:                 "DavidDiaz0317/acceptance-repository",
		RepositoryID:               "123456789",
		DefaultBranch:              "main",
		ScheduleCron:               "17 */6 * * *",
		StateBranch:                "hive/controller-state",
		StateKeySecret:             "HIVE_HOSTED_STATE_KEY",
		ReleaseRepository:          "DavidDiaz0317/hive",
		ReleaseTag:                 "v0.4.1-integrated.19",
		HiveCommit:                 strings.Repeat("a", 40),
		VisualHiveCommit:           strings.Repeat("b", 40),
		DistributionManifestSHA256: strings.Repeat("c", 64),
		ProviderRequired:           true,
	}
}

func TestGenerateHostedControllerWorkflowSecurityContract(t *testing.T) {
	config := hostedWorkflowFixture()
	generated, err := GenerateHostedControllerWorkflow(config)
	if err != nil {
		t.Fatalf("GenerateHostedControllerWorkflow: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(generated), &document); err != nil {
		t.Fatalf("generated hosted workflow is not valid YAML: %v", err)
	}
	for _, required := range []string{
		"permissions: {}",
		"group: hive-controller-" + config.RepositoryID,
		"cancel-in-progress: false",
		"- cron: \"" + config.ScheduleCron + "\"",
		"request_id:", "operation:", "- cycle", "- status", "- doctor", "- pause", "- resume",
		"actions: read\n      contents: write\n      issues: write\n      pull-requests: write\n      checks: read\n      statuses: read",
		"test \"$HIVE_REPOSITORY_ID_CONTEXT\" = \"" + config.RepositoryID + "\"",
		config.Repository + "/" + HostedControllerWorkflowPath + "@refs/heads/" + config.DefaultBranch,
		"actions/checkout@" + checkoutActionSHA,
		"persist-credentials: false\n          path: target-metadata",
		"ref: ${{ github.sha }}\n          fetch-depth: 1\n          persist-credentials: false",
		"ref: " + config.StateBranch + "\n          fetch-depth: 2\n          persist-credentials: true\n          path: hive-controller-state",
		"gh release download \"$HIVE_RELEASE_TAG\"",
		"gh attestation verify",
		"--deny-self-hosted-runners",
		"test \"$after_commit\" = \"$before_commit\"",
		config.ReleaseRepository, config.ReleaseTag, config.HiveCommit, config.VisualHiveCommit,
		config.DistributionManifestSHA256,
		"hosted-cycle \\", "--repo " + config.Repository, "--repository-id " + config.RepositoryID,
		"--state-branch " + config.StateBranch, "--state-key-env HIVE_HOSTED_STATE_KEY", "--json >\"$result\"",
		"--checkout-dir \"$GITHUB_WORKSPACE/target-metadata\"", "@openai/codex@0.144.1-linux-x64", "codex-cli 0.144.1",
		"if: ${{ true && (github.event_name == 'schedule' || inputs.operation == 'cycle' || inputs.operation == 'doctor') }}",
		"continue-on-error: true", "Authenticate exact Codex provider", "login --with-api-key", "chmod 0600 \"$auth_file\"",
		"--restore-release-version \"$HIVE_PREVIOUS_RELEASE_VERSION\"", "HIVE_PREVIOUS_MANIFEST_SHA256: \"\"",
		"result_bytes", "-le 1048576", "decoder.raw_decode", "actions/upload-artifact@" + uploadArtifactActionSHA,
	} {
		if !strings.Contains(generated, required) {
			t.Fatalf("generated hosted workflow lacks %q", required)
		}
	}
	for _, forbidden := range []string{
		"pull_request:", "pull_request_target:", "runs-on: self-hosted", "kubestellar/hive", "curl ", "wget ", "| sh", "| bash",
		"npm ", "go test", "make ", "./target-metadata", "cd target-metadata", "          - recover\n",
	} {
		if strings.Contains(generated, forbidden) {
			t.Fatalf("generated hosted workflow contains forbidden target/controller surface %q", forbidden)
		}
	}
	if count := strings.Count(generated, "permissions:\n      actions: read\n      contents: read"); count != 1 {
		t.Fatalf("status job must have one contents-read-only permission block, got %d", count)
	}
	if count := strings.Count(generated, "permissions:\n      actions: read\n      contents: write"); count != 1 {
		t.Fatalf("control job must have one contents-write-only permission block, got %d", count)
	}
	if count := strings.Count(generated, "path: target-metadata"); count != 3 {
		t.Fatalf("each permission-separated job must checkout the target without executing it, got %d", count)
	}
}

func TestGenerateHostedControllerWorkflowBindsExactPredecessorRelease(t *testing.T) {
	config := hostedWorkflowFixture()
	config.PreviousRelease = &HostedReleaseIdentity{
		Version: "v0.4.1-integrated.18", HiveCommit: strings.Repeat("d", 40),
		VisualHiveCommit: strings.Repeat("e", 40), DistributionManifestSHA256: strings.Repeat("f", 64),
		HostedControllerProtocol: HostedControllerProtocol,
	}
	generated, err := GenerateHostedControllerWorkflow(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{config.PreviousRelease.Version, config.PreviousRelease.HiveCommit, config.PreviousRelease.VisualHiveCommit, config.PreviousRelease.DistributionManifestSHA256} {
		if !strings.Contains(generated, expected) {
			t.Fatalf("hosted workflow omitted exact predecessor identity %q", expected)
		}
	}
	config.PreviousRelease.DistributionManifestSHA256 = ""
	if _, err := GenerateHostedControllerWorkflow(config); err == nil {
		t.Fatal("incomplete predecessor release was accepted")
	}
}

func TestGenerateHostedControllerWorkflowComparesCompletePredecessorIdentity(t *testing.T) {
	config := hostedWorkflowFixture()
	config.PreviousRelease = &HostedReleaseIdentity{
		Version: config.ReleaseTag, HiveCommit: config.HiveCommit, VisualHiveCommit: strings.Repeat("d", 40),
		DistributionManifestSHA256: strings.Repeat("e", 64), HostedControllerProtocol: HostedControllerProtocol,
	}
	if _, err := GenerateHostedControllerWorkflow(config); err != nil {
		t.Fatalf("distinct full predecessor sharing version and Hive commit was rejected: %v", err)
	}
	config.PreviousRelease.VisualHiveCommit = config.VisualHiveCommit
	config.PreviousRelease.DistributionManifestSHA256 = config.DistributionManifestSHA256
	if _, err := GenerateHostedControllerWorkflow(config); err == nil {
		t.Fatal("predecessor identical to the complete current release was accepted")
	}
}

func TestGenerateHostedControllerWorkflowScopesProviderSecretAndKeepsFailureReceipt(t *testing.T) {
	config := hostedWorkflowFixture()
	generated, err := GenerateHostedControllerWorkflow(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(generated, "OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}") != 3 {
		t.Fatalf("provider secret must appear only in the one auth step per job")
	}
	if strings.Contains(generated, "HIVE_GITHUB_TOKEN: ${{ github.token }}\n          OPENAI_API_KEY:") {
		t.Fatal("hosted Hive operation inherited the raw provider secret")
	}
	for _, required := range []string{
		"id: provider-install", "id: provider-auth", "continue-on-error: true",
		"HIVE_HOSTED_CODEX_CANDIDATE", "login --with-api-key", "test ! -L \"$auth_file\"",
		"chmod 0600 \"$auth_file\"", "steps.provider-install.outcome == 'success'",
	} {
		if !strings.Contains(generated, required) {
			t.Fatalf("provider bootstrap omitted %q", required)
		}
	}

	config.ProviderRequired = false
	withoutProvider, err := GenerateHostedControllerWorkflow(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withoutProvider, "if: ${{ false && (github.event_name") || !strings.Contains(withoutProvider, "if: ${{ false && steps.provider-install.outcome") {
		t.Fatal("advisory/issues controller did not statically disable provider installation and authentication")
	}
}

func TestGenerateHostedControllerWorkflowRejectsInvalidInputs(t *testing.T) {
	tests := map[string]func(*HostedWorkflowConfig){
		"repository injection": func(c *HostedWorkflowConfig) { c.Repository = "owner/repo\npermissions: write-all" },
		"repository path":      func(c *HostedWorkflowConfig) { c.Repository = "owner/.." },
		"repository id":        func(c *HostedWorkflowConfig) { c.RepositoryID = "0 || github.run_id" },
		"default branch":       func(c *HostedWorkflowConfig) { c.DefaultBranch = "main@{1}" },
		"state branch":         func(c *HostedWorkflowConfig) { c.StateBranch = "state/../main" },
		"shared state branch":  func(c *HostedWorkflowConfig) { c.StateBranch = c.DefaultBranch },
		"cron alias":           func(c *HostedWorkflowConfig) { c.ScheduleCron = "@hourly" },
		"cron range":           func(c *HostedWorkflowConfig) { c.ScheduleCron = "61 25 * * *" },
		"release repository":   func(c *HostedWorkflowConfig) { c.ReleaseRepository = "kubestellar/hive" },
		"release tag":          func(c *HostedWorkflowConfig) { c.ReleaseTag = "latest" },
		"hive commit":          func(c *HostedWorkflowConfig) { c.HiveCommit = strings.Repeat("A", 40) },
		"visual commit":        func(c *HostedWorkflowConfig) { c.VisualHiveCommit = "main" },
		"manifest digest":      func(c *HostedWorkflowConfig) { c.DistributionManifestSHA256 = strings.Repeat("z", 64) },
		"secret expression":    func(c *HostedWorkflowConfig) { c.StateKeySecret = "${{ secrets.OTHER }}" },
		"secret lowercase":     func(c *HostedWorkflowConfig) { c.StateKeySecret = "hive_key" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := hostedWorkflowFixture()
			mutate(&config)
			if generated, err := GenerateHostedControllerWorkflow(config); err == nil || generated != "" {
				t.Fatalf("invalid config rendered a workflow: err=%v", err)
			}
		})
	}
}

func TestGenerateHostedControllerWorkflowEmbedsPinsWithoutMutableExpressions(t *testing.T) {
	config := hostedWorkflowFixture()
	generated, err := GenerateHostedControllerWorkflow(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{
		"HIVE_RELEASE_REPOSITORY: " + config.ReleaseRepository,
		"HIVE_RELEASE_TAG: " + config.ReleaseTag,
		"HIVE_EXPECTED_COMMIT: " + config.HiveCommit,
		"HIVE_EXPECTED_VISUAL_COMMIT: " + config.VisualHiveCommit,
		"HIVE_EXPECTED_MANIFEST_SHA256: " + config.DistributionManifestSHA256,
	} {
		if strings.Count(generated, identity) != 3 {
			t.Fatalf("expected exact identity once per job for %q", identity)
		}
	}
	for _, mutable := range []string{"releases/latest", "--version latest", "@main", "@master", "github.head_ref", "github.event.pull_request"} {
		if strings.Contains(generated, mutable) {
			t.Fatalf("hosted controller contains mutable identity %q", mutable)
		}
	}
}
