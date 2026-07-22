package integrated

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	"gopkg.in/yaml.v3"
)

type isolatedWorkflowDocument struct {
	Jobs map[string]isolatedWorkflowJob `yaml:"jobs"`
}

func TestIntegratedArtifactRequestsBindPinnedVisualHiveProducer(t *testing.T) {
	for _, source := range []string{"run.go", "setup_baseline.go"} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if requests, bindings := strings.Count(text, "hivegithub.VisualHiveArtifactRequest{"), strings.Count(text, "ExpectedProducerGitCommit: config.VisualHiveRef"); requests != 1 || bindings != requests {
			t.Fatalf("%s must bind every Visual Hive artifact request to config.VisualHiveRef: requests=%d bindings=%d", source, requests, bindings)
		}
	}
}

func TestRunnerOwnedResolutionScopesRequireFreshStrictReceipts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to execute the runner-owned resolution receipt verifier")
	}
	type receiptMutation func(map[string]any, map[string]any, map[string]any, map[string]any, map[string]any)
	writeReceipts := func(t *testing.T, root string, mutate receiptMutation) {
		t.Helper()
		layerRows := make([]any, 0, 12)
		for id := 0; id < 12; id++ {
			layerRows = append(layerRows, map[string]any{"id": id, "status": "covered", "evidence": []any{"current-run"}, "gaps": []any{}, "skippedReasons": []any{}})
		}
		layers := map[string]any{"schemaVersion": 1, "layers": layerRows}
		workflows := map[string]any{"schemaVersion": 1, "summary": map[string]any{"workflowCount": 1}, "workflows": []any{map[string]any{"path": ".github/workflows/hive-visual-hive.yml"}}, "findings": []any{}}
		providers := map[string]any{"schemaVersion": 1, "providers": []any{map[string]any{"providerId": "playwright", "result": map[string]any{"providerId": "playwright", "status": "passed"}}}}
		evidence := map[string]any{"providers": []any{map[string]any{"providerId": "playwright", "status": "passed"}, map[string]any{"providerId": "playwright", "status": "passed"}}}
		report := map[string]any{"providerResults": []any{map[string]any{"providerId": "playwright", "status": "passed"}}}
		if mutate != nil {
			mutate(layers, workflows, providers, evidence, report)
		}
		for relative, value := range map[string]any{
			".visual-hive/testing-layers.json":   layers,
			".visual-hive/workflows.json":        workflows,
			".visual-hive/provider-results.json": providers,
			".visual-hive/evidence-packet.json":  evidence,
			".visual-hive/report.json":           report,
		} {
			data, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			writeFixture(t, root, relative, string(data))
		}
		writeFixture(t, root, ".visual-hive/evaluated-contracts.txt", "contract-a\n")
	}
	run := func(t *testing.T, mutate receiptMutation) ([]byte, error, string) {
		t.Helper()
		root := t.TempDir()
		writeReceipts(t, root, mutate)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, node, "-e", runnerOwnedResolutionScopeReceiptScript())
		command.Dir = root
		output, runErr := command.CombinedOutput()
		evaluated, _ := os.ReadFile(filepath.Join(root, ".visual-hive", "evaluated-contracts.txt"))
		return output, runErr, string(evaluated)
	}
	if output, runErr, evaluated := run(t, nil); runErr != nil {
		t.Fatalf("strict current receipts were rejected: %v\n%s", runErr, output)
	} else {
		for _, scope := range []string{"contract-a", "testing-layer:0", "testing-layer:11", "workflow-safety", "provider-governance"} {
			if !strings.Contains(evaluated, scope+"\n") {
				t.Fatalf("evaluated scope omitted %q: %q", scope, evaluated)
			}
		}
	}
	withheld := []struct {
		name   string
		mutate receiptMutation
		scope  string
	}{
		{name: "partial layer", scope: "testing-layer:2", mutate: func(layers, _, _, _, _ map[string]any) {
			layers["layers"].([]any)[2].(map[string]any)["status"] = "partial"
		}},
		{name: "covered layer with remaining gap", scope: "testing-layer:2", mutate: func(layers, _, _, _, _ map[string]any) {
			layers["layers"].([]any)[2].(map[string]any)["gaps"] = []any{"current-run gap"}
		}},
		{name: "workflow findings beyond issue projection", scope: "workflow-safety", mutate: func(_, workflows, _, _, _ map[string]any) {
			findings := make([]any, 11)
			for index := range findings {
				findings[index] = map[string]any{"id": fmt.Sprintf("workflow-finding-%02d", index)}
			}
			workflows["findings"] = findings
		}},
		{name: "provider failure", scope: "provider-governance", mutate: func(_, _, providers, evidence, report map[string]any) {
			providers["providers"].([]any)[0].(map[string]any)["result"].(map[string]any)["status"] = "failed"
			evidence["providers"] = []any{map[string]any{"providerId": "playwright", "status": "failed"}, map[string]any{"providerId": "playwright", "status": "failed"}}
			report["providerResults"] = []any{map[string]any{"providerId": "playwright", "status": "failed"}}
		}},
		{name: "provider upload failure", scope: "provider-governance", mutate: func(_, _, providers, evidence, report map[string]any) {
			upload := map[string]any{"status": "failed"}
			providers["providers"].([]any)[0].(map[string]any)["result"].(map[string]any)["upload"] = upload
			evidence["providers"] = []any{
				map[string]any{"providerId": "playwright", "status": "passed", "upload": upload},
				map[string]any{"providerId": "playwright", "status": "passed", "upload": upload},
			}
			report["providerResults"] = []any{map[string]any{"providerId": "playwright", "status": "passed", "upload": upload}}
		}},
	}
	for _, test := range withheld {
		t.Run(test.name, func(t *testing.T) {
			output, runErr, evaluated := run(t, test.mutate)
			if runErr != nil {
				t.Fatalf("valid active receipt was rejected: %v\n%s", runErr, output)
			}
			if strings.Contains(evaluated, test.scope+"\n") {
				t.Fatalf("active receipt authorized resolution scope %q: %q", test.scope, evaluated)
			}
		})
	}
	negative := []struct {
		name   string
		mutate receiptMutation
	}{
		{name: "incomplete layers", mutate: func(layers, _, _, _, _ map[string]any) { layers["layers"] = layers["layers"].([]any)[:11] }},
		{name: "empty workflow audit", mutate: func(_, workflows, _, _, _ map[string]any) { workflows["workflows"] = []any{} }},
		{name: "provider evidence mismatch", mutate: func(_, _, _, evidence, _ map[string]any) {
			evidence["providers"] = []any{map[string]any{"providerId": "playwright", "status": "failed"}}
		}},
	}
	for _, test := range negative {
		t.Run(test.name, func(t *testing.T) {
			if output, runErr, evaluated := run(t, test.mutate); runErr == nil {
				t.Fatalf("malformed receipt was accepted; evaluated=%q output=%s", evaluated, output)
			}
		})
	}
}

type isolatedWorkflowJob struct {
	Name           string            `yaml:"name"`
	If             string            `yaml:"if"`
	Needs          workflowNeedsList `yaml:"needs"`
	TimeoutMinutes int               `yaml:"timeout-minutes"`
	Steps          []struct {
		ID   string            `yaml:"id"`
		Name string            `yaml:"name"`
		If   string            `yaml:"if"`
		Uses string            `yaml:"uses"`
		Run  string            `yaml:"run"`
		Env  map[string]string `yaml:"env"`
		With map[string]any    `yaml:"with"`
	} `yaml:"steps"`
}

func TestGeneratedSetupBaselineJobInventoryMatchesVerifierContract(t *testing.T) {
	document := parseIsolatedWorkflow(t, workflow(isolationWorkflowConfig()))
	capture, captureExists := document.Jobs[setupBaselineCaptureJobID]
	verifier, verifierExists := document.Jobs[setupBaselineVerifyJobID]
	if !captureExists || !verifierExists {
		t.Fatalf("generated workflow is missing baseline inventory IDs: capture=%t verifier=%t", captureExists, verifierExists)
	}
	if capture.Name != hivegithub.SetupBaselineCaptureJobDisplayName || verifier.Name != hivegithub.SetupBaselineVerifyJobDisplayName {
		t.Fatalf("generated display names diverge from verifier allowlist: capture=%q verifier=%q", capture.Name, verifier.Name)
	}
	if !strings.Contains(capture.If, "inputs.hive_operation == 'setup-baseline-capture'") ||
		!strings.Contains(verifier.If, "inputs.hive_operation == 'setup-baseline-capture'") || !contains(verifier.Needs, setupBaselineCaptureJobID) {
		t.Fatalf("generated baseline inventory topology is not the exact two-job lane: capture=%+v verifier=%+v", capture, verifier)
	}
}

type workflowNeedsList []string

func (value *workflowNeedsList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*value = workflowNeedsList{node.Value}
		return nil
	case yaml.SequenceNode:
		var decoded []string
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		*value = decoded
		return nil
	default:
		return node.Decode((*[]string)(value))
	}
}

func isolationWorkflowConfig() Config {
	return Config{
		RepositoryID: "123", DefaultBranch: "main", SetupBranch: "hive/setup-123", SetupAuthorizationActorID: 456,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: visualHivePullRequestProducerCommit, ACMMLevel: 4,
		TestCommands: [][]string{{"node", "--test"}, {"python", "-m", "pytest", "-q"}},
	}
}

func TestPullRequestWorkflowUsesCanonicalAuditedProducer(t *testing.T) {
	config := isolationWorkflowConfig()
	configuredProductionRef := strings.Repeat("f", 40)
	config.VisualHiveRef = configuredProductionRef

	production := workflow(config)
	pullRequest := pullRequestWorkflow(config)
	if !strings.Contains(production, configuredProductionRef) {
		t.Fatal("production workflow does not retain its independently configured Visual Hive ref")
	}
	if strings.Contains(pullRequest, configuredProductionRef) {
		t.Fatal("pull request workflow contains the independent production Visual Hive ref")
	}
	for _, required := range []string{
		"ref: " + visualHivePullRequestProducerCommit,
		"HIVE_VISUAL_HIVE_REF: " + visualHivePullRequestProducerCommit,
		"HIVE_PRODUCER_COMMIT: " + visualHivePullRequestProducerCommit,
	} {
		if !strings.Contains(pullRequest, required) {
			t.Fatalf("pull request workflow does not use its canonical audited producer %q", required)
		}
	}
}

func TestGeneratedWorkflowsUseCanonicalYAMLWhitespace(t *testing.T) {
	config := isolationWorkflowConfig()
	workflows := map[string]string{
		"production":   workflow(config),
		"pull_request": pullRequestWorkflow(config),
	}
	for name, value := range workflows {
		if !strings.HasSuffix(value, "\n") || strings.HasSuffix(value, "\n\n") {
			t.Fatalf("%s workflow must have exactly one terminal newline", name)
		}
		if strings.Contains(value, "\n\n\n") {
			t.Fatalf("%s workflow contains a non-canonical repeated blank line", name)
		}
		for lineNumber, line := range strings.Split(value, "\n") {
			if strings.TrimRight(line, " \t") != line {
				t.Fatalf("%s workflow line %d contains trailing whitespace", name, lineNumber+1)
			}
		}
	}
	if !strings.Contains(workflows["pull_request"], `HIVE_UNINSTALL_REQUIRED_FILES: "[]"`) ||
		strings.Contains(workflows["pull_request"], `HIVE_UNINSTALL_REQUIRED_FILES: '[]'`) {
		t.Fatal("pull-request workflow does not use the canonical empty JSON array scalar")
	}
}

func TestGeneratedRepositoryTestAnchorsPreserveMaximumPlanSemantics(t *testing.T) {
	const maximumWorkflowBytes = 512 * 1024
	config := isolationWorkflowConfig()
	config.TestCommands = make([][]string, 256)
	for index := range config.TestCommands {
		config.TestCommands[index] = []string{"node", "--test", fmt.Sprintf("fixture-%03d.test.js", index+1)}
	}
	anchors := []string{
		"hive-repository-test-checkout",
		"hive-repository-test-setup-node",
		"hive-repository-test-setup-python",
		"hive-repository-test-prepare-account",
		"hive-repository-test-provision-tools",
		"hive-repository-test-install-dependencies",
		"hive-repository-test-seal-checkout",
		"hive-repository-test-cleanup",
	}
	tests := []struct {
		name      string
		generated string
	}{
		{name: "production", generated: workflow(config)},
		{name: "pull-request", generated: pullRequestWorkflow(config)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.generated) >= maximumWorkflowBytes {
				t.Fatalf("maximum 256-command workflow is %d bytes, want less than %d", len(test.generated), maximumWorkflowBytes)
			}
			if strings.Contains(test.generated, "<<:") {
				t.Fatal("generated workflow uses unsupported YAML merge-key semantics")
			}
			for _, anchor := range anchors {
				if definitions := strings.Count(test.generated, "&"+anchor); definitions != 1 {
					t.Fatalf("anchor %q has %d definitions, want 1", anchor, definitions)
				}
				if aliases := strings.Count(test.generated, "*"+anchor); aliases != len(config.TestCommands)-1 {
					t.Fatalf("anchor %q has %d aliases, want %d", anchor, aliases, len(config.TestCommands)-1)
				}
			}

			document := parseIsolatedWorkflow(t, test.generated)
			first := document.Jobs["repository-test-001"]
			if len(first.Steps) != 9 {
				t.Fatalf("expanded first repository job has %d steps, want 9", len(first.Steps))
			}
			for index, command := range config.TestCommands {
				jobID := fmt.Sprintf("repository-test-%03d", index+1)
				job, exists := document.Jobs[jobID]
				if !exists || job.Name != fmt.Sprintf("Hive repository test %03d", index+1) {
					t.Fatalf("expanded repository job %q is missing or renamed: %+v", jobID, job)
				}
				if job.TimeoutMinutes != 90 {
					t.Fatalf("expanded repository job %q timeout is %d minutes, want 90", jobID, job.TimeoutMinutes)
				}
				if len(job.Steps) != len(first.Steps) {
					t.Fatalf("expanded repository job %q has %d steps, want %d", jobID, len(job.Steps), len(first.Steps))
				}
				commandSteps := 0
				for stepIndex, step := range job.Steps {
					if step.Name == "Execute exact repository test command" {
						commandSteps++
						quoted := make([]string, 0, len(command))
						for _, argument := range command {
							quoted = append(quoted, shellQuote(argument))
						}
						expected := "set -euo pipefail\n" + isolatedTargetEnvPrefix() + " " + strings.Join(quoted, " ") + "\n"
						if step.Run != expected {
							t.Fatalf("expanded repository job %q command changed:\n got %q\nwant %q", jobID, step.Run, expected)
						}
						continue
					}
					got, err := json.Marshal(step)
					if err != nil {
						t.Fatal(err)
					}
					want, err := json.Marshal(first.Steps[stepIndex])
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("expanded repository job %q step %d differs from its anchored definition", jobID, stepIndex+1)
					}
				}
				if commandSteps != 1 {
					t.Fatalf("expanded repository job %q has %d exact command steps, want 1", jobID, commandSteps)
				}
			}
		})
	}
}

func TestGeneratedRepositoryTestsProvisionTrustedRipgrep(t *testing.T) {
	for workflowName, generated := range map[string]string{
		"production":   workflow(isolationWorkflowConfig()),
		"pull-request": pullRequestWorkflow(isolationWorkflowConfig()),
	} {
		document := parseIsolatedWorkflow(t, generated)
		for _, jobID := range []string{"repository-test-001", "repository-test-002"} {
			job := document.Jobs[jobID]
			stepIndex := map[string]int{}
			for index, step := range job.Steps {
				stepIndex[step.Name] = index
			}
			for _, stepName := range []string{"Prepare isolated target account", "Install repository test dependencies", "Execute exact repository test command"} {
				if _, exists := stepIndex[stepName]; !exists {
					t.Fatalf("%s workflow job %q lacks required step %q", workflowName, jobID, stepName)
				}
			}
			provisionIndex, exists := stepIndex["Provision trusted repository test tools"]
			if !exists {
				t.Fatalf("%s workflow job %q lacks trusted tool provisioning", workflowName, jobID)
			}
			if !(stepIndex["Prepare isolated target account"] < provisionIndex &&
				provisionIndex < stepIndex["Install repository test dependencies"] &&
				stepIndex["Install repository test dependencies"] < stepIndex["Execute exact repository test command"]) {
				t.Fatalf("%s workflow job %q provisions trusted tools in the wrong order: %+v", workflowName, jobID, stepIndex)
			}
			provision := job.Steps[provisionIndex].Run
			for _, required := range []string{
				"sudo apt-get install -y -qq --no-install-recommends ripgrep",
				`rg_path="$(readlink -f /usr/bin/rg)"`,
				`test "$rg_path" = "/usr/bin/rg"`,
				`test "$(dpkg-query -S "$rg_path")" = "ripgrep: /usr/bin/rg"`,
				`test "$(stat -Lc '%u:%g' "$rg_path")" = "0:0"`,
				`System ripgrep executable is writable by the isolated target account`,
				`case "$rg_version" in`,
				`target_rg="$(` + isolatedTargetEnvPrefix() + ` bash --noprofile --norc -c 'command -v rg')"`,
				`test "$(readlink -f "$target_rg")" = "$rg_path"`,
				`npm --prefix "$probe_root" run --silent hive-rg-probe`,
				`Ordinary target npm execution did not report the provisioned system ripgrep version`,
			} {
				if !strings.Contains(provision, required) {
					t.Fatalf("%s workflow job %q trusted ripgrep prerequisite is missing %q", workflowName, jobID, required)
				}
			}
			if strings.Count(provision, isolatedTargetEnvPrefix()) != 2 {
				t.Fatalf("%s workflow job %q does not verify ripgrep through the exact target environment", workflowName, jobID)
			}
		}
	}
}

func TestGeneratedWorkflowLongRunScalarsContainNoGitHubExpressions(t *testing.T) {
	const githubExpressionLengthLimit = 21000
	longRunScalars := 0
	for workflowName, generated := range map[string]string{
		"production":   workflow(isolationWorkflowConfig()),
		"pull-request": pullRequestWorkflow(isolationWorkflowConfig()),
	} {
		document := parseIsolatedWorkflow(t, generated)
		for jobName, job := range document.Jobs {
			for stepIndex, step := range job.Steps {
				if len(step.Run) <= githubExpressionLengthLimit {
					continue
				}
				longRunScalars++
				if strings.Contains(step.Run, "${{") {
					t.Fatalf("%s workflow job %q step %d (%q) embeds a GitHub expression in a %d-byte run scalar", workflowName, jobName, stepIndex, step.Name, len(step.Run))
				}
			}
		}
	}
	if longRunScalars == 0 {
		t.Fatal("regression did not inspect any run scalar above GitHub's expression-length limit")
	}
}

func TestGeneratedWorkflowSealStepBindsExactHeadOutsideLongRunScalar(t *testing.T) {
	production := workflow(isolationWorkflowConfig())
	tests := []struct {
		name         string
		generated    string
		jobName      string
		expectedHead string
	}{
		{name: "production", generated: production, jobName: visualExecutionJobName, expectedHead: "${{ github.sha }}"},
		{name: "setup-baseline", generated: production, jobName: setupBaselineCaptureJobID, expectedHead: "${{ github.sha }}"},
		{name: "pull-request", generated: pullRequestWorkflow(isolationWorkflowConfig()), jobName: visualExecutionJobName, expectedHead: "${{ github.event.pull_request.head.sha }}"},
	}
	const comparison = `test "$(git rev-parse HEAD)" = "$HIVE_TARGET_HEAD_SHA"`
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := parseIsolatedWorkflow(t, test.generated)
			job, exists := document.Jobs[test.jobName]
			if !exists {
				t.Fatalf("generated workflow is missing job %q", test.jobName)
			}
			for _, step := range job.Steps {
				if step.ID != "seal_raw_evidence" {
					continue
				}
				if got := step.Env["HIVE_TARGET_HEAD_SHA"]; got != test.expectedHead {
					t.Fatalf("seal step HIVE_TARGET_HEAD_SHA = %q, want %q", got, test.expectedHead)
				}
				if got := strings.Count(step.Run, comparison); got != 2 {
					t.Fatalf("seal step contains %d quoted exact-head comparisons, want 2", got)
				}
				if strings.Contains(step.Run, "${{") {
					t.Fatal("seal step embeds a GitHub expression in its long run scalar")
				}
				return
			}
			t.Fatal("generated workflow is missing the seal_raw_evidence step")
		})
	}
}

func TestIsolatedDependencyInstallPinsTheTargetWorkingDirectory(t *testing.T) {
	shell := isolatedTargetDependencyShell(nil, false)
	if !strings.Contains(shell, `test "$(pwd -P)" = "$HIVE_TARGET_WORKSPACE"`) {
		t.Fatal("isolated dependency installation does not verify its inherited runner-owned workspace")
	}
	if strings.Contains(shell, `cd "$HIVE_TARGET_WORKSPACE"`) {
		t.Fatal("isolated dependency installation must not re-traverse non-public hosted-runner ancestors")
	}
	account := prepareIsolatedTargetAccountShell()
	for _, required := range []string{
		`target_workspace="/opt/hive-target/workspace"`,
		`sudo mount --bind "$GITHUB_WORKSPACE" "$target_workspace"`,
		`test "$(stat -Lc '%d:%i' "$target_workspace")" = "$(stat -Lc '%d:%i' "$GITHUB_WORKSPACE")"`,
		`echo "HIVE_TARGET_WORKSPACE=$target_workspace" >> "$GITHUB_ENV"`,
	} {
		if !strings.Contains(account, required) {
			t.Fatalf("isolated account setup does not bind the exact checkout through a root-controlled path: missing %q", required)
		}
	}
	if strings.Contains(account, `chmod o+x "$RUNNER_WORKSPACE"`) {
		t.Fatal("isolated account setup widens traversal on the hosted runner workspace")
	}
	for _, prefix := range []string{isolatedTargetEnvPrefix(), isolatedVisualTargetEnvPrefix()} {
		if !strings.Contains(prefix, `env -i -C "$HIVE_TARGET_WORKSPACE"`) {
			t.Fatalf("isolated target environment does not enter the root-controlled bind mount: %s", prefix)
		}
		if !strings.Contains(prefix, `HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE"`) {
			t.Fatalf("isolated target environment does not bind the runner-owned workspace: %s", prefix)
		}
		if strings.Contains(prefix, `-C "$GITHUB_WORKSPACE"`) {
			t.Fatalf("isolated target environment re-enters the runner-private checkout path: %s", prefix)
		}
	}
	if !strings.Contains(verifyImmutableTargetCheckoutShell(), `sudo umount "$target_workspace"`) {
		t.Fatal("repository target cleanup does not unmount the isolated checkout")
	}
	review := runnerOwnedEvidenceRebuildShell(true)
	for _, required := range []string{
		`review_workspace="/opt/hive-target/workspace"`,
		`sudo mount --bind "$runner_workspace" "$review_workspace"`,
		`cd "$review_workspace"`,
		`sudo umount "$review_workspace"`,
	} {
		if !strings.Contains(review, required) {
			t.Fatalf("runner-owned review does not preserve the target evidence root: missing %q", required)
		}
	}
}

func TestSelectedPackageDependencyScopesAreSafeSortedAndDeduplicated(t *testing.T) {
	commands := [][]string{
		{"npm", "--prefix", "packages/zeta", "run", "test"},
		{"npm", "--prefix", "AvProj", "run", "test:django"},
		{"pnpm", "--dir", "AvProj", "run", "check:css"},
		{"yarn", "--cwd", "packages/alpha", "run", "test"},
		{"npm", "run", "test"},
		{"python", "-m", "pytest", "-q"},
	}
	scopes, err := selectedPackageDependencyScopes(commands)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", "AvProj", "packages/alpha", "packages/zeta"}
	if strings.Join(scopes, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("selected package scopes = %v, want %v", scopes, want)
	}

	for _, unsafe := range []string{"../outside", "AvProj/../outside", "/absolute", `C:\absolute`, "AvProj//nested", "AvProj/./nested", " AvProj", "--installer-option"} {
		t.Run(unsafe, func(t *testing.T) {
			_, scopeErr := selectedPackageDependencyScopes([][]string{{"npm", "--prefix", unsafe, "run", "test"}})
			if scopeErr == nil {
				t.Fatalf("unsafe selected package scope %q was accepted", unsafe)
			}
		})
	}
}

func TestIsolatedDependencyInstallUsesOnlySelectedPythonScopesWithoutSourceWrites(t *testing.T) {
	commands := [][]string{
		{"npm", "--prefix", "packages/zeta", "run", "test"},
		{"npm", "--prefix", "AvProj", "run", "test:django"},
		{"npm", "--prefix", "AvProj", "run", "check:css"},
	}
	shell := isolatedTargetDependencyShell(commands, false)
	for _, required := range []string{
		`install_python_scope AvProj`,
		`install_python_scope packages/zeta`,
		`pyproject="$scope/pyproject.toml"`,
		`requirements="$scope/requirements.txt"`,
		`python -m pip install "$scope"`,
		`python -m pip install -r "$requirements"`,
	} {
		if !strings.Contains(shell, required) {
			t.Fatalf("selected Python dependency install omitted %q:\n%s", required, shell)
		}
	}
	if strings.Index(shell, "install_python_scope AvProj") > strings.Index(shell, "install_python_scope packages/zeta") {
		t.Fatal("selected nested Python scopes are not emitted in deterministic order")
	}
	if strings.Count(shell, "install_python_scope AvProj") != 1 {
		t.Fatalf("duplicate selected scope was installed %d times", strings.Count(shell, "install_python_scope AvProj"))
	}
	for _, forbidden := range []string{
		`install_python_scope .`,
		"pip install -e",
		"find . -name requirements",
		"find . -name pyproject",
		"Unselected/requirements.txt",
		"Unselected/pyproject.toml",
	} {
		if strings.Contains(shell, forbidden) {
			t.Fatalf("selected Python dependency install contains forbidden discovery or source-writing form %q", forbidden)
		}
	}
}

func TestRootPythonDependenciesRequireAnExactSelectedRootScope(t *testing.T) {
	for _, commands := range [][][]string{
		{{"npm", "run", "test"}},
		{{"python", "-m", "pytest", "-q"}},
		{{"pytest", "-q"}},
	} {
		shell := isolatedTargetDependencyShell(commands, false)
		if !strings.Contains(shell, `install_python_scope .`) {
			t.Fatalf("selected root command did not preserve root Python dependency behavior: %v", commands)
		}
	}

	nestedOnly := isolatedTargetDependencyShell([][]string{{"npm", "--prefix", "AvProj", "run", "test"}}, false)
	if strings.Contains(nestedOnly, `install_python_scope .`) {
		t.Fatal("nested-only package selection installed an unrelated root Python manifest")
	}
}

func TestGeneratedRepositoryAndVisualJobsInstallExactSelectedPythonScope(t *testing.T) {
	config := isolationWorkflowConfig()
	config.TestCommands = [][]string{{"npm", "--prefix", "AvProj", "run", "test:django:collectstatic"}}
	document := parseIsolatedWorkflow(t, workflow(config))
	for _, jobID := range []string{"repository-test-001", visualExecutionJobName, setupBaselineCaptureJobID} {
		job, exists := document.Jobs[jobID]
		if !exists {
			t.Fatalf("generated workflow is missing %q", jobID)
		}
		found := false
		for _, step := range job.Steps {
			if !strings.Contains(step.Run, "HIVE_TARGET_DEPENDENCIES") {
				continue
			}
			found = true
			if !strings.Contains(step.Run, "install_python_scope AvProj") || !strings.Contains(step.Run, `requirements="$scope/requirements.txt"`) {
				t.Fatalf("job %q did not bind AvProj/requirements.txt through the exact selected scope:\n%s", jobID, step.Run)
			}
			if strings.Contains(step.Run, `install_python_scope .`) {
				t.Fatalf("job %q installed an unrelated root Python manifest for nested-only selection", jobID)
			}
			if strings.Contains(step.Run, "pip install -e") || strings.Contains(step.Run, "find . -name requirements") {
				t.Fatalf("job %q uses source-writing or recursive Python dependency installation", jobID)
			}
		}
		if !found {
			t.Fatalf("job %q lacks the isolated dependency step", jobID)
		}
	}
}

func TestGeneratedWorkflowsIsolateTargetProcessesFromLifecycleAuthority(t *testing.T) {
	config := isolationWorkflowConfig()
	production := workflow(config)
	pullRequest := pullRequestWorkflow(config)
	productionDocument := parseIsolatedWorkflow(t, production)
	pullDocument := parseIsolatedWorkflow(t, pullRequest)

	for _, name := range []string{"repository-test-001", "repository-test-002", visualExecutionJobName, "visual-hive-production", "visual-hive"} {
		if _, exists := productionDocument.Jobs[name]; !exists {
			t.Fatalf("production workflow is missing isolated job %q", name)
		}
	}
	productionExecution := productionDocument.Jobs[visualExecutionJobName]
	productionAggregator := productionDocument.Jobs["visual-hive-production"]
	executionText := workflowJobText(productionExecution)
	aggregatorText := workflowJobText(productionAggregator)
	for _, required := range []string{
		"sudo -u hive-target -- env -i", "sudo chown -R root:root .git", "sudo chmod -R a-w .git",
		"sudo chown root:root visual-hive.config.yaml", "sudo chmod 0444 visual-hive.config.yaml",
		`sudo git -c safe.directory="$GITHUB_WORKSPACE" diff --no-ext-diff --no-textconv --exit-code -- .`, "HIVE_VISUAL_HIVE_CLI_SHA", "test ! -w \"$VISUAL_HIVE_CLI\"",
		"HIVE_TRUSTED_NODE_SHA", `"$HIVE_TRUSTED_NODE" "$VISUAL_HIVE_CLI" pipeline`, `sudo -u hive-target -- test ! -w "$HIVE_TRUSTED_NODE"`,
		"hive.visual-runner-outcome.v1", ".visual-hive/hive-runner-outcome.json", "visual-hive-raw-${{ github.run_id }}",
		`runner_pipeline_exit="$RUNNER_TEMP/hive-visual-pipeline-exit-${GITHUB_RUN_ID}.txt"`,
		`sudo -u hive-evidence -- test ! -e .visual-hive/pipeline-exit-code.txt`,
		`sudo -u hive-evidence -- test -f .visual-hive/hive-runner-outcome.json`,
		`sudo -u hive-evidence -- test ! -w .visual-hive/hive-runner-outcome.json`,
		`sudo -u hive-evidence -- stat -c '%u:%g:%a' .visual-hive/pipeline-exit-code.txt`,
		"raw evidence contains a hard-linked file", `sudo find "$evidence_root" -xdev -type f -exec chmod 0444 -- {} +`,
		"steps.seal_raw_evidence.outcome == 'success'",
	} {
		if !strings.Contains(executionText, required) {
			t.Fatalf("isolated target execution is missing %q", required)
		}
	}
	for name, generated := range map[string]string{"production": production, "pull-request": pullRequest} {
		if !strings.Contains(generated, "id: seal_raw_evidence") {
			t.Fatalf("%s workflow does not bind upload to the successful evidence-seal step", name)
		}
		execution := workflowJobText(parseIsolatedWorkflow(t, generated).Jobs[visualExecutionJobName])
		privilegedDiff := `sudo git -c safe.directory="$GITHUB_WORKSPACE" diff --no-ext-diff --no-textconv --exit-code -- .`
		if count := strings.Count(execution, privilegedDiff); count != 2 {
			t.Fatalf("%s workflow has %d protected-evidence source checks, want 2", name, count)
		}
	}
	for _, forbidden := range []string{"visual-hive-evidence-${{ github.run_id }}", "visual-hive-bundle-${{ github.run_id }}", "--authoritative-for-resolution"} {
		if strings.Contains(executionText, forbidden) {
			t.Fatalf("target execution can emit lifecycle-authority output %q", forbidden)
		}
	}
	if strings.Contains(executionText, `> .visual-hive/pipeline-exit-code.txt`) {
		t.Fatal("runner metadata is still written into the target-owned evidence directory before the authority handoff")
	}
	for _, required := range []string{
		"actions/download-artifact@" + downloadArtifactActionSHA, "Rebuild exact immutable Visual Hive CLI on fresh runner",
		`GITHUB_SHA="$HIVE_VISUAL_HIVE_REF" node scripts/build-release-bundle.mjs --output "$release_dir"`,
		`cli="$trusted_tooling/visual-hive.mjs"`, `manifest.release !== true`, `manifest.clean !== true`,
		"visual-hive-evidence-${{ github.run_id }}", "visual-hive-bundle-${{ github.run_id }}", "--authoritative-for-resolution",
		"authoritative-resolution.txt", "authority_args=()", `pipeline.mode === "full"`, `report.mode === "full"`,
		`runnerOutcome.outcome === "success"`, `plan --config visual-hive.config.yaml --mode full --output .visual-hive/plan.json`,
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN", "Raw evidence contains a symbolic link", "Verify isolated runner prerequisites before trusted publication",
		"isolated Visual Hive execution did not complete successfully", "Stage content-addressed evidence root",
		`evidence_stage="$RUNNER_TEMP/hive-visual-hive-evidence-${GITHUB_RUN_ID}"`,
		`test -f "$evidence_stage/.visual-hive/artifacts-index.json"`,
	} {
		if !strings.Contains(aggregatorText, required) {
			t.Fatalf("runner-owned production aggregator is missing %q", required)
		}
	}
	if strings.Contains(aggregatorText, `--scan-scope full --authoritative-for-resolution`) {
		t.Fatal("production bundle still receives unconditional resolution authority")
	}
	if !strings.Contains(production, `path: ${{ runner.temp }}/hive-visual-hive-evidence-${{ github.run_id }}`) {
		t.Fatal("content-addressed evidence upload does not preserve the staged .visual-hive source root")
	}
	if strings.Contains(production, "path: |\n            .visual-hive") {
		t.Fatal("content-addressed evidence upload flattens the required .visual-hive source root")
	}
	receiptIndex := strings.Index(executionText, `sudo install -o root -g root -m 0444 "$runner_outcome" .visual-hive/hive-runner-outcome.json`)
	uploadIndex := strings.Index(executionText, "Upload isolated raw Visual evidence")
	if receiptIndex < 0 || uploadIndex < 0 || receiptIndex > uploadIndex {
		t.Fatalf("runner-owned pipeline outcome receipt is not root-created after target termination and before raw upload: receipt=%d upload=%d", receiptIndex, uploadIndex)
	}
	for _, forbidden := range []string{"sudo -u hive-target", "npm --prefix \"$(dirname \"$lockfile\")\" ci", "target_visual_pipeline"} {
		if strings.Contains(aggregatorText, forbidden) {
			t.Fatalf("runner-owned production aggregator executes target code %q", forbidden)
		}
	}
	for _, need := range []string{visualExecutionJobName, "repository-test-001", "repository-test-002"} {
		if !contains(productionAggregator.Needs, need) {
			t.Fatalf("production aggregator does not need isolated job %q: %v", need, productionAggregator.Needs)
		}
	}

	for _, name := range []string{"repository-test-001", "repository-test-002", visualExecutionJobName, "visual-hive"} {
		job, exists := pullDocument.Jobs[name]
		if !exists {
			t.Fatalf("pull-request workflow is missing isolated job %q", name)
		}
		if name != "visual-hive" && !strings.Contains(job.If, "github.event_name == 'pull_request'") {
			t.Fatalf("target job %q is not explicitly pull_request-only: %q", name, job.If)
		}
	}
	pullAggregator := pullDocument.Jobs["visual-hive"]
	if !strings.Contains(pullAggregator.If, "github.event_name == 'pull_request'") || strings.Contains(pullAggregator.If, "pull_request_target") || strings.Contains(pullAggregator.If, "operation == 'uninstall'") {
		t.Fatalf("protected aggregator must be pull_request-only: %q", pullAggregator.If)
	}
	for _, step := range pullAggregator.Steps {
		if step.Name == "Enforce deterministic verdict" || strings.TrimSpace(step.Run) == "" && strings.TrimSpace(step.Uses) == "" {
			continue
		}
		if (step.Uses != "" || step.Run != "") && !strings.Contains(step.If, "operation != 'uninstall'") && !strings.Contains(step.If, "operation == ''") {
			t.Fatalf("aggregator step %q could execute proposed code during pull_request_target: if=%q", step.Name, step.If)
		}
	}
	if strings.Count(pullRequest, "visual-hive-pr-raw-${{ github.run_id }}") != 2 || strings.Count(pullRequest, "name: visual-hive-pr\n") != 1 {
		t.Fatal("PR raw and final artifacts are not separated into one producer/consumer boundary")
	}
	for _, required := range []string{
		`--mode pr --changed-files "$HIVE_CHANGED_FILES" --runtime-sidecar .visual-hive/proof/pr/runtime.json`,
		`GITHUB_ACTIONS=true GITHUB_REPOSITORY="$HIVE_TARGET_REPOSITORY" GITHUB_HEAD_REF="$HIVE_TARGET_HEAD_REF" GITHUB_BASE_REF="$HIVE_TARGET_BASE_REF" GITHUB_SHA="$HIVE_TARGET_HEAD_SHA" GITHUB_EVENT_NAME=pull_request`,
		`GITHUB_RUN_ID="$HIVE_TARGET_RUN_ID" GITHUB_RUN_ATTEMPT="$HIVE_TARGET_RUN_ATTEMPT" GITHUB_WORKFLOW="$HIVE_TARGET_WORKFLOW"`,
		`node "$VISUAL_HIVE_CLI" plan --config visual-hive.config.yaml --mode pr --changed-files .visual-hive/changed-files.pr.txt`,
		`schemaVersion: "hive.visual-hive-pr-source.v2"`,
		`GITHUB_SHA="$HIVE_HEAD_SHA" GITHUB_REF="refs/heads/$HIVE_HEAD_REF" GITHUB_EVENT_NAME=pull_request`,
		`--scan-scope changed-files "${contract_args[@]}" "${file_args[@]}"`,
		`--issues .visual-hive/proof/pr/issues.present.json`,
		`issue.status === "open_candidate" || issue.status === "update_candidate"`,
		`name: visual-hive-pr-source-${{ github.run_id }}-${{ github.run_attempt }}`,
		`name: visual-hive-pr-bundle-${{ github.run_id }}-${{ github.run_attempt }}`,
		`.visual-hive/proof/pr/inputs/visual-hive.config.yaml`,
		`.visual-hive/proof/pr/inputs/visual-hive-pr.yml`,
		`git show "$HIVE_BASE_SHA:.github/workflows/visual-hive-pr.yml" > "$workflow_snapshot"`,
		`export HIVE_BASE_WORKFLOW_SHA256="$base_workflow_sha256"`,
		`.visual-hive/proof/pr/baselines/`,
		`test ! -e .visual-hive/proof/pr/source-binding.json`,
		`["diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, head]`,
		`Changed-file path cannot be represented exactly in the Visual Hive scope`,
		`review_stage="$RUNNER_TEMP/hive-visual-hive-pr-review-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"`,
		`path: ${{ runner.temp }}/hive-visual-hive-pr-review-${{ github.run_id }}-${{ github.run_attempt }}/.visual-hive`,
	} {
		if !strings.Contains(pullRequest, required) {
			t.Fatalf("PR source/bundle lane is missing %q", required)
		}
	}
	if strings.Contains(workflowJobText(pullAggregator), "--trusted-source") || strings.Contains(workflowJobText(pullAggregator), "--authoritative-for-resolution") ||
		strings.Contains(workflowJobText(pullAggregator), "${{ github.sha }}") || strings.Contains(workflowJobText(pullAggregator), `copySealed(workflowPath`) ||
		strings.Contains(workflowJobText(pullAggregator), `hive bundle --config visual-hive.config.yaml --issues .visual-hive/issues.json`) {
		t.Fatal("PR source/bundle lane acquired trust, resolution authority, or merge-SHA input")
	}
	if strings.Count(pullRequest, `"--name-only", "-z"`) != 2 || strings.Contains(pullRequest, `--name-only "$HIVE_BASE_SHA" "$HIVE_HEAD_SHA" >`) {
		t.Fatal("PR changed-file scope is not captured twice from exact NUL-delimited Git paths")
	}
	ordered := []string{
		"name: Enforce deterministic verdict", "name: Stage immutable legacy pull request review evidence", "name: Prepare admissible pull request source binding", "name: Stage content-addressed pull request source",
		"name: Upload independently verifiable pull request source", "name: Build non-authoritative pull request bundle",
		"name: Upload pull request bundle", "name: Upload review evidence",
	}
	previous := -1
	for _, marker := range ordered {
		index := strings.Index(pullRequest, marker)
		if index <= previous {
			t.Fatalf("PR source/bundle/review order is unsafe at %q: prior=%d current=%d", marker, previous, index)
		}
		previous = index
	}
	for _, stepName := range []string{"Prepare admissible pull request source binding", "Stage content-addressed pull request source", "Upload independently verifiable pull request source", "Build non-authoritative pull request bundle", "Upload pull request bundle"} {
		found := false
		for _, step := range pullAggregator.Steps {
			if step.Name != stepName {
				continue
			}
			found = true
			expected := "${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation == ''"
			if stepName != "Prepare admissible pull request source binding" {
				expected += " && steps.pr_source.outputs.admissible == 'true'"
			}
			expected += " }}"
			if step.If != expected || strings.Contains(step.If, "pull_request_target") || strings.Contains(step.If, "operation != 'uninstall'") {
				t.Fatalf("PR bundle step %q is not strictly admissibility/event gated: %q", stepName, step.If)
			}
		}
		if !found {
			t.Fatalf("PR bundle step %q is missing", stepName)
		}
	}
	for _, operation := range []string{"setup", "upgrade", "rollback", "authorizer-transfer", "uninstall"} {
		if operation == "" {
			t.Fatalf("hostile managed operation fixture unexpectedly empty")
		}
		if strings.Contains(workflowJobText(pullAggregator), fmt.Sprintf("outputs.operation == %q", operation)) {
			t.Fatalf("managed operation %q can select the PR v3 lane", operation)
		}
	}
	if !strings.Contains(pullRequest, `HIVE_SETUP_OPERATION: ${{ needs.setup-authorization.outputs.operation }}`) ||
		!strings.Contains(workflowJobText(pullAggregator), `test -z "$HIVE_SETUP_OPERATION"`) {
		t.Fatal("PR source preflight does not independently reject every managed setup operation")
	}
}

func TestGeneratedPullRequestExecutionUsesDistinctEvidenceAuthority(t *testing.T) {
	generated := pullRequestWorkflow(isolationWorkflowConfig())
	execution := parseIsolatedWorkflow(t, generated).Jobs[visualExecutionJobName]
	executionText := workflowJobText(execution)
	for _, required := range []string{
		"sudo useradd --create-home --shell /usr/sbin/nologin hive-evidence",
		`sudo install -d -o hive-evidence -g hive-evidence -m 0700 "$evidence_root"`,
		"Target principal can access the protected evidence root",
		"/usr/bin/unshare --mount --fork /opt/hive-target/trusted/run-visual-hive-evidence",
		"runtime_modules=/opt/hive-target/trusted/visual-hive-runtime/node_modules",
		`case "$runtime_modules" in`,
		`"$evidence_root"|"$evidence_root"/*) echo "Visual Hive runtime modules entered the evidence root"`,
		"mount --bind /opt/hive-target/trusted/visual-hive-tooling/node_modules \"$runtime_modules\"",
		`"NODE_PATH=$runtime_modules"`,
		"exec /usr/bin/sudo -n -u hive-target -- /usr/bin/env -i",
		"exec /usr/bin/sudo -n -u hive-evidence -- /usr/bin/env -i",
		"hive-visual-target-child-shell",
		`test "$(sudo stat -c '%u:%g:%a:%s' "$execution_key")" = "0:0:400:32"`,
		"Execution authority key is readable outside the runner root principal",
		"protected evidence contains an entry from another principal",
		"An isolated execution principal remained live before evidence authentication",
		"hive.visual-hive-runner-attestation.v1",
		"hive.visual-hive-runner-execution.v1",
		"crypto.createHmac(\"sha256\", key).update(payloadBytes)",
	} {
		if !strings.Contains(executionText, required) {
			t.Fatalf("PR execution does not enforce the distinct evidence authority invariant %q", required)
		}
	}
	if strings.Contains(executionText, `runtime_modules="$evidence_root/node_modules"`) ||
		strings.Contains(executionText, `sudo install -d -o hive-evidence -g hive-evidence -m 0700 "$evidence_root" "$evidence_root/node_modules"`) {
		t.Fatal("Visual Hive runtime dependencies still enter the complete evidence root")
	}
	var pipeline string
	for _, step := range execution.Steps {
		if step.Name == "Run target-facing Visual Hive collection" {
			pipeline = step.Run
			break
		}
	}
	if !strings.Contains(pipeline, "/usr/bin/unshare --mount --fork "+isolatedEvidenceLauncher+` "$HIVE_TRUSTED_NODE" "$VISUAL_HIVE_CLI" pipeline`) ||
		strings.Contains(pipeline, isolatedVisualPullRequestTargetEnvPrefix()+` "$HIVE_TRUSTED_NODE" "$VISUAL_HIVE_CLI" pipeline`) {
		t.Fatalf("Visual Hive collection is not launched by the evidence principal boundary:\n%s", pipeline)
	}
	source := pullRequestSourceBindingScript()
	for _, required := range []string{
		`schemaVersion: "hive.visual-hive-pr-source.v2"`,
		`readJSON(".visual-hive/proof/pr/runner-execution-attestation.json", "runner execution attestation")`,
		`crypto.timingSafeEqual(providedMac, expectedMac)`,
		`authenticatedPayloadBytes.equals(Buffer.from(JSON.stringify(authenticatedPayload), "utf8"))`,
		`runnerAttestation: attestationIdentity`,
		`fs.readFileSync(path.join(root, ".visual-hive/pipeline-exit-code.txt"), "utf8") !== "0\n"`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("PR source binding does not cryptographically verify %q", required)
		}
	}
	if strings.Index(executionText, "An isolated execution principal remained live before evidence authentication") > strings.Index(executionText, "hive.visual-hive-runner-attestation.v1") {
		t.Fatal("runner authentication occurs before both untrusted execution principals are quiesced")
	}
}

func TestGeneratedEvidenceLauncherUsesPrivateUmask(t *testing.T) {
	generated := prepareIsolatedVisualEvidenceRuntimeShell(true)
	launcherPrefix := "HIVE_EVIDENCE_LAUNCHER'\n#!" + isolatedTargetBash + "\nset -euo pipefail\numask 077\n"
	if !strings.Contains(generated, launcherPrefix) {
		t.Fatal("isolated evidence launcher does not force private permissions for newly collected evidence")
	}
	postDropExec := "exec /usr/bin/sudo -n -u " + isolatedEvidenceAccount + ` -- /usr/bin/env -i -C "$HIVE_TARGET_WORKSPACE" "${evidence_environment[@]}" ` + isolatedTargetBash + ` --noprofile --norc -c 'umask 077; exec "$@"' hive-visual-evidence "$@"`
	if !strings.Contains(generated, postDropExec) {
		t.Fatal("isolated evidence launcher does not restore its private umask after sudo changes principal")
	}
}

func TestGeneratedVisualExecutionPreparesEvidenceAfterBrowserHandoff(t *testing.T) {
	production := workflow(isolationWorkflowConfig())
	workflows := []struct {
		name      string
		generated string
		job       string
	}{
		{name: "production", generated: production, job: visualExecutionJobName},
		{name: "setup-baseline-capture", generated: production, job: setupBaselineCaptureJobID},
		{name: "pull-request", generated: pullRequestWorkflow(isolationWorkflowConfig()), job: visualExecutionJobName},
	}
	const prepareStepName = "Prepare isolated Visual Hive evidence root"
	const scopedClean = `sudo git -c safe.directory="$HIVE_TARGET_WORKSPACE" -C "$HIVE_TARGET_WORKSPACE" clean -ffdx -- .visual-hive`
	for _, workflow := range workflows {
		t.Run(workflow.name, func(t *testing.T) {
			execution := parseIsolatedWorkflow(t, workflow.generated).Jobs[workflow.job]
			executionText := workflowJobText(execution)
			for _, required := range []string{
				"sudo install -d -o root -g root -m 0555 /opt/hive-target/trusted/visual-hive-runtime /opt/hive-target/trusted/visual-hive-runtime/node_modules",
				"runtime_modules=/opt/hive-target/trusted/visual-hive-runtime/node_modules",
				"mount --bind /opt/hive-target/trusted/visual-hive-tooling/node_modules \"$runtime_modules\"",
				`mount -o remount,bind,ro "$runtime_modules"`,
				`"PATH=$runtime_modules/.bin:/usr/local/bin:/usr/bin:/bin"`,
				`"NODE_PATH=$runtime_modules"`,
			} {
				if !strings.Contains(executionText, required) {
					t.Fatalf("%s execution does not keep immutable runtime modules outside complete evidence: missing %q", workflow.name, required)
				}
			}
			if strings.Contains(executionText, `runtime_modules="$evidence_root/node_modules"`) ||
				strings.Contains(executionText, `mount --bind /opt/hive-target/trusted/visual-hive-tooling/node_modules "$evidence_root/node_modules"`) {
				t.Fatalf("%s execution still mounts runtime dependencies inside complete evidence", workflow.name)
			}
			preflightIndex := -1
			prepareIndex := -1
			collectionIndex := -1
			prepareCount := 0
			prepareRun := ""
			for index, step := range execution.Steps {
				switch step.Name {
				case "Install target dependencies in isolated account":
					if strings.Contains(step.Run, scopedClean) || strings.Contains(step.Run, `evidence_root="$HIVE_TARGET_WORKSPACE/.visual-hive"`) {
						t.Fatal("dependency installation still prepares the protected evidence root")
					}
				case "Verify sealed Playwright browser handoff":
					preflightIndex = index
				case prepareStepName:
					prepareIndex = index
					prepareCount++
					prepareRun = step.Run
				case "Run target-facing Visual Hive collection":
					collectionIndex = index
				}
			}
			if prepareCount != 1 || preflightIndex+1 != prepareIndex || prepareIndex+1 != collectionIndex {
				t.Fatalf("evidence preparation is not one distinct step immediately between browser verification and collection: preflight=%d prepare=%d collection=%d count=%d", preflightIndex, prepareIndex, collectionIndex, prepareCount)
			}
			for _, required := range []string{
				`evidence_root="$HIVE_TARGET_WORKSPACE/.visual-hive"`,
				scopedClean,
				`sudo find "$evidence_root" -type l -print -quit`,
				`sudo find "$evidence_root" ! -type d ! -type f -print -quit`,
				`sudo chown -R hive-evidence:hive-evidence "$evidence_root"`,
				`sudo install -d -o hive-evidence -g hive-evidence -m 0700 "$evidence_root"`,
			} {
				if !strings.Contains(prepareRun, required) {
					t.Fatalf("evidence preparation step is missing %q", required)
				}
			}
			if strings.Contains(prepareRun, `evidence_root="$GITHUB_WORKSPACE/.visual-hive"`) || strings.Contains(prepareRun, "rm -rf") {
				t.Fatalf("evidence preparation does not preserve the tracked evidence tree:\n%s", prepareRun)
			}
			if strings.Contains(prepareRun, `sudo install -d -o hive-evidence -g hive-evidence -m 0700 "$evidence_root" "$evidence_root/node_modules"`) {
				t.Fatalf("evidence preparation still creates a runtime mountpoint inside the complete artifact root:\n%s", prepareRun)
			}
		})
	}
}

func TestTrustedCollectorNodeCannotBeShadowedByTargetPath(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	shadow := filepath.Join(root, "shadow")
	if err := os.MkdirAll(shadow, 0o755); err != nil {
		t.Fatal(err)
	}
	trustedNode := filepath.Join(root, "trusted-node")
	writeFixture(t, root, "trusted-node", "#!/usr/bin/env bash\nprintf 'trusted\\n'\n")
	writeFixture(t, shadow, "node", "#!/usr/bin/env bash\nprintf 'shadow\\n'\n")
	if err := os.Chmod(trustedNode, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(shadow, "node"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Invoke the captured absolute runtime directly. Generated workflow shell
	// syntax is checked separately by TestGeneratedIsolationShellsAreExecutable;
	// avoiding `bash -c` here also keeps the assertion portable through Git for
	// Windows' command-line quoting layer.
	command := exec.Command(bash, bashFilesystemPath(trustedNode))
	command.Env = append(os.Environ(), "PATH="+bashFilesystemPath(shadow)+":/usr/bin:/bin")
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "trusted" {
		t.Fatalf("target PATH shadow replaced the absolute trusted node: err=%v output=%q", err, output)
	}
}

func TestGeneratedPullRequestSourceBindingRoundTripsStrictGoSchema(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to execute the generated PR source-binding producer")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is required to freeze the base-side PR workflow definition")
	}
	bash := workflowBash(t)
	workflow := pullRequestWorkflow(isolationWorkflowConfig())
	aggregator := parseIsolatedWorkflow(t, workflow).Jobs["visual-hive"]
	var preparation string
	for _, step := range aggregator.Steps {
		if step.Name == "Prepare admissible pull request source binding" {
			preparation = step.Run
			break
		}
	}
	const startMarker = "node <<'NODE'\n"
	start := strings.Index(preparation, startMarker)
	end := -1
	if start >= 0 {
		start += len(startMarker)
		end = strings.Index(preparation[start:], "\nNODE")
	}
	if start < len(startMarker) || end < 0 {
		t.Fatalf("generated admissibility step does not contain one extractable source-binding producer:\n%s", preparation)
	}
	producer := preparation[start : start+end]
	if producer != pullRequestSourceBindingScript() {
		t.Fatal("generated workflow source-binding producer diverges from its tested implementation")
	}
	if !strings.Contains(preparation, pullRequestBaseWorkflowSnapshotShell()) {
		t.Fatal("generated workflow does not freeze the tested base-side workflow definition")
	}

	const baseWorkflowDefinition = "name: Trusted Base Visual Hive PR\non:\n  pull_request:\n"
	const hostileHeadWorkflowDefinition = "name: Head Must Not Authorize Itself\non:\n  pull_request_target:\n"
	const generatedSpecPath = ".visual-hive/generated/visual-hive.generated.spec.mjs"
	const generatedConfigPath = ".visual-hive/generated/visual-hive.generated.config.cjs"
	const generatedSpec = "// exact generated Playwright spec\n"
	const generatedConfig = "module.exports = { reporter: 'json' };\n"
	const specialUserAgent = "fixture-agent&separator\u2028"
	const specialFont = "A<B>"
	const goCanonicalStableRuntime = `{"browser":{"name":"chromium","version":"123.0.0"},"environment":{"os":"linux","architecture":"x64","nodeVersion":"v22.23.1","playwrightVersion":"1.58.2","locale":"en-US","timezone":"UTC","userAgent":"fixture-agent\u0026separator\u2028","deviceScaleFactor":1,"fonts":[{"name":"A\u003cB\u003e","available":true}]}}`

	type fixtureOptions struct {
		noOp             bool
		omitRuntime      bool
		mismatchRuntime  bool
		generatedDrift   bool
		workflowDrift    bool
		invalidBaseline  bool
		baselinePath     string
		missingPRNumber  bool
		reportPRNumber   int
		failedReport     bool
		outsideSpecPath  bool
		spacedChanged    bool
		mixedIgnored     bool
		omittedIgnored   bool
		extraIgnored     bool
		planContracts    string
		evaluationScopes string
		resultTarget     string
		headRef          string
		producerCommit   string
		tamperAttested   bool
		tamperMAC        bool
	}
	run := func(t *testing.T, options fixtureOptions) (string, string, []byte, error) {
		t.Helper()
		root := t.TempDir()
		runnerTemp := filepath.Join(root, "runner-temp")
		if err := os.MkdirAll(runnerTemp, 0o700); err != nil {
			t.Fatal(err)
		}
		runGit := func(args ...string) string {
			t.Helper()
			command := exec.Command(git, args...)
			command.Dir = root
			output, commandErr := command.CombinedOutput()
			if commandErr != nil {
				t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), commandErr, output)
			}
			return strings.TrimSpace(string(output))
		}
		runGit("init", "-q")
		runGit("config", "core.autocrlf", "false")
		writeFixture(t, root, ".github/workflows/visual-hive-pr.yml", baseWorkflowDefinition)
		runGit("add", ".github/workflows/visual-hive-pr.yml")
		runGit("-c", "user.name=Hive Test", "-c", "user.email=hive@example.invalid", "commit", "-q", "-m", "trusted base workflow")
		baseSHA := runGit("rev-parse", "HEAD")
		writeFixture(t, root, ".github/workflows/visual-hive-pr.yml", hostileHeadWorkflowDefinition)
		runGit("add", ".github/workflows/visual-hive-pr.yml")
		runGit("-c", "user.name=Hive Test", "-c", "user.email=hive@example.invalid", "commit", "-q", "-m", "hostile head workflow")
		headSHA := runGit("rev-parse", "HEAD")

		writeFixture(t, root, "visual-hive.config.yaml", "schemaVersion: 1\nproject:\n  name: fixture\n")
		changedFileContents := "web/app.ts\n"
		if options.mixedIgnored || options.omittedIgnored {
			changedFileContents = "docs/guide.md\nweb/app.ts\n"
		}
		if options.spacedChanged {
			changedFileContents = " web/app.ts\n"
		}
		writeFixture(t, root, ".visual-hive/changed-files.pr.txt", changedFileContents)
		writeFixture(t, root, "testdata/baseline.png", "\x89PNG\r\n\x1a\nfixed-baseline-bytes\n")
		writeFixture(t, root, "testdata/Z-baseline.png", "\x89PNG\r\n\x1a\nsecond-fixed-baseline-bytes\n")
		if options.invalidBaseline {
			writeFixture(t, root, "testdata/baseline.png", "not-a-png\n")
		}
		writeFixture(t, root, generatedSpecPath, generatedSpec)
		writeFixture(t, root, generatedConfigPath, generatedConfig)
		binding := map[string]string{
			"bindingMacSha256":      strings.Repeat("a", 64),
			"generatedConfigSha256": fmt.Sprintf("%x", sha256.Sum256([]byte(generatedConfig))),
			"generatedSpecSha256":   fmt.Sprintf("%x", sha256.Sum256([]byte(generatedSpec))),
			"nonceSha256":           strings.Repeat("d", 64),
			"payloadSha256":         strings.Repeat("e", 64),
		}
		planItems := []any{map[string]any{"contractId": "contract-a", "targetId": "target-a"}}
		selected := []string{"contract-a"}
		results := []any{map[string]any{
			"contractId": "contract-a", "targetId": "target-a", "status": "passed",
			"screenshotAssertions": []any{
				map[string]any{"baselinePath": "testdata/baseline.png", "status": "passed"},
				map[string]any{"baselinePath": "testdata/Z-baseline.png", "status": "passed"},
			},
		}}
		if options.baselinePath != "" {
			results[0].(map[string]any)["screenshotAssertions"].([]any)[0].(map[string]any)["baselinePath"] = options.baselinePath
		}
		evaluated := "contract-a\n"
		reportBinding := any(binding)
		if options.noOp {
			planItems, selected, results, evaluated, reportBinding = []any{}, []string{}, []any{}, "", nil
		}
		planContractReceipt := evaluated
		if options.planContracts != "" {
			planContractReceipt = options.planContracts
		}
		evaluationScopeReceipt := evaluated
		if evaluated != "" {
			evaluationScopeReceipt += "provider-governance\nworkflow-safety\n"
		}
		if options.evaluationScopes != "" {
			evaluationScopeReceipt = options.evaluationScopes
		}
		if options.resultTarget != "" && len(results) > 0 {
			results[0].(map[string]any)["targetId"] = options.resultTarget
		}
		planChangedFiles := []string{"web/app.ts"}
		reportChangedFiles := []string{"web/app.ts"}
		effectiveChangedFiles := []string{"web/app.ts"}
		ignoredChangedFiles := []any{}
		if options.mixedIgnored || options.omittedIgnored {
			planChangedFiles = []string{"docs/guide.md", "web/app.ts"}
			reportChangedFiles = []string{"docs/guide.md", "web/app.ts"}
			if options.mixedIgnored {
				ignoredChangedFiles = []any{map[string]any{"file": "docs/guide.md", "pattern": "docs/**", "reason": "documentation-only change"}}
			}
		}
		if options.extraIgnored {
			ignoredChangedFiles = []any{map[string]any{"file": "docs/guide.md", "pattern": "docs/**", "reason": "documentation-only change"}}
		}
		plan := map[string]any{
			"schemaVersion": 1, "mode": "pr", "changedFiles": planChangedFiles, "effectiveChangedFiles": effectiveChangedFiles,
			"ignoredChangedFiles": ignoredChangedFiles, "items": planItems, "excluded": []any{},
		}
		reportedSpecPath := generatedSpecPath
		if options.outsideSpecPath {
			reportedSpecPath = ".visual-hive/proof/pr/runtime.json"
		}
		reportStatus := "passed"
		if options.failedReport {
			reportStatus = "failed"
		}
		reportRepository := map[string]any{
			"provider": "github-actions", "repository": "owner/repo", "branch": "feature/exact-head", "baseBranch": "main",
			"commitSha": headSHA, "pullRequestNumber": 7, "runId": "77", "runAttempt": "2", "workflow": "Visual Hive PR",
		}
		if options.missingPRNumber {
			delete(reportRepository, "pullRequestNumber")
		} else if options.reportPRNumber != 0 {
			reportRepository["pullRequestNumber"] = options.reportPRNumber
		}
		report := map[string]any{
			"schemaVersion": 2, "mode": "pr", "status": reportStatus, "changedFiles": reportChangedFiles, "selectedContracts": selected, "results": results,
			"generatedSpecPath": reportedSpecPath,
			"repository":        reportRepository,
		}
		if reportBinding != nil {
			report["executionBinding"] = reportBinding
		}
		writeJSON := func(relative string, value any) {
			data, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			writeFixture(t, root, relative, string(data)+"\n")
		}
		writeJSON(".visual-hive/plan.json", plan)
		writeJSON(".visual-hive/report.json", report)
		writeFixture(t, root, ".visual-hive/evaluated-plan-contracts.txt", planContractReceipt)
		writeFixture(t, root, ".visual-hive/evaluated-contracts.txt", evaluationScopeReceipt)
		if !options.noOp && !options.omitRuntime {
			runtimeBinding := maps.Clone(binding)
			if options.mismatchRuntime {
				runtimeBinding["bindingMacSha256"] = strings.Repeat("f", 64)
			}
			writeJSON(".visual-hive/proof/pr/runtime.json", map[string]any{
				"schemaVersion": "visual-hive.playwright-runtime.v1", "capturedAt": "2026-07-16T12:00:00.000Z", "executionBinding": runtimeBinding,
				"browser": map[string]any{"name": "chromium", "version": "123.0.0"},
				"environment": map[string]any{
					"os": "linux", "architecture": "x64", "nodeVersion": "v22.23.1", "playwrightVersion": "1.58.2",
					"locale": "en-US", "timezone": "UTC", "userAgent": specialUserAgent, "deviceScaleFactor": 1,
					"fonts": []any{map[string]any{"name": specialFont, "available": true}},
				},
			})
		}
		writeFixture(t, root, ".visual-hive/pipeline-exit-code.txt", "0\n")
		writeJSON(".visual-hive/hive-runner-outcome.json", map[string]any{
			"schemaVersion": "hive.visual-runner-outcome.v1", "outcome": "success", "conclusion": "success",
		})
		attestationRelative := runnerAttestationPath
		if !options.noOp && !options.omitRuntime && !options.mismatchRuntime && !options.outsideSpecPath {
			key := []byte("0123456789abcdef0123456789abcdef")
			keyPath := filepath.Join(runnerTemp, "execution-authority.key")
			if err := os.WriteFile(keyPath, key, 0o600); err != nil {
				t.Fatal(err)
			}
			attestation := exec.Command(node, "-e", runnerExecutionAttestationScript())
			attestation.Dir = root
			attestation.Env = append(os.Environ(),
				"HIVE_EXECUTION_AUTHORITY_KEY="+keyPath,
				fmt.Sprintf("HIVE_EXECUTION_AUTHORITY_KEY_COMMITMENT=%x", sha256.Sum256(key)),
				"HIVE_RUNNER_ATTESTATION_OUTPUT="+filepath.Join(root, filepath.FromSlash(attestationRelative)),
				"HIVE_TARGET_REPOSITORY=owner/repo", "HIVE_TARGET_PULL_REQUEST=7", "HIVE_TARGET_BASE_REF=main",
				"HIVE_TARGET_HEAD_REF=feature/exact-head", "HIVE_TARGET_HEAD_SHA="+headSHA,
				"HIVE_TARGET_RUN_ID=77", "HIVE_TARGET_RUN_ATTEMPT=2", "HIVE_TARGET_WORKFLOW=Visual Hive PR",
				"HIVE_PRODUCER_COMMIT="+visualHivePullRequestProducerCommit,
			)
			if output, attestationErr := attestation.CombinedOutput(); attestationErr != nil {
				t.Fatalf("runner execution attestation fixture failed: %v\n%s", attestationErr, output)
			}
			if options.tamperMAC {
				data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(attestationRelative)))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var document map[string]any
				if err := json.Unmarshal(data, &document); err != nil {
					t.Fatal(err)
				}
				document["macSha256"] = strings.Repeat("0", 64)
				writeJSON(attestationRelative, document)
			}
		}
		artifactIdentity := func(relative string) map[string]any {
			data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
			if readErr != nil {
				t.Fatal(readErr)
			}
			return map[string]any{"path": relative, "bytes": len(data), "sha256": fmt.Sprintf("%x", sha256.Sum256(data))}
		}
		artifacts := []any{
			artifactIdentity(generatedSpecPath), artifactIdentity(generatedConfigPath),
			artifactIdentity(".visual-hive/pipeline-exit-code.txt"), artifactIdentity(".visual-hive/hive-runner-outcome.json"),
		}
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(attestationRelative))); statErr == nil {
			artifacts = append(artifacts, artifactIdentity(attestationRelative))
		}
		writeJSON(".visual-hive/artifacts-index.json", map[string]any{
			"schemaVersion": 1, "contentAddressed": true, "complete": true, "artifacts": artifacts,
		})
		if options.generatedDrift {
			writeFixture(t, root, generatedSpecPath, "// drifted after artifact indexing\n")
		}
		if options.tamperAttested {
			reportPath := filepath.Join(root, ".visual-hive", "report.json")
			handle, openErr := os.OpenFile(reportPath, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
			if _, appendErr := handle.WriteString(" \n"); appendErr != nil {
				_ = handle.Close()
				t.Fatal(appendErr)
			}
			if closeErr := handle.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		snapshot := exec.Command(bash, "-c", pullRequestBaseWorkflowSnapshotShell())
		snapshot.Dir = root
		snapshot.Env = append(os.Environ(),
			"HIVE_BASE_SHA="+baseSHA,
			"HIVE_WORKFLOW_PATH=.github/workflows/visual-hive-pr.yml",
		)
		if output, snapshotErr := snapshot.CombinedOutput(); snapshotErr != nil {
			t.Fatalf("base-side workflow freeze failed: %v\n%s", snapshotErr, output)
		}
		if options.workflowDrift {
			writeFixture(t, root, visualhive.PullRequestWorkflowPath, hostileHeadWorkflowDefinition)
		}
		statePath := filepath.Join(runnerTemp, "admissible.txt")
		headRef := options.headRef
		if headRef == "" {
			headRef = "feature/exact-head"
		}
		producerCommit := options.producerCommit
		if producerCommit == "" {
			producerCommit = visualHivePullRequestProducerCommit
		}
		command := exec.Command(node, "-e", producer)
		command.Dir = root
		command.Env = append(os.Environ(),
			"RUNNER_TEMP="+runnerTemp, "HIVE_PR_ADMISSIBILITY_PATH="+statePath,
			"HIVE_EVENT_NAME=pull_request", "HIVE_REPOSITORY=owner/repo", "HIVE_PULL_REQUEST_NUMBER=7",
			"HIVE_BASE_REPOSITORY=owner/repo", "HIVE_BASE_REPOSITORY_ID=123", "HIVE_BASE_REF=main", "HIVE_BASE_SHA="+baseSHA,
			"HIVE_HEAD_REPOSITORY=fork/repo", "HIVE_HEAD_REPOSITORY_ID=456", "HIVE_HEAD_REF="+headRef, "HIVE_HEAD_SHA="+headSHA,
			"HIVE_WORKFLOW_RUN_ID=77", "HIVE_WORKFLOW_RUN_ATTEMPT=2", "HIVE_WORKFLOW_NAME=Visual Hive PR", "HIVE_WORKFLOW_PATH=.github/workflows/visual-hive-pr.yml",
			fmt.Sprintf("HIVE_BASE_WORKFLOW_SHA256=%x", sha256.Sum256([]byte(baseWorkflowDefinition))),
			"HIVE_PRODUCER_COMMIT="+producerCommit, "HIVE_SOURCE_ARTIFACT_NAME=visual-hive-pr-source-77-2", "HIVE_BUNDLE_ARTIFACT_NAME=visual-hive-pr-bundle-77-2",
		)
		output, runErr := command.CombinedOutput()
		state, _ := os.ReadFile(statePath)
		return root, strings.TrimSpace(string(state)), output, runErr
	}

	t.Run("hostile head workflow cannot self-authorize strict round trip", func(t *testing.T) {
		root, state, output, runErr := run(t, fixtureOptions{})
		if runErr != nil || state != "true" {
			t.Fatalf("generated source binding failed: %v state=%q\n%s", runErr, state, output)
		}
		data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(visualhive.PullRequestSourceBindingPath)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		binding, parseErr := visualhive.ParsePullRequestSourceBinding(data)
		if parseErr != nil {
			t.Fatalf("generated binding does not round-trip through the normative strict Go parser: %v\n%s", parseErr, data)
		}
		if binding.SchemaVersion != visualhive.PullRequestSourceBindingSchema || binding.RepositoryID != "123" || binding.PullRequest != 7 ||
			binding.Workflow.Definition.Path != visualhive.PullRequestWorkflowPath || binding.Config.Path != visualhive.PullRequestConfigPath ||
			binding.Runtime.Receipt.Path != visualhive.PullRequestRuntimePath || binding.Runtime.GeneratedSpec.Path != generatedSpecPath ||
			binding.Runtime.GeneratedConfig.Path != generatedConfigPath || binding.Runtime.GeneratedSpec.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(generatedSpec))) ||
			binding.Runtime.GeneratedConfig.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(generatedConfig))) || len(binding.Baselines.Files) != 2 ||
			binding.PlanContracts.Path != visualhive.PullRequestPlanContractsPath || binding.EvaluationScopes.Path != visualhive.PullRequestEvaluationScopesPath ||
			binding.Runtime.StableSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(goCanonicalStableRuntime))) ||
			!strings.HasPrefix(binding.Baselines.Files[0].Path, ".visual-hive/proof/pr/baselines/") || binding.Baselines.Files[0].Path >= binding.Baselines.Files[1].Path {
			t.Fatalf("generated binding identity is incomplete: %+v", binding)
		}
		workflowSnapshot, snapshotErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(visualhive.PullRequestWorkflowPath)))
		if snapshotErr != nil || string(workflowSnapshot) != baseWorkflowDefinition || string(workflowSnapshot) == hostileHeadWorkflowDefinition ||
			binding.Workflow.Definition.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(baseWorkflowDefinition))) {
			t.Fatalf("workflow identity was not frozen from the trusted base commit: err=%v identity=%+v bytes=%q", snapshotErr, binding.Workflow.Definition, workflowSnapshot)
		}
		for source, snapshot := range map[string]string{
			"visual-hive.config.yaml": visualhive.PullRequestConfigPath,
			"testdata/baseline.png":   ".visual-hive/proof/pr/baselines/testdata/baseline.png",
			"testdata/Z-baseline.png": ".visual-hive/proof/pr/baselines/testdata/Z-baseline.png",
		} {
			original, originalErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(source)))
			copied, copiedErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(snapshot)))
			if originalErr != nil || copiedErr != nil || !bytes.Equal(original, copied) {
				t.Fatalf("sealed source snapshot %q does not exactly match %q: original=%v copied=%v", snapshot, source, originalErr, copiedErr)
			}
		}
	})

	t.Run("empty plan emits no source binding", func(t *testing.T) {
		root, state, output, runErr := run(t, fixtureOptions{noOp: true, omitRuntime: true})
		if runErr != nil || state != "false" {
			t.Fatalf("no-op plan did not stop before v3 source binding: %v state=%q\n%s", runErr, state, output)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(visualhive.PullRequestSourceBindingPath))); !os.IsNotExist(err) {
			t.Fatalf("no-op plan wrote an admissible source binding: %v", err)
		}
	})

	t.Run("producer report may omit PR number without using a merge ref", func(t *testing.T) {
		root, state, output, runErr := run(t, fixtureOptions{missingPRNumber: true})
		if runErr != nil || state != "true" {
			t.Fatalf("real-producer-compatible report was rejected: %v state=%q\n%s", runErr, state, output)
		}
		data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(visualhive.PullRequestSourceBindingPath)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		binding, parseErr := visualhive.ParsePullRequestSourceBinding(data)
		if parseErr != nil || binding.PullRequest != 7 || binding.Head.Ref != "feature/exact-head" {
			t.Fatalf("root-owned source binding lost exact PR/head identity: binding=%+v err=%v", binding, parseErr)
		}
	})

	t.Run("mixed ignored and relevant changes preserve the exact producer partition", func(t *testing.T) {
		_, state, output, runErr := run(t, fixtureOptions{mixedIgnored: true})
		if runErr != nil || state != "true" {
			t.Fatalf("real producer ignored-file partition was rejected: %v state=%q\n%s", runErr, state, output)
		}
	})

	for _, test := range []struct {
		name    string
		options fixtureOptions
		want    string
	}{
		{name: "missing runtime", options: fixtureOptions{omitRuntime: true}, want: "runtime"},
		{name: "runtime execution mismatch", options: fixtureOptions{mismatchRuntime: true}, want: "not bound"},
		{name: "generated spec index drift", options: fixtureOptions{generatedDrift: true}, want: "artifact-index entry"},
		{name: "base workflow snapshot drift", options: fixtureOptions{workflowDrift: true}, want: "identity drifted"},
		{name: "invalid baseline bytes", options: fixtureOptions{invalidBaseline: true}, want: "exact PNG"},
		{name: "noncanonical baseline path", options: fixtureOptions{baselinePath: " testdata/baseline.png"}, want: "exact passing baseline"},
		{name: "failed report status", options: fixtureOptions{failedReport: true}, want: "report is malformed"},
		{name: "mismatched present report PR number", options: fixtureOptions{reportPRNumber: 8}, want: "exact allowlisted"},
		{name: "generated spec outside canonical directory", options: fixtureOptions{outsideSpecPath: true}, want: "canonical generated directory"},
		{name: "noncanonical changed-file line", options: fixtureOptions{spacedChanged: true}, want: "non-canonical line"},
		{name: "omitted ignored changed file", options: fixtureOptions{omittedIgnored: true}, want: "do not exactly partition"},
		{name: "extra ignored changed file", options: fixtureOptions{extraIgnored: true}, want: "outside the runner-owned diff"},
		{name: "missing evaluated plan contract", options: fixtureOptions{planContracts: "other-contract\n"}, want: "evaluated plan contracts"},
		{name: "duplicated evaluated plan contract", options: fixtureOptions{planContracts: "contract-a\ncontract-a\n"}, want: "duplicates"},
		{name: "full scopes omit plan contract", options: fixtureOptions{evaluationScopes: "provider-governance\n"}, want: "Full evaluation scopes"},
		{name: "full scopes contain arbitrary extra", options: fixtureOptions{evaluationScopes: "attacker-scope\ncontract-a\n"}, want: "deterministic lifecycle scopes"},
		{name: "unaudited producer", options: fixtureOptions{producerCommit: strings.Repeat("f", 40)}, want: "exact audited"},
		{name: "target contract mismatch", options: fixtureOptions{resultTarget: "target-b"}, want: "exactly bound"},
		{name: "merge ref", options: fixtureOptions{headRef: "refs/pull/7/merge"}, want: "merge-derived"},
		{name: "report changed after root authentication", options: fixtureOptions{tamperAttested: true}, want: "authenticate the exact execution artifacts"},
		{name: "forged runner HMAC", options: fixtureOptions{tamperMAC: true}, want: "HMAC verification failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, state, output, runErr := run(t, test.options)
			if runErr == nil || state != "" || !strings.Contains(string(output), test.want) {
				t.Fatalf("unsafe PR source was not rejected: err=%v state=%q want=%q\n%s", runErr, state, test.want, output)
			}
		})
	}
}

func TestPullRequestPresentIssuesFilterPreservesMetadataAndRejectsAbsence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to execute the generated present-only issue filter")
	}
	run := func(t *testing.T, issues any) (string, map[string]any, error) {
		t.Helper()
		root := t.TempDir()
		report := map[string]any{
			"schemaVersion": "visual-hive.issues.v1",
			"generatedAt":   "2026-07-16T12:00:00.000Z",
			"project":       "fixture",
			"summary":       map[string]any{"openCandidates": 1, "updateCandidates": 1, "resolvedCandidates": 1},
			"issues":        issues,
		}
		data, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		writeFixture(t, root, ".visual-hive/issues.json", string(data)+"\n")
		command := exec.Command(node, "-e", pullRequestPresentIssuesScript())
		command.Dir = root
		output, runErr := command.CombinedOutput()
		var filtered map[string]any
		if filteredData, readErr := os.ReadFile(filepath.Join(root, ".visual-hive", "proof", "pr", "issues.present.json")); readErr == nil {
			if decodeErr := json.Unmarshal(filteredData, &filtered); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return string(output), filtered, runErr
	}

	t.Run("preserves metadata and retains only present candidates", func(t *testing.T) {
		output, filtered, runErr := run(t, []any{
			map[string]any{"id": "open", "status": "open_candidate", "issueKind": "screenshot_diff", "severity": "high"},
			map[string]any{"id": "update", "status": "update_candidate", "issueKind": "screenshot_diff", "severity": "medium"},
			map[string]any{"id": "resolved", "status": "resolved_candidate", "issueKind": "stale_baseline", "severity": "low"},
			map[string]any{"id": "suppressed", "status": "suppressed", "issueKind": "workflow_safety", "severity": "critical"},
			map[string]any{"id": "blocked", "status": "blocked", "issueKind": "provider_governance", "severity": "high"},
		})
		if runErr != nil {
			t.Fatalf("present-only issue filter failed: %v\n%s", runErr, output)
		}
		issues, ok := filtered["issues"].([]any)
		if !ok || len(issues) != 2 || filtered["schemaVersion"] != "visual-hive.issues.v1" || filtered["project"] != "fixture" || filtered["generatedAt"] != "2026-07-16T12:00:00.000Z" {
			t.Fatalf("present-only issue report lost metadata or retained absence: %#v", filtered)
		}
		for _, value := range issues {
			status := value.(map[string]any)["status"]
			if status != "open_candidate" && status != "update_candidate" {
				t.Fatalf("present-only issue report retained %q", status)
			}
		}
		summary, ok := filtered["summary"].(map[string]any)
		if !ok || summary["total"] != float64(2) || summary["openCandidates"] != float64(1) || summary["updateCandidates"] != float64(1) ||
			summary["resolvedCandidates"] != float64(0) || summary["suppressed"] != float64(0) || summary["blocked"] != float64(0) {
			t.Fatalf("present-only issue summary retained absence claims: %#v", summary)
		}
	})

	for _, test := range []struct {
		name   string
		issues any
		want   string
	}{
		{name: "unknown status", issues: []any{map[string]any{"status": "mystery_candidate", "issueKind": "screenshot_diff", "severity": "high"}}, want: "unknown or malformed status"},
		{name: "malformed issue array", issues: map[string]any{"status": "open_candidate"}, want: "malformed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, filtered, runErr := run(t, test.issues)
			if runErr == nil || filtered != nil || !strings.Contains(output, test.want) {
				t.Fatalf("unsafe issue report was not rejected: err=%v filtered=%#v want=%q\n%s", runErr, filtered, test.want, output)
			}
		})
	}
}

func TestTrustedCollectorUsesSealedPinnedAndTargetPlaywrightBrowsers(t *testing.T) {
	protectedEvidencePrune := `find "$HIVE_TARGET_WORKSPACE" -path "$HIVE_TARGET_WORKSPACE/.visual-hive" -prune -o -path '*/node_modules/@playwright/test/cli.js'`
	if !strings.Contains(trustedBrowserHandoffVerificationShell(), protectedEvidencePrune) {
		t.Fatal("post-protection browser verification does not prune the protected evidence root")
	}
	for name, value := range map[string]string{"production": workflow(isolationWorkflowConfig()), "pull-request": pullRequestWorkflow(isolationWorkflowConfig())} {
		execution := workflowJobText(parseIsolatedWorkflow(t, value).Jobs[visualExecutionJobName])
		for _, invariant := range []string{
			`trusted_browser_path="/opt/hive-target/trusted/playwright-${GITHUB_RUN_ID}"`,
			`target_browser_staging="/opt/hive-target/playwright-staging-${GITHUB_RUN_ID}"`,
			`sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium`,
			`PLAYWRIGHT_BROWSERS_PATH="$target_browser_staging"`,
			`sudo find "$target_browser_staging" -type l -print -quit`,
			`sudo find "$target_browser_staging" ! -type d ! -type f -print -quit`,
			`sudo du -sb "$target_browser_staging"`,
			`sudo cp -a --update=none "$target_browser_staging"/. "$trusted_browser_path"/`,
			`sudo chown -R root:root "$trusted_browser_path"`,
			`sudo find "$trusted_browser_path" -type d -exec chmod a+rx,a-w {} +`,
			`sudo find "$trusted_browser_path" -type f -exec chmod a+r,a-w {} +`,
			`Sealed Playwright browser root is not readable and traversable by the isolated target account`,
			`Sealed Playwright browser root remains writable by the isolated target account`,
			`trusted_tooling="/opt/hive-target/trusted/visual-hive-tooling"`,
			`HIVE_TRUSTED_BROWSER_MANIFEST=$trusted_browser_manifest`,
			`Target Playwright runtime lacks a sealed executable binding`,
			`runtime.chromium.launch({ headless: true })`,
			`Target Playwright runtime could not launch its exact sealed headless browser`,
			protectedEvidencePrune,
			`Trusted Playwright browser executable is missing before Visual Hive execution`,
			`PLAYWRIGHT_BROWSERS_PATH="$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"`,
			`test "$(sha256sum "$HIVE_TRUSTED_BROWSER_EXECUTABLE" | cut -d ' ' -f 1)" = "$HIVE_TRUSTED_BROWSER_SHA"`,
			`sudo -u hive-target -- test ! -w "$HIVE_TRUSTED_BROWSER_EXECUTABLE"`,
			`sudo rm -rf -- "$expected_browser_path"`,
			`test ! -e "$expected_browser_path"`,
		} {
			if !strings.Contains(execution, invariant) {
				t.Fatalf("%s workflow lost sealed browser invariant %q", name, invariant)
			}
		}
		if strings.Contains(execution, `PLAYWRIGHT_BROWSERS_PATH=/home/hive-target/.cache/ms-playwright HIVE_TARGET_PROCESS=1 "$HIVE_TRUSTED_NODE"`) {
			t.Fatalf("%s Visual collector still accepts the target-owned browser cache", name)
		}
		if strings.Contains(execution, `sudo cp -a --no-clobber`) {
			t.Fatalf("%s browser handoff still uses the failing legacy no-clobber mode", name)
		}
		if !strings.Contains(value, "permissions:\n      contents: read\n      actions: read") || strings.Contains(execution, "contents: write") || strings.Contains(execution, "id-token: write") {
			t.Fatalf("%s browser provisioning changed the read-only target-execution permissions", name)
		}
		if strings.Contains(execution, `mv .hive-visual-tooling "$RUNNER_TEMP/visual-hive-tooling"`) ||
			strings.Contains(execution, `trusted_browser_path="$RUNNER_TEMP/hive-playwright`) ||
			strings.Contains(execution, `sudo chmod o+x "$RUNNER_TEMP"`) {
			t.Fatalf("%s target-readable sealed tooling still traverses the runner-private temp root", name)
		}
		install := strings.Index(value, `sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium`)
		targetInstall := strings.Index(value, `PLAYWRIGHT_BROWSERS_PATH="$target_browser_staging"`)
		seal := strings.Index(value, `sudo find "$trusted_browser_path" -type d -exec chmod a+rx,a-w {} +`)
		preflight := strings.Index(value, "name: Verify sealed Playwright browser handoff")
		collection := strings.Index(value, "name: Run target-facing Visual Hive collection")
		cleanup := strings.LastIndex(value, `sudo rm -rf -- "$expected_browser_path"`)
		if install < 0 || targetInstall < install || seal < targetInstall || preflight < seal || collection < preflight || cleanup < collection {
			t.Fatalf("%s workflow browser install/seal/preflight/execute/cleanup order is unsafe: install=%d target=%d seal=%d preflight=%d collection=%d cleanup=%d", name, install, targetInstall, seal, preflight, collection, cleanup)
		}
	}
	pullRequestExecution := workflowJobText(parseIsolatedWorkflow(t, pullRequestWorkflow(isolationWorkflowConfig())).Jobs["visual-hive"])
	if !strings.Contains(pullRequestExecution, `const report = JSON.parse(fs.readFileSync(".visual-hive/report.json", "utf8"))`) ||
		!strings.Contains(pullRequestExecution, `["failed", "blocked"].includes(report.status) && verdictSummary.visualHiveVerdict === "blocked"`) {
		t.Fatal("pull-request verifier does not accept Visual Hive's blocked missing-baseline status")
	}
	if !strings.Contains(pullRequestExecution, `blocking.length === 0`) {
		t.Fatal("pull-request verifier does not require every gating contribution to pass")
	}
	for _, stale := range []string{"pipeline.summary", "pipeline.verdictSummary", "pipeline.verdictContributions", "pipeline.results"} {
		if strings.Contains(pullRequestExecution, stale) {
			t.Fatalf("pull-request verifier still reads %s from the pipeline envelope", stale)
		}
	}
	dependencyShell := isolatedTargetDependencyShell(nil, true)
	install := strings.Index(dependencyShell, `sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium`)
	targetDependencies := strings.Index(dependencyShell, "HIVE_TARGET_DEPENDENCIES")
	if install < 0 || targetDependencies < 0 || install > targetDependencies {
		t.Fatal("trusted browser is installed only after target dependency code")
	}
	if strings.Contains(dependencyShell, isolatedVisualTargetEnvPrefix()+` bash`) {
		t.Fatal("target dependency code was granted the final trusted browser directory")
	}
}

func TestRunnerOwnedEvaluationScopeUsesCompletedReportRowsAndFailsClosed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to execute the runner-owned evidence scope verifier")
	}
	tests := []struct {
		name              string
		pipeline          string
		plan              string
		report            string
		runnerOutcome     string
		wantEvaluated     string
		wantAuthoritative string
		wantError         bool
	}{
		{
			name:              "full green complete",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-b"},{"contractId":"contract-a"}],"excluded":[]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":["contract-a","contract-b"],"excludedContracts":[],"results":[{"contractId":"contract-b","status":"passed"},{"contractId":"contract-a","status":"passed"}]}`,
			wantEvaluated:     "contract-a\ncontract-b\n",
			wantAuthoritative: "true\n",
		},
		{
			name:              "crafted green artifacts cannot override runner failure",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-a"}],"excluded":[]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"passed"}]}`,
			runnerOutcome:     `{"schemaVersion":"hive.visual-runner-outcome.v1","outcome":"failure","conclusion":"success"}`,
			wantEvaluated:     "",
			wantAuthoritative: "false\n",
		},
		{
			name:              "planned contract skipped",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-a"},{"contractId":"contract-b"}],"excluded":[]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":["contract-a","contract-b"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"passed"},{"contractId":"contract-b","status":"skipped"}]}`,
			wantEvaluated:     "contract-a\n",
			wantAuthoritative: "false\n",
		},
		{
			name:              "configured contract excluded",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-a"}],"excluded":[{"contractId":"contract-b"}]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[{"contractId":"contract-b"}],"results":[{"contractId":"contract-a","status":"passed"}]}`,
			wantEvaluated:     "contract-a\n",
			wantAuthoritative: "true\n",
		},
		{
			name:              "pipeline not green",
			pipeline:          `{"mode":"full","status":"failed","exitCode":1}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-a"}],"excluded":[]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"passed"}]}`,
			wantEvaluated:     "contract-a\n",
			wantAuthoritative: "false\n",
		},
		{
			name:              "report not green",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-a"}],"excluded":[]}`,
			report:            `{"mode":"full","status":"failed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"failed"}]}`,
			wantEvaluated:     "",
			wantAuthoritative: "false\n",
		},
		{
			name:              "created result is not an absence receipt",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[{"contractId":"contract-a"}],"excluded":[]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"created"}]}`,
			wantEvaluated:     "",
			wantAuthoritative: "false\n",
		},
		{
			name:              "non-full mode",
			pipeline:          `{"mode":"pr","status":"passed","exitCode":0}`,
			plan:              `{"mode":"pr","items":[{"contractId":"contract-a"}],"excluded":[]}`,
			report:            `{"mode":"pr","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"passed"}]}`,
			wantEvaluated:     "contract-a\n",
			wantAuthoritative: "false\n",
		},
		{
			name:      "extra unselected result is rejected",
			pipeline:  `{"mode":"full","status":"passed","exitCode":0}`,
			plan:      `{"mode":"full","items":[{"contractId":"contract-a"}],"excluded":[]}`,
			report:    `{"mode":"full","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"passed"},{"contractId":"unselected","status":"skipped"}]}`,
			wantError: true,
		},
		{
			name:              "vacuous green plan is non-authoritative",
			pipeline:          `{"mode":"full","status":"passed","exitCode":0}`,
			plan:              `{"mode":"full","items":[],"excluded":[]}`,
			report:            `{"mode":"full","status":"passed","selectedContracts":[],"excludedContracts":[],"results":[]}`,
			wantEvaluated:     "",
			wantAuthoritative: "false\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runnerOutcome := test.runnerOutcome
			if runnerOutcome == "" {
				runnerOutcome = `{"schemaVersion":"hive.visual-runner-outcome.v1","outcome":"success","conclusion":"success"}`
			}
			writeFixture(t, root, ".visual-hive/hive-runner-outcome.json", runnerOutcome)
			writeFixture(t, root, ".visual-hive/pipeline.json", test.pipeline)
			writeFixture(t, root, ".visual-hive/plan.json", test.plan)
			writeFixture(t, root, ".visual-hive/report.json", test.report)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, node, "-e", runnerOwnedEvaluationScopeScript())
			command.Dir = root
			if output, runErr := command.CombinedOutput(); test.wantError {
				if runErr == nil || !strings.Contains(string(output), "exact one-to-one") {
					t.Fatalf("runner-owned scope verifier error = %v, output=%s", runErr, output)
				}
				return
			} else if runErr != nil {
				t.Fatalf("runner-owned scope verifier failed: %v\n%s", runErr, output)
			}
			evaluated, readErr := os.ReadFile(filepath.Join(root, ".visual-hive", "evaluated-contracts.txt"))
			if readErr != nil || string(evaluated) != test.wantEvaluated {
				t.Fatalf("evaluated contracts = %q, err=%v; want %q", evaluated, readErr, test.wantEvaluated)
			}
			planContracts, readErr := os.ReadFile(filepath.Join(root, ".visual-hive", "evaluated-plan-contracts.txt"))
			if readErr != nil || string(planContracts) != test.wantEvaluated {
				t.Fatalf("evaluated plan contracts = %q, err=%v; want %q", planContracts, readErr, test.wantEvaluated)
			}
			authority, readErr := os.ReadFile(filepath.Join(root, ".visual-hive", "authoritative-resolution.txt"))
			if readErr != nil || string(authority) != test.wantAuthoritative {
				t.Fatalf("resolution authority = %q, err=%v; want %q", authority, readErr, test.wantAuthoritative)
			}
		})
	}
}

func TestRunnerOwnedPlanRegenerationReplacesHostileRawPlanAndBindsReport(t *testing.T) {
	bash := workflowBash(t)
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required to execute the runner-owned plan regeneration verifier")
	}
	run := func(t *testing.T, report string) ([]byte, error, string, bool) {
		t.Helper()
		root := t.TempDir()
		writeFixture(t, root, "visual-hive.config.yaml", "schemaVersion: 1\n")
		writeFixture(t, root, ".visual-hive/hive-runner-outcome.json", `{"schemaVersion":"hive.visual-runner-outcome.v1","outcome":"success","conclusion":"success"}`)
		writeFixture(t, root, ".visual-hive/pipeline.json", `{"mode":"full","status":"passed","exitCode":0}`)
		writeFixture(t, root, ".visual-hive/report.json", report)
		writeFixture(t, root, ".visual-hive/plan.json", `{"mode":"full","items":[{"contractId":"hostile-contract"}],"excluded":[]}`)
		writeFixture(t, root, ".visual-hive/plan.full.json", `{"mode":"full","items":[{"contractId":"hostile-contract"}],"excluded":[]}`)
		writeFixture(t, root, "fake-visual-hive.js", `const fs = require("fs");
const args = process.argv.slice(2);
if (args[0] !== "plan" || args[args.indexOf("--mode") + 1] !== "full") process.exit(2);
const output = args[args.indexOf("--output") + 1];
fs.writeFileSync(output, JSON.stringify({mode:"full",items:[{contractId:"contract-a"}],excluded:[]}) + "\n");`)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, bash, "-e", "-o", "pipefail", "-c", runnerOwnedPlanRegenerationAndEvaluationShell(false))
		command.Dir = root
		command.Env = append(os.Environ(), "VISUAL_HIVE_CLI=fake-visual-hive.js")
		output, runErr := command.CombinedOutput()
		evaluated, _ := os.ReadFile(filepath.Join(root, ".visual-hive", "evaluated-contracts.txt"))
		_, staleErr := os.Stat(filepath.Join(root, ".visual-hive", "plan.full.json"))
		return output, runErr, string(evaluated), os.IsNotExist(staleErr)
	}
	t.Run("fresh immutable plan replaces hostile raw plans", func(t *testing.T) {
		output, runErr, evaluated, staleRemoved := run(t, `{"mode":"full","status":"passed","selectedContracts":["contract-a"],"excludedContracts":[],"results":[{"contractId":"contract-a","status":"passed"}]}`)
		if runErr != nil || evaluated != "contract-a\n" || !staleRemoved {
			t.Fatalf("fresh plan replacement failed: err=%v evaluated=%q stale_removed=%t\n%s", runErr, evaluated, staleRemoved, output)
		}
	})
	t.Run("hostile report cannot retain deleted hostile plan scope", func(t *testing.T) {
		output, runErr, evaluated, staleRemoved := run(t, `{"mode":"full","status":"passed","selectedContracts":["hostile-contract"],"excludedContracts":[],"results":[{"contractId":"hostile-contract","status":"passed"}]}`)
		if runErr == nil || !strings.Contains(string(output), "exact one-to-one") || evaluated != "" || !staleRemoved {
			t.Fatalf("hostile report was not rejected: err=%v evaluated=%q stale_removed=%t\n%s", runErr, evaluated, staleRemoved, output)
		}
	})
}

func TestRunnerOwnedRebuildRefreshesArtifactIndexLast(t *testing.T) {
	shell := runnerOwnedEvidenceRebuildShell(false)
	artifactCommand := `"${safe_env[@]}" node "$VISUAL_HIVE_CLI" artifacts --config visual-hive.config.yaml --complete`
	generated := workflow(isolationWorkflowConfig())
	if !strings.Contains(generated, artifactCommand) {
		t.Fatal("generated production workflow does not request a complete artifact index")
	}
	artifactIndex := strings.LastIndex(shell, artifactCommand)
	if artifactIndex < 0 {
		t.Fatal("runner-owned rebuild does not refresh a complete artifact index")
	}
	capabilityCommand := `"${safe_env[@]}" node "$VISUAL_HIVE_CLI" capabilities --config visual-hive.config.yaml`
	if !strings.Contains(generated, capabilityCommand) {
		t.Fatal("generated production workflow does not regenerate capability parity with the pinned CLI")
	}
	capabilityIndex := strings.LastIndex(shell, capabilityCommand)
	safeEnvironmentIndex := strings.Index(shell, `safe_env=(env `)
	if safeEnvironmentIndex < 0 || capabilityIndex < safeEnvironmentIndex || capabilityIndex > artifactIndex {
		t.Fatalf("runner-owned capability parity is not regenerated with the pinned safe CLI before the complete index: safe_env=%d capabilities=%d artifacts=%d", safeEnvironmentIndex, capabilityIndex, artifactIndex)
	}
	if !strings.Contains(shell, `rm -f `) || !strings.Contains(shell, `.visual-hive/capability-parity.json`) {
		t.Fatal("runner-owned rebuild does not discard the target-produced capability parity receipt")
	}
	for _, command := range []string{" workflows --config", " providers list --config", " evidence --config", " layers --config", " verdict --config", " issues --config", " baselines list --config", " hive integration-smoke --config"} {
		index := strings.Index(shell, command)
		if index < 0 || index > artifactIndex {
			t.Fatalf("artifact index is not refreshed after %q", command)
		}
	}
	for _, required := range []string{`.visual-hive/workflows.json`, `.visual-hive/provider-results.json`, `.visual-hive/testing-layers.json`, `"testing-layer:" + layer.id`, `workflows.findings.length === 0`, `issueProducingProviderStatuses`, `scopes.push("workflow-safety")`, `scopes.push("provider-governance")`} {
		if !strings.Contains(shell, required) {
			t.Fatalf("runner-owned rebuild omitted current receipt invariant %q", required)
		}
	}
}

func TestGeneratedIsolationShellsAreExecutable(t *testing.T) {
	bash := workflowBash(t)
	for name, value := range map[string]string{"production": workflow(isolationWorkflowConfig()), "pull-request": pullRequestWorkflow(isolationWorkflowConfig())} {
		document := parseIsolatedWorkflow(t, value)
		for jobName, job := range document.Jobs {
			for _, step := range job.Steps {
				if strings.TrimSpace(step.Run) == "" {
					continue
				}
				command := exec.Command(bash, "-n")
				command.Stdin = strings.NewReader(step.Run)
				if output, err := command.CombinedOutput(); err != nil {
					lines := strings.Split(step.Run, "\n")
					for index := range lines {
						lines[index] = fmt.Sprintf("%4d: %s", index+1, lines[index])
					}
					t.Fatalf("%s job %s step %q has invalid shell: %v\n%s\n%s", name, jobName, step.Name, err, output, strings.Join(lines, "\n"))
				}
			}
		}
	}
}

func TestSealIsolatedVisualEvidenceCrossPrincipal(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-principal evidence seal requires the hosted Linux privilege boundary")
	}
	bash, bashErr := exec.LookPath("bash")
	sudo, sudoErr := exec.LookPath("sudo")
	_, pythonErr := exec.LookPath("python")
	if bashErr != nil || sudoErr != nil || pythonErr != nil {
		t.Skip("bash, sudo, and python are required for the hosted evidence seal proof")
	}
	if output, err := exec.Command(sudo, "-n", "true").CombinedOutput(); err != nil {
		t.Skipf("passwordless sudo is unavailable: %v: %s", err, output)
	}
	if uid, err := exec.Command("id", "-u").Output(); err != nil || strings.TrimSpace(string(uid)) == "0" {
		t.Skip("the proof requires a non-root runner principal")
	}
	if output, err := exec.Command("id", "-u", "nobody").CombinedOutput(); err != nil {
		t.Skipf("the distinct nobody account is unavailable: %v: %s", err, output)
	}
	groupOutput, err := exec.Command("id", "-gn", "nobody").Output()
	if err != nil {
		t.Fatal(err)
	}
	targetGroup := strings.TrimSpace(string(groupOutput))

	run := func(command *exec.Cmd) (string, error) {
		output, runErr := command.CombinedOutput()
		return string(output), runErr
	}
	for _, test := range []struct {
		name        string
		prepare     string
		wantFailure string
	}{
		{name: "valid secure evidence"},
		{name: "symbolic link", prepare: `ln -s payload.json "$HIVE_EVIDENCE_ROOT/link.json"`, wantFailure: "symbolic link"},
		{name: "hard link", prepare: `ln "$HIVE_EVIDENCE_ROOT/payload.json" "$HIVE_EVIDENCE_ROOT/link.json"`, wantFailure: "hard-linked file"},
		{name: "fifo", prepare: `mkfifo "$HIVE_EVIDENCE_ROOT/pipe"`, wantFailure: "non-regular file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := crossPrincipalTempDir(t)
			evidence := filepath.Join(workspace, ".visual-hive")
			if err := os.Mkdir(evidence, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = run(exec.Command(sudo, "-n", "chown", "-R", fmt.Sprintf("%s:%s", strings.TrimSpace(mustCommandOutput(t, "id", "-u")), strings.TrimSpace(mustCommandOutput(t, "id", "-g"))), workspace))
				_ = os.Chmod(workspace, 0o700)
			})
			if output, err := run(exec.Command(sudo, "-n", "chown", "-R", "nobody:"+targetGroup, evidence)); err != nil {
				t.Fatalf("prepare target ownership: %v: %s", err, output)
			}
			prepare := `umask 077; printf '{"schemaVersion":"fixture"}\n' > "$HIVE_EVIDENCE_ROOT/payload.json"`
			if test.prepare != "" {
				prepare += "; " + test.prepare
			}
			prepareCommand := exec.Command(sudo, "-n", "-u", "nobody", "env", "HIVE_EVIDENCE_ROOT="+evidence, bash, "-euo", "pipefail", "-c", prepare)
			if output, err := run(prepareCommand); err != nil {
				t.Fatalf("prepare target evidence: %v: %s", err, output)
			}
			seal := exec.Command(bash, "-euo", "pipefail", "-c", sealIsolatedVisualEvidenceShell())
			seal.Dir = workspace
			seal.Env = append(os.Environ(), "GITHUB_WORKSPACE="+workspace)
			output, sealErr := run(seal)
			if test.wantFailure != "" {
				if sealErr == nil || !strings.Contains(output, test.wantFailure) {
					t.Fatalf("unsafe evidence seal error = %v, output=%q; want %q", sealErr, output, test.wantFailure)
				}
				return
			}
			if sealErr != nil {
				t.Fatalf("seal valid cross-principal evidence: %v: %s", sealErr, output)
			}
			payload := filepath.Join(evidence, "payload.json")
			if _, err := os.ReadFile(payload); err != nil {
				t.Fatalf("runner cannot read sealed evidence: %v", err)
			}
			if handle, err := os.OpenFile(payload, os.O_WRONLY, 0); err == nil {
				_ = handle.Close()
				t.Fatal("runner can write sealed target evidence")
			}
		})
	}
}

func TestProtectedEvidenceDeniesContinuousTargetOverwriteAndPostAuthenticationForgery(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-principal hostile overwrite proof requires the hosted Linux privilege boundary")
	}
	bash, bashErr := exec.LookPath("bash")
	sudo, sudoErr := exec.LookPath("sudo")
	if bashErr != nil || sudoErr != nil {
		t.Skip("bash and sudo are required for the hostile overwrite proof")
	}
	if output, err := exec.Command(sudo, "-n", "true").CombinedOutput(); err != nil {
		t.Skipf("passwordless sudo is unavailable: %v: %s", err, output)
	}
	if uid, err := exec.Command("id", "-u").Output(); err != nil || strings.TrimSpace(string(uid)) == "0" {
		t.Skip("the proof requires a non-root evidence principal")
	}
	if output, err := exec.Command("id", "-u", "nobody").CombinedOutput(); err != nil {
		t.Skipf("the distinct nobody target account is unavailable: %v: %s", err, output)
	}

	workspace := crossPrincipalTempDir(t)
	evidence := filepath.Join(workspace, ".visual-hive")
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(evidence, "report.json")
	legitimate := []byte("{\"schemaVersion\":2,\"status\":\"passed\"}\n")
	if err := os.WriteFile(reportPath, legitimate, 0o600); err != nil {
		t.Fatal(err)
	}
	authorityRoot := filepath.Join(workspace, "runner-authority")
	if err := os.Mkdir(authorityRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	authorityKey := []byte("0123456789abcdef0123456789abcdef")
	keyPath := filepath.Join(authorityRoot, "execution.key")
	if err := os.WriteFile(keyPath, authorityKey, 0o600); err != nil {
		t.Fatal(err)
	}
	message := sha256.Sum256(legitimate)
	mac := hmac.New(sha256.New, authorityKey)
	_, _ = mac.Write(message[:])
	authenticatedMAC := mac.Sum(nil)

	if command := exec.Command(sudo, "-n", "-u", "nobody", "test", "-r", keyPath); command.Run() == nil {
		t.Fatal("target principal can read the evidence authentication key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hostile := exec.CommandContext(ctx, sudo, "-n", "-u", "nobody", "env", "HIVE_EVIDENCE_REPORT="+reportPath, bash, "-c", `deadline=$((SECONDS + 2)); while [ "$SECONDS" -lt "$deadline" ]; do { printf '%s\n' '{"status":"passed","forged":true}' > "$HIVE_EVIDENCE_REPORT"; } 2>/dev/null || :; done`)
	if output, err := hostile.CombinedOutput(); err != nil {
		t.Fatalf("hostile target loop did not complete cleanly: %v: %s", err, output)
	}
	observed, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(observed, legitimate) {
		t.Fatalf("hostile target overwrote protected evidence: %q", observed)
	}
	observedDigest := sha256.Sum256(observed)
	verified := hmac.New(sha256.New, authorityKey)
	_, _ = verified.Write(observedDigest[:])
	if !hmac.Equal(authenticatedMAC, verified.Sum(nil)) {
		t.Fatal("unchanged protected evidence failed runner authentication")
	}

	forged := append(bytes.Clone(observed), byte(' '))
	forgedDigest := sha256.Sum256(forged)
	forgedMAC := hmac.New(sha256.New, authorityKey)
	_, _ = forgedMAC.Write(forgedDigest[:])
	if hmac.Equal(authenticatedMAC, forgedMAC.Sum(nil)) {
		t.Fatal("post-authentication evidence forgery retained Ready authority")
	}
}

func mustCommandOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func crossPrincipalTempDir(t *testing.T) string {
	t.Helper()
	workspace, err := os.MkdirTemp("", "hive-cross-principal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	if err := os.Chmod(workspace, 0o711); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestRawTargetEvidenceIsBoundedBeforeArtifactUpload(t *testing.T) {
	for name, value := range map[string]string{"production": workflow(isolationWorkflowConfig()), "pull-request": pullRequestWorkflow(isolationWorkflowConfig())} {
		if !strings.Contains(value, "raw evidence contains a symbolic link") {
			t.Fatalf("%s workflow does not reject target symlinks before upload", name)
		}
		for _, invariant := range []string{
			`if files > 5000 or total > 1073741824:`,
			`item.st_nlink != 1`,
			`item.st_dev != device`,
			`sudo find "$evidence_root" -xdev -type f -exec chmod 0444 -- {} +`,
		} {
			if !strings.Contains(value, invariant) {
				t.Fatalf("%s workflow does not bound raw evidence before upload: %q", name, invariant)
			}
		}
		for _, invariant := range []string{`artifact_files="$(find .visual-hive -type f | wc -l)"`, `test "$artifact_files" -le 5000`, `test "$artifact_bytes" -le 1073741824`} {
			if !strings.Contains(value, invariant) {
				t.Fatalf("%s workflow does not recheck downloaded raw evidence: %q", name, invariant)
			}
		}
	}
}

func TestProductionPrerequisiteRequiresExactTopologyAndSuccessfulVisualExecution(t *testing.T) {
	bash := workflowBash(t)
	run := func(needs string) error {
		command := exec.Command(bash, "-e", "-o", "pipefail", "-c", productionPrerequisiteShell())
		command.Env = append(os.Environ(), `HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`, "HIVE_NEEDS_JSON="+needs)
		return command.Run()
	}
	if err := run(`{"repository-test-001":{"result":"failure"},"visual-hive-execution":{"result":"success"}}`); err != nil {
		t.Fatalf("terminal repository failure with successful isolated Visual execution was rejected: %v", err)
	}
	for _, invalid := range []string{
		`{"repository-test-001":{"result":"success"},"visual-hive-execution":{"result":"failure"}}`,
		`{"repository-test-001":{"result":"success"}}`,
		`{"repository-test-001":{"result":"success"},"repository-test-002":{"result":"success"},"visual-hive-execution":{"result":"success"}}`,
		`{"repository-test-001":{"result":"skipped"},"visual-hive-execution":{"result":"success"}}`,
		`{"repository-test-001":{"result":"cancelled"},"visual-hive-execution":{"result":"success"}}`,
	} {
		if err := run(invalid); err == nil {
			t.Fatalf("invalid production prerequisite topology/outcome was accepted: %s", invalid)
		}
	}
}

func TestPullRequestEnforcementUsesOnlyRunnerOwnedJobTopology(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".visual-hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, ".visual-hive"), "pipeline.json", `{"status":"passed","exitCode":0}`)
	writeFixture(t, filepath.Join(root, ".visual-hive"), "report.json", `{"status":"passed","summary":{},"results":[]}`)
	writeFixture(t, filepath.Join(root, ".visual-hive"), "verdict.json", `{"summary":{"visualHiveVerdict":"passed"},"gatingContributions":[]}`)
	baseEnv := append(os.Environ(),
		`HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"}}`,
		"HIVE_TRUSTED_REBUILD_OUTCOME=success",
		"HIVE_SETUP_OPERATION=setup", "HIVE_SETUP_AUTHORIZED=false",
	)
	run := func(environment []string) error {
		command := exec.Command(bash, "-e", "-o", "pipefail", "-c", pullRequestEnforcementShell())
		command.Dir = root
		command.Env = environment
		return command.Run()
	}
	if err := run(baseEnv); err != nil {
		t.Fatalf("valid runner topology was rejected: %v", err)
	}
	writeFixture(t, filepath.Join(root, ".visual-hive"), "verdict.json", `{"summary":{"visualHiveVerdict":"passed"},"gatingContributions":[{"kind":"future_required_capability","status":"blocked","gating":true}]}`)
	if err := run(baseEnv); err == nil {
		t.Fatal("nominally passed verdict accepted an unknown blocked gating contribution")
	}
	writeFixture(t, filepath.Join(root, ".visual-hive"), "verdict.json", `{"summary":{"visualHiveVerdict":"passed"}}`)
	if err := run(baseEnv); err == nil {
		t.Fatal("nominally passed verdict accepted a missing gating contribution inventory")
	}
	writeFixture(t, filepath.Join(root, ".visual-hive"), "verdict.json", `{"summary":{"visualHiveVerdict":"passed"},"gatingContributions":[]}`)
	for _, invalid := range []string{
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"failure"},"repository-test-001":{"result":"success"}}`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"},"repository-test-002":{"result":"success"}}`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"}}`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"cancelled"}}`,
	} {
		environment := replaceEnvironment(baseEnv, "HIVE_NEEDS_JSON", invalid)
		if err := run(environment); err == nil {
			t.Fatalf("invalid runner-owned topology was accepted: %s", invalid)
		}
	}
	setupEnvironment := replaceEnvironment(baseEnv, "HIVE_NEEDS_JSON", `HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"failure"}}`)
	setupEnvironment = replaceEnvironment(setupEnvironment, "HIVE_SETUP_AUTHORIZED", "HIVE_SETUP_AUTHORIZED=true")
	setupEnvironment = append(setupEnvironment, "HIVE_REPOSITORY=owner/repo", "HIVE_HEAD_SHA="+strings.Repeat("a", 40), "HIVE_SETUP_CONTEXT=context", "HIVE_SETUP_BINDING_DIGEST=binding", "HIVE_SETUP_DIFF_DIGEST=diff")
	if err := run(setupEnvironment); err != nil {
		t.Fatalf("exact setup exception rejected runner-owned repository failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".visual-hive", "setup-bootstrap-repository-failure.json")); err != nil {
		t.Fatalf("setup exception proof missing: %v", err)
	}
}

func TestPullRequestEnforcementTreatsDerivedReadinessAsMissingBaselineOnly(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	visualDir := filepath.Join(root, ".visual-hive")
	if err := os.MkdirAll(visualDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, visualDir, "pipeline.json", `{
  "schemaVersion":"visual-hive.pipeline.v1","status":"failed","exitCode":1,
  "steps":[],"artifacts":{}
}`)
	writeFixture(t, visualDir, "report.json", `{
  "schemaVersion":"visual-hive.report.v1","status":"failed",
  "summary":{"missingBaselines":1,"visualDiffs":0,"consoleErrors":0,"pageErrors":0,"flowStepsFailed":0},
  "results":[{"screenshotAssertions":[{"status":"missing_baseline","contractId":"app-shell","screenshotName":"desktop","baselinePath":"snapshots/app.png","actualPath":"artifacts/app.png"}]}]
}`)
	writeFixture(t, visualDir, "verdict.json", `{
  "schemaVersion":"visual-hive.verdict.v1",
  "summary":{"visualHiveVerdict":"blocked","failedBecause":[]},
  "gatingContributions":[
    {"kind":"deterministic_run","status":"blocked","gating":true},
    {"kind":"contract_result","status":"blocked","gating":true},
    {"kind":"missing_baseline","status":"blocked","gating":true},
    {"kind":"readiness_gate","status":"blocked","gating":true}
  ]
}`)
	writeFixture(t, visualDir, "readiness.json", `{"status":"blocked","gates":[{"id":"deterministic:status","status":"blocked"},{"id":"baselines:missing-baseline","status":"blocked"},{"id":"mutation:missing","status":"warning"}]}`)
	environment := append(os.Environ(),
		`HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"}}`,
		"HIVE_TRUSTED_REBUILD_OUTCOME=failure", "HIVE_SETUP_OPERATION=setup", "HIVE_SETUP_AUTHORIZED=true",
		"HIVE_REPOSITORY=owner/repo", "HIVE_HEAD_SHA="+strings.Repeat("a", 40),
		"HIVE_SETUP_CONTEXT=context", "HIVE_SETUP_BINDING_DIGEST=binding", "HIVE_SETUP_DIFF_DIGEST=diff",
	)
	run := func() error {
		command := exec.Command(bash, "-e", "-o", "pipefail", "-c", pullRequestEnforcementShell())
		command.Dir = root
		command.Env = environment
		return command.Run()
	}
	if err := run(); err != nil {
		t.Fatalf("derived missing-baseline readiness did not enter the supported setup handoff: %v", err)
	}
	if _, err := os.Stat(filepath.Join(visualDir, "setup-baseline-required.json")); err != nil {
		t.Fatalf("setup baseline handoff proof missing: %v", err)
	}
	writeFixture(t, visualDir, "readiness.json", `{"status":"blocked","gates":[{"id":"deterministic:status","status":"blocked"},{"id":"baselines:missing-baseline","status":"blocked"},{"id":"security:posture","status":"blocked"}]}`)
	if err := run(); err == nil {
		t.Fatal("independent readiness blocker was misclassified as missing-baseline-only")
	}
}

func parseIsolatedWorkflow(t *testing.T, value string) isolatedWorkflowDocument {
	t.Helper()
	var document isolatedWorkflowDocument
	if err := yaml.Unmarshal([]byte(value), &document); err != nil {
		t.Fatalf("generated workflow is invalid YAML: %v\n%s", err, value)
	}
	return document
}

func workflowJobText(job isolatedWorkflowJob) string {
	var result strings.Builder
	result.WriteString(job.If)
	for _, step := range job.Steps {
		result.WriteString("\n" + step.Name + "\n" + step.If + "\n" + step.Uses + "\n" + step.Run)
		for name, value := range step.With {
			result.WriteString("\n" + name + "=" + strings.TrimSpace(fmt.Sprint(value)))
		}
	}
	return result.String()
}

func replaceEnvironment(environment []string, name, replacement string) []string {
	result := append([]string(nil), environment...)
	prefix := name + "="
	for index, value := range result {
		if strings.HasPrefix(value, prefix) {
			result[index] = replacement
			return result
		}
	}
	return append(result, replacement)
}
