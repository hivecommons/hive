package oktotest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const repoRootFromPackage = "../../.."

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRootFromPackage, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestWorkflowIsSelfContained(t *testing.T) {
	out := Workflow(Options{AppLogins: "acme-hive[bot]"})
	for _, forbidden := range []string{"uses: hivecommons/hive/", "workflow_call", "uses:"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("self-contained workflow must not contain %q:\n%s", forbidden, out)
		}
	}
	for _, want := range []string{
		"issue_comment:",
		"types: [created]",
		"actions: write",
		"contents: read",
		"pull-requests: read",
		"issues: write",
		`HIVE_APP_LOGINS: "acme-hive[bot]"`,
		`TRUSTED_AUTHORS: "` + DefaultTrustedAuthors + `"`,
		`COMMAND: "` + DefaultCommand + `"`,
		"collaborators/${commenter}/permission",
		"status=action_required",
		approveEndpointFragment + "${run_id}/approve",
		"sentinel-alert",
		"::error::",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("workflow missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, PlaceholderAppLogin) {
		t.Fatalf("workflow should not keep the placeholder when a login is given")
	}
}

func TestWorkflowDefaultsAndOverrides(t *testing.T) {
	out := Workflow(Options{})
	if !strings.Contains(out, `HIVE_APP_LOGINS: "`+PlaceholderAppLogin+`"`) {
		t.Fatalf("placeholder missing:\n%s", out)
	}
	out = Workflow(Options{AppLogins: " a[bot], b[bot] ", TrustedAuthors: "c[bot]", Command: "/lets-go"})
	for _, want := range []string{`HIVE_APP_LOGINS: "a[bot], b[bot]"`, `TRUSTED_AUTHORS: "c[bot]"`, `COMMAND: "/lets-go"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("override missing %q:\n%s", want, out)
		}
	}
	if got := yamlScalar(`say "hi" \ there`); got != `"say \"hi\" \\ there"` {
		t.Fatalf("yamlScalar = %s", got)
	}
}

func TestWorkflowParsesAsYAMLAndCarriesTheScript(t *testing.T) {
	for name, src := range map[string]string{"workflow": Workflow(Options{AppLogins: "x[bot]"}), "reusable": ReusableWorkflow()} {
		var doc struct {
			Jobs map[string]struct {
				Permissions map[string]string `yaml:"permissions"`
				Steps       []struct {
					Run string            `yaml:"run"`
					Env map[string]string `yaml:"env"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatalf("%s: yaml: %v\n%s", name, err, src)
		}
		job, ok := doc.Jobs["ok-to-test"]
		if !ok || len(job.Steps) != 1 {
			t.Fatalf("%s: jobs = %+v", name, doc.Jobs)
		}
		if job.Permissions["actions"] != "write" || job.Permissions["issues"] != "write" ||
			job.Permissions["contents"] != "read" || job.Permissions["pull-requests"] != "read" {
			t.Fatalf("%s: permissions = %v", name, job.Permissions)
		}
		if strings.TrimRight(job.Steps[0].Run, "\n") != strings.TrimRight(Script(), "\n") {
			t.Fatalf("%s: run block diverged from approve.sh", name)
		}
		for _, key := range []string{"GH_TOKEN", "HIVE_APP_LOGINS", "TRUSTED_AUTHORS", "COMMAND"} {
			if _, ok := job.Steps[0].Env[key]; !ok {
				t.Fatalf("%s: env missing %s", name, key)
			}
		}
	}
	if ApproveEndpointFragment() != approveEndpointFragment {
		t.Fatal("ApproveEndpointFragment mismatch")
	}
}

// The committed copies are generated from this package; regenerate with
// `go test ./pkg/oktotest -run TestCommittedCopies -update`-style edits by
// writing Workflow(Options{}) / ReusableWorkflow() to the paths below.
func TestCommittedCopiesMatchRendering(t *testing.T) {
	if got, want := readRepoFile(t, "src/docs/templates/hive-ok-to-test.yml"), Workflow(Options{}); got != want {
		t.Fatalf("src/docs/templates/hive-ok-to-test.yml is stale; regenerate from oktotest.Workflow(Options{})")
	}
	if got, want := readRepoFile(t, ".github/workflows/hive-ok-to-test.yml"), ReusableWorkflow(); got != want {
		t.Fatalf(".github/workflows/hive-ok-to-test.yml is stale; regenerate from oktotest.ReusableWorkflow()")
	}
	if strings.Contains(readRepoFile(t, "src/docs/templates/hive-ok-to-test.yml"), "uses: hivecommons/hive/") {
		t.Fatal("template must not reference hivecommons/hive reusable workflows")
	}
}
