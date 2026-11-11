package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureCorpus is the curated ingest corpus shared with
// pkg/workflowmodel/ingest. The test copies it (symlinks would fail the
// collector layout's IsDir check) into the layout the collector writes.
const fixtureCorpus = "../../pkg/workflowmodel/testdata/corpus"

// buildCorpus assembles a <corpus>/repos/<owner>__<repo> tree from the
// fixture directories plus a projects.json mapping one repo to a maturity.
func buildCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repos := filepath.Join(root, "repos")
	entries, err := os.ReadDir(fixtureCorpus)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src, err := filepath.Abs(filepath.Join(fixtureCorpus, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(repos, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(repos, e.Name()), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no fixture repositories under %s", fixtureCorpus)
	}
	// One repo with process conventions but no pipelines exercises the
	// intake counts (RFC phase 5).
	intake, err := filepath.Abs(filepath.Join("..", "..", "pkg", "workflowmodel", "ingest", "testdata", "k8s-owners"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(repos, "example__k8s-owners"), os.DirFS(intake)); err != nil {
		t.Fatal(err)
	}
	// A stray file in repos/ must be ignored, as must a repo not in projects.json.
	if err := os.WriteFile(filepath.Join(repos, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	projects := `[{"project":"graduated","repos":["ArgoProj/argo-cd"]},{"project":"incubating","repos":["cilium/cilium"]}]`
	if err := os.WriteFile(filepath.Join(root, "projects.json"), []byte(projects), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRealMainSurveysCorpus(t *testing.T) {
	corpus := buildCorpus(t)
	var stdout, stderr bytes.Buffer
	if code := realMain([]string{"-corpus", corpus}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr: %s", stderr.String())
	}
	for _, repo := range []string{"argoproj/argo-cd", "tektoncd/pipeline", "konflux-ci/konflux-ci"} {
		if !strings.Contains(stdout.String(), repo) {
			t.Errorf("stdout missing %s:\n%s", repo, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "INVALID") {
		t.Errorf("fixture model reported invalid:\n%s", stdout.String())
	}

	out := filepath.Join(corpus, "out")
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sum summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Repos != 7 || sum.Valid != sum.Repos {
		t.Errorf("repos=%d valid=%d, want 7 valid", sum.Repos, sum.Valid)
	}
	if sum.Compiled != sum.Repos {
		t.Errorf("compiled=%d of %d", sum.Compiled, sum.Repos)
	}
	if in := sum.Intake; in.ReposWithIntakeStates != 1 || in.ReposWithTriage != 1 || in.ReposWithRoles != 1 || in.ReposWithGates != 1 || in.ReposWithChecks != 1 || in.ReposWithBranchRules != 1 {
		t.Errorf("intake summary %+v, want exactly the k8s-owners repo", in)
	}
	for _, r := range sum.Repos_ {
		if r.Repo == "example/k8s-owners" && (r.Intake.IntakeStates != 3 || !r.Intake.Triage || r.Intake.MemberRoles != 2 || r.Intake.ManualChecks != 4 || r.Intake.CIChecks != 3 || r.Intake.IntakeFindings != 1) {
			t.Errorf("k8s-owners intake %+v", r.Intake)
		}
	}
	if sum.ReposWithGHA == 0 || sum.ReposWithTekton == 0 {
		t.Errorf("expected both systems in the corpus: gha=%d tekton=%d", sum.ReposWithGHA, sum.ReposWithTekton)
	}
	if sum.ReposChangedCols == 0 {
		t.Error("expected at least one repo whose columns differ from the hive default")
	}
	if got := sum.ByProject["graduated"]; got == nil || got.Repos != 1 {
		t.Errorf("projects.json maturity join (case-insensitive) not applied: %+v", sum.ByProject)
	}
	if got := sum.ByProject["unknown"]; got == nil || got.Repos != 5 {
		t.Errorf("repos without a maturity should land in unknown: %+v", sum.ByProject)
	}
	if len(sum.Worst) != sum.Repos || len(sum.Top) != sum.Repos {
		t.Errorf("worst/top should list every repo when under the cap: %d/%d", len(sum.Worst), len(sum.Top))
	}
	for i := 1; i < len(sum.Top); i++ {
		if sum.Top[i-1].Pipelines < sum.Top[i].Pipelines {
			t.Errorf("top not sorted by pipelines desc at %d", i)
		}
	}

	md, err := os.ReadFile(filepath.Join(out, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Workflow model survey", "## Pipeline roles", "## Column sets (Activity Monitor)", "## Intake proposals (RFC phase 5)", "| Triage state | 1 |", "| graduated | 1 |", "trigger:pull_request"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("summary.md missing %q", want)
		}
	}

	for _, f := range []string{"model.json", "columns.json", "diagram.md", "result.json"} {
		if _, err := os.Stat(filepath.Join(out, "argoproj__argo-cd", f)); err != nil {
			t.Errorf("per-repo artifact missing: %v", err)
		}
	}
	diagram, _ := os.ReadFile(filepath.Join(out, "konflux-ci__konflux-ci", "diagram.md"))
	if !strings.Contains(string(diagram), "stateDiagram-v2") || !strings.Contains(string(diagram), "flowchart") {
		t.Error("diagram.md should carry the state diagram and per-pipeline flowcharts")
	}
}

func TestRealMainOnlyFilterAndOutDir(t *testing.T) {
	corpus := buildCorpus(t)
	out := filepath.Join(t.TempDir(), "nested", "out")
	var stdout, stderr bytes.Buffer
	code := realMain([]string{"-corpus", corpus, "-out", out, "-only", " cilium/cilium , envoyproxy/envoy"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("filter should leave two repos, got %d:\n%s", len(lines), stdout.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sum summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Repos != 2 {
		t.Errorf("repos=%d, want 2", sum.Repos)
	}
	if _, err := os.Stat(filepath.Join(out, "argoproj__argo-cd")); !os.IsNotExist(err) {
		t.Error("filtered-out repo should not be written")
	}
}

func TestRealMainErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no corpus flag", nil, exitUsage, "usage:"},
		{"unknown flag", []string{"-bogus"}, exitUsage, "flag provided but not defined"},
		{"missing corpus dir", []string{"-corpus", filepath.Join(t.TempDir(), "nope")}, exitFailure, "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := realMain(tc.args, &stdout, &stderr); code != tc.code {
				t.Errorf("exit %d, want %d", code, tc.code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr %q should contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunReportsUnreadableRepo(t *testing.T) {
	corpus := buildCorpus(t)
	// A repo directory containing an unreadable file is reported on
	// stderr and skipped rather than aborting the survey.
	bad := filepath.Join(corpus, "repos", "acme__broken", ".github", "workflows")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "ci.yml"), []byte("on: push\njobs: {}\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		t.Skip("root can read mode 0000 files")
	}
	var stdout, stderr bytes.Buffer
	if code := realMain([]string{"-corpus", corpus}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "acme/broken") || !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr should name the skipped repo: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "acme/broken") {
		t.Error("skipped repo must not be counted")
	}
}

func TestRunFailsWhenOutputUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	corpus := buildCorpus(t)
	out := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(out, 0o500); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := realMain([]string{"-corpus", corpus, "-out", out}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr %q should explain the write failure", stderr.String())
	}
}

func TestHelpers(t *testing.T) {
	if pct(1, 0) != 0 || pct(1, 4) != 25 {
		t.Error("pct")
	}
	if ternary(true, "a", "b") != "a" || ternary(false, "a", "b") != "b" {
		t.Error("ternary")
	}
	if got := sortedKeys(map[string]int{"b": 1, "a": 2}); strings.Join(got, ",") != "a,b" {
		t.Errorf("sortedKeys = %v", got)
	}
	if m := loadProjects(t.TempDir()); m["project-akri/akri"] != "sandbox" {
		t.Errorf("loadProjects without projects.json should fall back to the embedded corpus, got %d entries", len(m))
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := loadProjects(dir); len(m) != 0 {
		t.Errorf("malformed projects.json should yield no entries, got %v", m)
	}
	if err := writeJSON(filepath.Join(dir, "x.json"), make(chan int)); err == nil {
		t.Error("writeJSON should surface marshal errors")
	}
}
