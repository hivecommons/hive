package githubactions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

const jobsOneStep = `
jobs:
  a:
    runs-on: ubuntu-latest
    steps: [{run: echo}]
`

func mustParse(t *testing.T, path, src string) (workflowmodel.Pipeline, []workflowmodel.Finding) {
	t.Helper()
	p, fs, err := ParseWorkflow(path, []byte(src))
	if err != nil {
		t.Fatalf("ParseWorkflow(%s): %v", path, err)
	}
	return p, fs
}

func hasFinding(fs []workflowmodel.Finding, code, node string) bool {
	for _, f := range fs {
		if f.Code == code && f.Node == node {
			return true
		}
	}
	return false
}

func TestIsWorkflowPath(t *testing.T) {
	cases := map[string]bool{
		".github/workflows/ci.yml":         true,
		".github/workflows/ci.yaml":        true,
		".github/workflows/.yml":           false,
		".github/workflows/sub/ci.yml":     false,
		".github/workflows/ci.json":        false,
		".github/workflows/README.md":      false,
		"workflows/ci.yml":                 false,
		".github/actions/setup/action.yml": false,
	}
	for p, want := range cases {
		if got := IsWorkflowPath(p); got != want {
			t.Errorf("IsWorkflowPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestTriggers(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []workflowmodel.Trigger
	}{
		{"string", "on: push\n" + jobsOneStep, []workflowmodel.Trigger{{Event: "push"}}},
		{"list", "on: [push, pull_request]\n" + jobsOneStep, []workflowmodel.Trigger{{Event: "push"}, {Event: "pull_request"}}},
		{"map", `
on:
  push:
    branches: [main]
    branches-ignore: [wip/*]
    tags: ["v*"]
    tags-ignore: ["v0*"]
    paths: [src/**]
    paths-ignore: ["**.md"]
  pull_request:
    types: [opened, synchronize]
  schedule:
    - cron: "0 3 * * *"
    - cron: "0 15 * * 1"
  workflow_dispatch:
    inputs:
      level: {type: string}
  workflow_call:
  workflow_run:
    workflows: [CI, Lint]
    types: [completed]
` + jobsOneStep, []workflowmodel.Trigger{
			{Event: "push", Branches: []string{"main", "!wip/*"}, Tags: []string{"v*", "!v0*"}, Paths: []string{"src/**", "!**.md"}},
			{Event: "pull_request", Types: []string{"opened", "synchronize"}},
			{Event: "schedule", Schedule: []string{"0 3 * * *", "0 15 * * 1"}},
			{Event: "workflow_dispatch"},
			{Event: "workflow_call"},
			{Event: "workflow_run", Types: []string{"CI", "Lint", "completed"}},
		}},
		{"bare-on-as-yaml11-bool", "on:\n  push:\n    branches: [main]\n" + jobsOneStep,
			[]workflowmodel.Trigger{{Event: "push", Branches: []string{"main"}}}},
		{"quoted-on", "\"on\": [push]\n" + jobsOneStep, []workflowmodel.Trigger{{Event: "push"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := mustParse(t, ".github/workflows/x.yml", tc.src)
			if !reflect.DeepEqual(p.Triggers, tc.want) {
				t.Errorf("triggers = %+v, want %+v", p.Triggers, tc.want)
			}
		})
	}
}

func TestOnKeyNotDecodedAsBoolFalse(t *testing.T) {
	// If the YAML library ever reads the bare `on` key as a boolean, the
	// workflow must still be recognised.
	p, _ := mustParse(t, ".github/workflows/x.yml", "name: X\non: push\njobs: {a: {steps: [{run: x}]}}\n")
	if len(p.Triggers) != 1 || p.Triggers[0].Event != "push" {
		t.Fatalf("triggers = %+v", p.Triggers)
	}
	if _, _, err := ParseWorkflow(".github/workflows/x.yml", []byte("name: X\n")); !errors.Is(err, ErrNotAWorkflow) {
		t.Fatalf("name-only doc: err = %v, want ErrNotAWorkflow", err)
	}
}

func TestNameDefaultsToBasename(t *testing.T) {
	p, _ := mustParse(t, ".github/workflows/nightly-build.yaml", "on: push\n"+jobsOneStep)
	if p.Name != "nightly-build" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.ID != workflowmodel.PipelineID(workflowmodel.SystemGitHubActions, p.Path) {
		t.Errorf("ID = %q", p.ID)
	}
	if p.System != workflowmodel.SystemGitHubActions {
		t.Errorf("System = %q", p.System)
	}
}

func TestJobs(t *testing.T) {
	src := `
on: push
jobs:
  a:
    name: A job
    runs-on: ubuntu-latest
    if: github.actor != 'bot'
    environment: staging
    steps: [{run: echo}]
  b:
    needs: a
    runs-on: [self-hosted, linux]
    environment:
      name: production
      url: https://example.com
    steps: [{run: echo}]
  c:
    needs: [a, b]
    runs-on:
      group: big-runners
      labels: [gpu]
    steps: [{run: echo}]
  d:
    needs: [c]
    uses: org/repo/.github/workflows/reusable.yml@main
    secrets: inherit
  e:
    runs-on: ${{ matrix.os }}
    environment: ${{ inputs.env }}
    container: {image: golang:1.23}
    services:
      db: {image: postgres}
    steps: [{run: echo}]
  f:
    runs-on: ubuntu-latest
`
	p, fs := mustParse(t, ".github/workflows/x.yml", src)
	want := []workflowmodel.Node{
		{ID: "a", Name: "A job", Kind: workflowmodel.NodeJob, If: "github.actor != 'bot'", Environment: "staging", RunsOn: []string{"ubuntu-latest"}},
		{ID: "b", Kind: workflowmodel.NodeJob, Needs: []string{"a"}, Environment: "production", RunsOn: []string{"self-hosted", "linux"}, Level: 1},
		{ID: "c", Kind: workflowmodel.NodeJob, Needs: []string{"a", "b"}, RunsOn: []string{"big-runners", "gpu"}, Level: 2},
		{ID: "d", Kind: workflowmodel.NodeReusableWorkflow, Needs: []string{"c"}, Ref: "org/repo/.github/workflows/reusable.yml@main", Level: 3},
		{ID: "e", Kind: workflowmodel.NodeJob, Environment: "${{ inputs.env }}", RunsOn: []string{"${{ matrix.os }}"}},
		{ID: "f", Kind: workflowmodel.NodeJob, RunsOn: []string{"ubuntu-latest"}},
	}
	if !reflect.DeepEqual(p.Nodes, want) {
		got, _ := json.MarshalIndent(p.Nodes, "", " ")
		t.Errorf("nodes:\n%s", got)
	}
	if p.Levels != 4 {
		t.Errorf("Levels = %d, want 4", p.Levels)
	}
	for _, f := range []struct{ code, node string }{
		{CodeSecretsInherit, "d"}, {CodeJobContainer, "e"}, {CodeJobEmpty, "f"},
	} {
		if !hasFinding(fs, f.code, f.node) {
			t.Errorf("missing finding %s on %s in %+v", f.code, f.node, fs)
		}
	}
	if hasFinding(fs, CodeJobEmpty, "d") {
		t.Errorf("reusable job must not be flagged job-empty")
	}
}

func TestMatrix(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want *workflowmodel.Matrix
	}{
		{"static", `
on: push
jobs:
  a:
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu, macos]
        go: ["1.22", "1.23"]
        include:
          - {os: windows, go: "1.23"}
        exclude:
          - {os: macos, go: "1.22"}
          - {os: ubuntu, go: "1.22"}
    steps: [{run: echo}]
`, &workflowmodel.Matrix{Keys: []string{"go", "os"}, Include: 1, Exclude: 2}},
		{"dynamic-whole", `
on: push
jobs:
  a:
    strategy:
      matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps: [{run: echo}]
`, &workflowmodel.Matrix{Dynamic: true}},
		{"dynamic-value", `
on: push
jobs:
  a:
    strategy:
      matrix:
        os: [ubuntu]
        version: ${{ fromJSON(needs.plan.outputs.versions) }}
    steps: [{run: echo}]
`, &workflowmodel.Matrix{Keys: []string{"os", "version"}, Dynamic: true}},
		{"none", "on: push\n" + jobsOneStep, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := mustParse(t, ".github/workflows/x.yml", tc.src)
			if !reflect.DeepEqual(p.Nodes[0].Matrix, tc.want) {
				t.Errorf("matrix = %+v, want %+v", p.Nodes[0].Matrix, tc.want)
			}
		})
	}
}

func TestRole(t *testing.T) {
	envJob := "\njobs:\n  deploy:\n    environment: prod\n    steps: [{run: echo}]\n"
	cases := []struct {
		name    string
		src     string
		want    workflowmodel.Role
		finding string
	}{
		{"pull_request", "on: pull_request\n" + jobsOneStep, workflowmodel.RolePRVerify, ""},
		{"pull_request_target", "on: [pull_request_target]\n" + jobsOneStep, workflowmodel.RolePRVerify, ""},
		{"merge_group", "on:\n  merge_group:\n" + jobsOneStep, workflowmodel.RolePRVerify, ""},
		{"pr+push", "on: [push, pull_request]\n" + jobsOneStep, workflowmodel.RolePRVerify, ""},
		{"pr+environment", "on: pull_request" + envJob, workflowmodel.RolePRVerify, CodeEnvironmentOnPR},
		{"push-tags", "on:\n  push:\n    tags: ['v*']\n" + jobsOneStep, workflowmodel.RoleRelease, ""},
		{"push-tags-ignore-only", "on:\n  push:\n    tags-ignore: ['v0*']\n" + jobsOneStep, workflowmodel.RolePushVerify, ""},
		{"release", "on:\n  release:\n    types: [published]\n" + jobsOneStep, workflowmodel.RoleRelease, ""},
		{"push-branches", "on:\n  push:\n    branches: [main]\n" + jobsOneStep, workflowmodel.RolePushVerify, ""},
		{"push-unrestricted", "on: push\n" + jobsOneStep, workflowmodel.RolePushVerify, ""},
		{"push+dispatch", "on: [push, workflow_dispatch]\n" + jobsOneStep, workflowmodel.RolePushVerify, ""},
		{"dispatch+environment", "on: workflow_dispatch" + envJob, workflowmodel.RoleDeploy, ""},
		{"deployment", "on: deployment\n" + jobsOneStep, workflowmodel.RoleDeploy, ""},
		{"deployment_status", "on: [deployment_status]\n" + jobsOneStep, workflowmodel.RoleDeploy, ""},
		{"schedule-only", "on:\n  schedule:\n    - cron: '0 0 * * *'\n" + jobsOneStep, workflowmodel.RoleScheduled, ""},
		{"dispatch-only", "on: workflow_dispatch\n" + jobsOneStep, workflowmodel.RoleManual, ""},
		{"call-only", "on:\n  workflow_call:\n    inputs:\n      x: {type: string}\n" + jobsOneStep, workflowmodel.RoleOther, CodeReusableOnly},
		{"schedule+dispatch", "on: [schedule, workflow_dispatch]\n" + jobsOneStep, workflowmodel.RoleScheduled, ""},
		{"schedule+dispatch+call", "on: [schedule, workflow_dispatch, workflow_call]\n" + jobsOneStep, workflowmodel.RoleScheduled, ""},
		{"schedule+issue_comment", "on: [schedule, issue_comment]\n" + jobsOneStep, workflowmodel.RoleOther, ""},
		{"dispatch+call", "on: [workflow_dispatch, workflow_call]\n" + jobsOneStep, workflowmodel.RoleManual, ""},
		{"dispatch+call-no-reusable-finding", "on: [workflow_call, workflow_dispatch]\n" + jobsOneStep, workflowmodel.RoleManual, ""},
		{"issue_comment", "on: issue_comment\n" + jobsOneStep, workflowmodel.RoleOther, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fs := mustParse(t, ".github/workflows/x.yml", tc.src)
			if p.Role != tc.want {
				t.Errorf("role = %q, want %q", p.Role, tc.want)
			}
			if tc.finding != "" && !hasFinding(fs, tc.finding, "") && !hasFinding(fs, tc.finding, "deploy") {
				t.Errorf("missing finding %s in %+v", tc.finding, fs)
			}
			if tc.finding == "" && hasFinding(fs, CodeReusableOnly, "") {
				t.Errorf("unexpected reusable-only finding in %+v", fs)
			}
		})
	}
}

func TestNeedsRepair(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		p, fs := mustParse(t, ".github/workflows/x.yml", `
on: push
jobs:
  a:
    steps: [{run: echo}]
  b:
    needs: [a, ghost]
    steps: [{run: echo}]
`)
		if !hasFinding(fs, CodeNeedsUnresolved, "b") {
			t.Fatalf("findings = %+v", fs)
		}
		if got := p.Nodes[1].Needs; !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("needs = %v, want [a]", got)
		}
		if p.Levels != 2 || p.Nodes[1].Level != 1 {
			t.Errorf("Levels = %d, b.Level = %d", p.Levels, p.Nodes[1].Level)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		p, fs := mustParse(t, ".github/workflows/x.yml", `
on: push
jobs:
  a:
    needs: b
    steps: [{run: echo}]
  b:
    needs: a
    steps: [{run: echo}]
`)
		if !hasFinding(fs, CodeNeedsCycle, "") {
			t.Fatalf("findings = %+v", fs)
		}
		if p.Levels != 1 {
			t.Errorf("Levels = %d, want 1", p.Levels)
		}
		for _, n := range p.Nodes {
			if n.Level != 0 {
				t.Errorf("node %s level = %d, want 0", n.ID, n.Level)
			}
		}
	})
}

func TestConcurrency(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    *workflowmodel.Concurrency
		finding bool
	}{
		{"none", "on: push\n" + jobsOneStep, nil, false},
		{"string", "on: push\nconcurrency: release\n" + jobsOneStep, &workflowmodel.Concurrency{Group: "release"}, false},
		{"map-true", "on: push\nconcurrency:\n  group: ci-${{ github.ref }}\n  cancel-in-progress: true\n" + jobsOneStep,
			&workflowmodel.Concurrency{Group: "ci-${{ github.ref }}", CancelInProgress: true}, false},
		{"map-false", "on: push\nconcurrency:\n  group: g\n  cancel-in-progress: false\n" + jobsOneStep,
			&workflowmodel.Concurrency{Group: "g"}, false},
		{"map-expression", "on: push\nconcurrency:\n  group: g\n  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n" + jobsOneStep,
			&workflowmodel.Concurrency{Group: "g"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fs := mustParse(t, ".github/workflows/x.yml", tc.src)
			if !reflect.DeepEqual(p.Concurrency, tc.want) {
				t.Errorf("concurrency = %+v, want %+v", p.Concurrency, tc.want)
			}
			if got := hasFinding(fs, CodeConcurrencyExpression, ""); got != tc.finding {
				t.Errorf("concurrency-expression finding = %v, want %v", got, tc.finding)
			}
		})
	}
}

func TestNoJobs(t *testing.T) {
	for _, src := range []string{"on: push\n", "on: push\njobs: {}\n", "on: push\njobs:\n"} {
		p, fs := mustParse(t, ".github/workflows/x.yml", src)
		if len(p.Nodes) != 0 || p.Levels != 0 || !hasFinding(fs, CodeNoJobs, "") {
			t.Errorf("%q: nodes=%d levels=%d findings=%+v", src, len(p.Nodes), p.Levels, fs)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want error
	}{
		{"yaml", "on: [push\njobs: {", ErrYAML},
		{"scalar-doc", "just a string\n", ErrNotAWorkflow},
		{"list-doc", "- a\n- b\n", ErrNotAWorkflow},
		{"empty-doc", "", ErrNotAWorkflow},
		{"action-metadata", "name: Setup\nruns:\n  using: composite\n", ErrNotAWorkflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseWorkflow(".github/workflows/x.yml", []byte(tc.src))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestIngest(t *testing.T) {
	files := []File{
		{Path: ".github/workflows/zeta.yml", Data: []byte("on: push\n" + jobsOneStep)},
		{Path: ".github/workflows/broken.yml", Data: []byte("on: [\n")},
		{Path: ".github/workflows/alpha.yml", Data: []byte("on: pull_request\njobs:\n  b:\n    uses: o/r/.github/workflows/w.yml@v1\n    secrets: inherit\n  a:\n    runs-on: x\n")},
		{Path: ".github/workflows/action.yml", Data: []byte("name: not a workflow\nruns: {using: node20}\n")},
	}
	r1 := Ingest(files)
	r2 := Ingest(files)
	j1, _ := json.Marshal(r1)
	j2, _ := json.Marshal(r2)
	if string(j1) != string(j2) {
		t.Fatalf("Ingest is not deterministic:\n%s\n%s", j1, j2)
	}

	if r1.Coverage.Files != 4 || r1.Coverage.Parsed != 2 {
		t.Errorf("coverage = files %d parsed %d", r1.Coverage.Files, r1.Coverage.Parsed)
	}
	if len(r1.Sources) != 4 {
		t.Errorf("sources = %d", len(r1.Sources))
	}
	for i, s := range r1.Sources {
		if s.System != workflowmodel.SystemGitHubActions || s.SHA256 == "" {
			t.Errorf("source %d = %+v", i, s)
		}
	}
	var paths []string
	for _, p := range r1.Pipelines {
		paths = append(paths, p.Path)
	}
	if !reflect.DeepEqual(paths, []string{".github/workflows/alpha.yml", ".github/workflows/zeta.yml"}) {
		t.Errorf("pipeline order = %v", paths)
	}
	if ids := []string{r1.Pipelines[0].Nodes[0].ID, r1.Pipelines[0].Nodes[1].ID}; !reflect.DeepEqual(ids, []string{"b", "a"}) {
		t.Errorf("node order = %v, want declaration order [b a]", ids)
	}

	var keys []string
	for _, f := range r1.Coverage.Findings {
		keys = append(keys, f.Path+"|"+f.Node+"|"+f.Code)
	}
	wantKeys := []string{
		".github/workflows/action.yml||" + CodeNotAWorkflow,
		".github/workflows/alpha.yml|a|" + CodeJobEmpty,
		".github/workflows/alpha.yml|b|" + CodeSecretsInherit,
		".github/workflows/broken.yml||" + CodeYAMLParse,
	}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("findings = %v\nwant %v", keys, wantKeys)
	}
	for _, f := range r1.Coverage.Findings {
		if (f.Code == CodeYAMLParse || f.Code == CodeNotAWorkflow) && f.Severity != workflowmodel.SeverityUnknown {
			t.Errorf("%s severity = %s", f.Code, f.Severity)
		}
	}
}

func TestFixtures(t *testing.T) {
	read := func(name string) File {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return File{Path: ".github/workflows/" + name, Data: b}
	}
	r := Ingest([]File{read("release.yml"), read("kubernetes-ci.yml")})
	if r.Coverage.Files != 2 || r.Coverage.Parsed != 2 || len(r.Coverage.Findings) != 0 {
		t.Fatalf("coverage = %+v", r.Coverage)
	}

	ci := r.Pipelines[0]
	if ci.Name != "CI" || ci.Role != workflowmodel.RolePRVerify || ci.Levels != 4 {
		t.Errorf("ci: name=%q role=%q levels=%d", ci.Name, ci.Role, ci.Levels)
	}
	wantLevels := map[string]int{"lint": 0, "build": 1, "unit": 2, "integration": 2, "e2e": 3}
	if len(ci.Nodes) != len(wantLevels) {
		t.Fatalf("ci nodes = %d", len(ci.Nodes))
	}
	for _, n := range ci.Nodes {
		if n.Level != wantLevels[n.ID] {
			t.Errorf("ci %s level = %d, want %d", n.ID, n.Level, wantLevels[n.ID])
		}
	}
	if got := workflowmodel.ByLevel(ci.Nodes); !reflect.DeepEqual(got, [][]string{{"lint"}, {"build"}, {"integration", "unit"}, {"e2e"}}) {
		t.Errorf("ByLevel = %v", got)
	}
	integ := ci.Nodes[3]
	if integ.Matrix == nil || !reflect.DeepEqual(integ.Matrix.Keys, []string{"go", "os"}) || integ.Matrix.Exclude != 1 || integ.Matrix.Dynamic {
		t.Errorf("integration matrix = %+v", integ.Matrix)
	}
	if !strings.HasPrefix(integ.Name, "Integration (") {
		t.Errorf("integration name = %q", integ.Name)
	}
	if ci.Concurrency == nil || !ci.Concurrency.CancelInProgress {
		t.Errorf("ci concurrency = %+v", ci.Concurrency)
	}
	if len(ci.Triggers) != 2 || ci.Triggers[0].Event != "pull_request" || !reflect.DeepEqual(ci.Triggers[0].Paths, []string{"!docs/**", "!**.md"}) {
		t.Errorf("ci triggers = %+v", ci.Triggers)
	}

	rel := r.Pipelines[1]
	if rel.Name != "Release" || rel.Role != workflowmodel.RoleRelease || rel.Levels != 2 {
		t.Errorf("release: name=%q role=%q levels=%d", rel.Name, rel.Role, rel.Levels)
	}
	if rel.Nodes[1].Environment != "production" || rel.Nodes[1].Level != 1 {
		t.Errorf("release publish-image = %+v", rel.Nodes[1])
	}
}
