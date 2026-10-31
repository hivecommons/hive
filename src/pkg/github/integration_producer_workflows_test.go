package github

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type integrationWorkflowContract struct {
	On struct {
		Call struct {
			Inputs map[string]struct {
				Type    string `yaml:"type"`
				Default bool   `yaml:"default"`
			} `yaml:"inputs"`
		} `yaml:"workflow_call"`
		Group struct {
			Types []string `yaml:"types"`
		} `yaml:"merge_group"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group  string `yaml:"group"`
		Cancel string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If          string            `yaml:"if"`
		RunsOn      string            `yaml:"runs-on"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
			With map[string]string `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func integrationWorkflowSourcePath(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("repository source path unavailable")
	}
	return current
}

func TestIntegrationProducerGeneratedReadOnlyEntrypoints(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("repository source path unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "../../..", ".github/workflows"))
	for _, file := range []string{
		"integration-technical-v2-ci.yml",
		"integration-technical-v2-tests.yml",
		"integration-technical-go-security-analysis.yml",
	} {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatal(err)
			}
			var workflow integrationWorkflowContract
			var document map[string]yaml.Node
			if err := yaml.Unmarshal(body, &workflow); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(body, &document); err != nil {
				t.Fatal(err)
			}
			on := document["on"]
			if on.Kind != yaml.MappingNode || len(on.Content) != 2 || on.Content[0].Value != "workflow_call" {
				t.Fatal("candidate-only callee acquired automatic PR/push/group/schedule triggers")
			}
			if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" ||
				len(workflow.Jobs) == 0 || workflow.Concurrency.Cancel != "false" {
				t.Fatal("candidate-only callee lacks a noncanceling readonly graph")
			}
			if strings.Contains(string(body), "inputs.integration_candidate") || strings.Contains(string(body), "vars.") {
				t.Fatal("candidate-only profile is mutable or can be switched to legacy behavior")
			}
			for name, job := range workflow.Jobs {
				if job.RunsOn != "ubuntu-latest" {
					t.Errorf("job %s can acquire an operator/self-hosted runner", name)
				}
				for _, permission := range job.Permissions {
					if permission != "read" && permission != "none" {
						t.Errorf("job %s still declares writer authority (even if skipped)", name)
					}
				}
				for _, step := range job.Steps {
					if strings.HasPrefix(step.Uses, "actions/checkout@") &&
						(step.With["persist-credentials"] != "false" || step.With["ref"] != "") {
						t.Errorf("job %s does not isolate checkout credentials/exact event commit", name)
					}
				}
			}
		})
	}
}
func TestIntegrationProducerWorkflowIsolationAndLegacySeams(t *testing.T) {
	current := integrationWorkflowSourcePath(t)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "../../.."))
	for _, file := range []struct {
		name        string
		concurrency string
	}{
		{"v2-ci.yml", "integration-ci"},
		{"v2-tests.yml", "integration-tests"},
		{"go-security-analysis.yml", "integration-security"},
	} {
		t.Run(file.name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, ".github/workflows", file.name))
			if err != nil {
				t.Fatal(err)
			}
			var workflow integrationWorkflowContract
			if err := yaml.Unmarshal(body, &workflow); err != nil {
				t.Fatal(err)
			}
			flag, exists := workflow.On.Call.Inputs["integration_candidate"]
			if !exists || flag.Type != "boolean" || flag.Default {
				t.Fatal("candidate execution is not a typed default-false call")
			}
			if len(workflow.On.Group.Types) != 1 || workflow.On.Group.Types[0] != "checks_requested" {
				t.Fatal("landed native merge_group trigger changed")
			}
			if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
				t.Fatal("technical workflow inherited additional authority")
			}
			if !strings.Contains(workflow.Concurrency.Group, "'"+file.concurrency+"' || github.workflow") ||
				!strings.Contains(workflow.Concurrency.Group, "github.run_id") ||
				!strings.Contains(workflow.Concurrency.Cancel, "!inputs.integration_candidate && github.event_name == 'pull_request'") {
				t.Fatal("candidate callees collide or change legacy/native concurrency")
			}
			for name, job := range workflow.Jobs {
				writes := false
				for _, permission := range job.Permissions {
					writes = writes || permission == "write"
				}
				excluded := strings.Contains(job.If, "!inputs.integration_candidate")
				if writes && !excluded {
					t.Errorf("job %s can acquire write authority for a candidate", name)
				}
				if excluded {
					continue
				}
				if job.RunsOn != "ubuntu-latest" && !strings.Contains(job.RunsOn, "!inputs.integration_candidate &&") {
					t.Errorf("job %s may execute candidate code on an operator/self-hosted runner", name)
				}
				for _, step := range job.Steps {
					if strings.HasPrefix(step.Uses, "actions/checkout@") {
						if step.With["persist-credentials"] != "${{ !inputs.integration_candidate }}" {
							t.Errorf("job %s leaves checkout credentials in candidate code", name)
						}
						if step.With["ref"] != "" {
							t.Errorf("job %s does not test the actual event commit", name)
						}
					}
					if softFail := step.Env["HIVE_CI_MISSING_TOOLCHAIN_SOFT_FAIL"]; softFail != "" &&
						!strings.Contains(softFail, "!inputs.integration_candidate &&") {
						t.Errorf("job %s permits manual-run toolchain soft failure", name)
					}
				}
			}
			scope := workflow.Jobs["required-context-scope"]
			var scopeScript strings.Builder
			for _, step := range scope.Steps {
				scopeScript.WriteString(step.Run)
			}
			if !strings.Contains(scopeScript.String(), `"$GITHUB_EVENT_NAME" != workflow_dispatch`) ||
				!strings.Contains(scopeScript.String(), `"$GITHUB_REF" != refs/tags/hive-integration/*`) {
				t.Fatal("candidate calls are not restricted to the installed dispatch/ref protocol")
			}
			if file.name == "v2-tests.yml" &&
				!strings.Contains(workflow.Jobs["coverage"].If, "inputs.integration_candidate || github.event_name == 'merge_group'") {
				t.Fatal("candidate or native coverage was dropped")
			}
		})
	}
}
