package tekton

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func file(path, data string) File { return File{Path: path, Data: []byte(data)} }

func findPipeline(t *testing.T, r Result, name string) workflowmodel.Pipeline {
	t.Helper()
	for _, p := range r.Pipelines {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("pipeline %q not found; have %v", name, pipelineNames(r))
	return workflowmodel.Pipeline{}
}

func pipelineNames(r Result) []string {
	var out []string
	for _, p := range r.Pipelines {
		out = append(out, p.Name)
	}
	return out
}

func findNode(t *testing.T, p workflowmodel.Pipeline, id string) workflowmodel.Node {
	t.Helper()
	for _, n := range p.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not in pipeline %q", id, p.Name)
	return workflowmodel.Node{}
}

func findingCodes(r Result) []string {
	var out []string
	for _, f := range r.Coverage.Findings {
		out = append(out, f.Code)
	}
	return out
}

func hasFinding(r Result, code string) bool {
	for _, f := range r.Coverage.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func findingByCode(t *testing.T, r Result, code string) workflowmodel.Finding {
	t.Helper()
	for _, f := range r.Coverage.Findings {
		if f.Code == code {
			return f
		}
	}
	t.Fatalf("finding %q not present; have %v", code, findingCodes(r))
	return workflowmodel.Finding{}
}

const pipelineWithTask = `apiVersion: tekton.dev/v1
kind: Task
metadata:
  name: unit
spec:
  steps:
    - name: go
      image: golang
      script: go test ./...
---
apiVersion: tekton.dev/v1
kind: Pipeline
metadata:
  name: verify
spec:
  tasks:
    - name: unit
      taskRef:
        name: unit
    - name: lint
      taskSpec:
        steps:
          - image: golangci/golangci-lint
`

const pacRun = `apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  generateName: build-on-pr-
  annotations:
    pipelinesascode.tekton.dev/on-event: "[pull_request]"
    pipelinesascode.tekton.dev/on-target-branch: "[main, 'release-*']"
    pipelinesascode.tekton.dev/on-path-change: "[src/***]"
spec:
  pipelineSpec:
    tasks:
      - name: clone
        taskRef:
          resolver: git
          params:
            - name: url
              value: https://github.com/example/catalog.git
            - name: pathInRepo
              value: tasks/git-clone.yaml
      - name: build
        runAfter: [clone]
        taskRef:
          name: buildah
        params:
          - name: context
            value: $(tasks.clone.results.path)
      - name: test
        params:
          - name: image
            value: $(tasks.build.results.IMAGE_URL)
        when:
          - input: $(params.skip-tests)
            operator: notin
            values: ["true"]
        taskRef:
          name: go-test
    finally:
      - name: notify
        taskRef:
          name: slack
`

func TestIngestMultiDocPipelineAndTask(t *testing.T) {
	r := Ingest([]File{file(".tekton/verify.yaml", pipelineWithTask)})
	if len(r.Pipelines) != 1 {
		t.Fatalf("pipelines = %d, want 1", len(r.Pipelines))
	}
	p := r.Pipelines[0]
	if p.ID != "tkn-tekton-verify" || p.Name != "verify" || p.System != workflowmodel.SystemTekton || p.Path != ".tekton/verify.yaml" {
		t.Errorf("pipeline identity = %+v", p)
	}
	if p.Role != workflowmodel.RoleOther {
		t.Errorf("role = %q, want other", p.Role)
	}
	if got := findNode(t, p, "unit").Ref; got != "unit" {
		t.Errorf("unit ref = %q", got)
	}
	if got := findNode(t, p, "lint").Ref; got != refInline {
		t.Errorf("lint ref = %q, want inline", got)
	}
	if p.Levels != 1 {
		t.Errorf("levels = %d, want 1", p.Levels)
	}
	tasks := findingByCode(t, r, CodeTaskDefinitions)
	if tasks.Detail != "1 task/stepaction documents" || tasks.Severity != workflowmodel.SeverityWarning {
		t.Errorf("task-definitions finding = %+v", tasks)
	}
	if !hasFinding(r, CodeNoTrigger) {
		t.Errorf("want no-trigger finding; have %v", findingCodes(r))
	}
	if r.Coverage.Files != 1 || r.Coverage.Parsed != 1 {
		t.Errorf("coverage = %+v", r.Coverage)
	}
	if len(r.Sources) != 1 || r.Sources[0].SHA256 != workflowmodel.SHA256Hex([]byte(pipelineWithTask)) {
		t.Errorf("sources = %+v", r.Sources)
	}
}

func TestIngestEmbeddedSpecWithPaC(t *testing.T) {
	r := Ingest([]File{file(".tekton/build.yaml", pacRun)})
	if len(r.Pipelines) != 1 {
		t.Fatalf("pipelines = %d, want 1 (%v)", len(r.Pipelines), pipelineNames(r))
	}
	p := r.Pipelines[0]
	if p.Name != "build-on-pr" {
		t.Errorf("name = %q, want generateName trimmed", p.Name)
	}
	if p.Role != workflowmodel.RolePRVerify {
		t.Errorf("role = %q, want pr-verify", p.Role)
	}
	wantTrigger := workflowmodel.Trigger{Event: "pull_request", Branches: []string{"main", "release-*"}, Paths: []string{"src/***"}}
	if len(p.Triggers) != 1 || !reflect.DeepEqual(p.Triggers[0], wantTrigger) {
		t.Errorf("triggers = %+v, want [%+v]", p.Triggers, wantTrigger)
	}
	if got := findNode(t, p, "clone").Ref; got != "git:https://github.com/example/catalog.git/tasks/git-clone.yaml" {
		t.Errorf("clone ref = %q", got)
	}
	if got := findNode(t, p, "build").Needs; !reflect.DeepEqual(got, []string{"clone"}) {
		t.Errorf("build needs = %v", got)
	}
	test := findNode(t, p, "test")
	if !reflect.DeepEqual(test.Needs, []string{"build"}) {
		t.Errorf("test implicit needs = %v, want [build]", test.Needs)
	}
	if !reflect.DeepEqual(test.When, []string{"$(params.skip-tests) notin [true]"}) {
		t.Errorf("test when = %v", test.When)
	}
	notify := findNode(t, p, "notify")
	if notify.Kind != workflowmodel.NodeFinally {
		t.Errorf("notify kind = %q", notify.Kind)
	}
	if !reflect.DeepEqual(notify.Needs, []string{"clone", "build", "test"}) {
		t.Errorf("notify needs = %v", notify.Needs)
	}
	if p.Levels != 4 || notify.Level != 3 {
		t.Errorf("levels = %d, notify level = %d; want 4 and 3", p.Levels, notify.Level)
	}
	f := findingByCode(t, r, CodeFinally)
	if f.Severity != workflowmodel.SeverityWarning {
		t.Errorf("finally finding = %+v", f)
	}
	if hasFinding(r, CodeNoTrigger) {
		t.Errorf("unexpected no-trigger finding")
	}
}

func TestIngestPipelineRunReferences(t *testing.T) {
	const pipeline = `apiVersion: tekton.dev/v1beta1
kind: Pipeline
metadata:
  name: release
spec:
  tasks:
    - name: publish
      taskRef:
        name: publish
`
	const runs = `apiVersion: tekton.dev/v1beta1
kind: PipelineRun
metadata:
  name: release-on-tag
  annotations:
    pipelinesascode.tekton.dev/on-event: "[push]"
    pipelinesascode.tekton.dev/on-target-branch: "[refs/tags/*]"
spec:
  pipelineRef:
    name: release
---
apiVersion: tekton.dev/v1beta1
kind: PipelineRun
metadata:
  name: orphan
spec:
  pipelineRef:
    name: does-not-exist
`
	r := Ingest([]File{file("tekton/release.yaml", pipeline), file(".tekton/runs.yaml", runs)})
	if len(r.Pipelines) != 1 {
		t.Fatalf("pipelines = %v, want only release", pipelineNames(r))
	}
	p := r.Pipelines[0]
	if p.Role != workflowmodel.RoleRelease {
		t.Errorf("role = %q, want release", p.Role)
	}
	if len(p.Triggers) != 1 || p.Triggers[0].Event != "push" || !reflect.DeepEqual(p.Triggers[0].Branches, []string{"refs/tags/*"}) {
		t.Errorf("triggers = %+v", p.Triggers)
	}
	ext := findingByCode(t, r, CodePipelineRefExternal)
	if ext.Path != ".tekton/runs.yaml" || !strings.Contains(ext.Detail, "does-not-exist") || ext.Severity != workflowmodel.SeverityWarning {
		t.Errorf("pipelineref-external = %+v", ext)
	}
	if hasFinding(r, CodeNoTrigger) {
		t.Errorf("release has a run; no-trigger must not fire")
	}
}

func TestIngestKubernetesList(t *testing.T) {
	const list = `apiVersion: v1
kind: List
items:
  - apiVersion: tekton.dev/v1
    kind: Pipeline
    metadata:
      name: alpha
    spec:
      tasks:
        - name: a
          taskRef:
            name: a
  - apiVersion: tekton.dev/v1
    kind: Pipeline
    metadata:
      name: beta
    spec:
      tasks:
        - name: b
          taskRef:
            name: b
`
	r := Ingest([]File{file("all.yaml", list)})
	if got := pipelineNames(r); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("pipelines = %v", got)
	}
	if r.Pipelines[0].ID != "tkn-all-alpha" || r.Pipelines[1].ID != "tkn-all-beta" {
		t.Errorf("ids = %q %q, want name-qualified", r.Pipelines[0].ID, r.Pipelines[1].ID)
	}
}

func TestIngestTriggerTemplateWebhook(t *testing.T) {
	const docs = `apiVersion: tekton.dev/v1
kind: Pipeline
metadata:
  name: deploy
spec:
  tasks:
    - name: apply
      taskRef:
        name: kubectl
---
apiVersion: triggers.tekton.dev/v1beta1
kind: TriggerTemplate
metadata:
  name: deploy-template
spec:
  resourcetemplates:
    - apiVersion: tekton.dev/v1
      kind: PipelineRun
      metadata:
        generateName: deploy-
      spec:
        pipelineRef:
          name: deploy
---
apiVersion: triggers.tekton.dev/v1beta1
kind: EventListener
metadata:
  name: listener
spec:
  triggers: []
`
	r := Ingest([]File{file("triggers.yaml", docs)})
	p := findPipeline(t, r, "deploy")
	if len(p.Triggers) != 1 || p.Triggers[0].Event != "webhook" {
		t.Errorf("triggers = %+v, want webhook", p.Triggers)
	}
	if p.Role != workflowmodel.RoleOther {
		t.Errorf("role = %q, want other", p.Role)
	}
	var details []string
	for _, f := range r.Coverage.Findings {
		if f.Code == CodeTektonTriggers {
			if f.Severity != workflowmodel.SeverityUnsupported {
				t.Errorf("tekton-triggers severity = %q", f.Severity)
			}
			details = append(details, f.Detail)
		}
	}
	if !reflect.DeepEqual(details, []string{"EventListener listener", "TriggerTemplate deploy-template"}) {
		t.Errorf("tekton-triggers details = %v", details)
	}
}

func TestIngestCustomTaskAndBundleFindings(t *testing.T) {
	const doc = `apiVersion: tekton.dev/v1beta1
kind: Pipeline
metadata:
  name: mixed
spec:
  resources:
    - name: src
      type: git
  tasks:
    - name: legacy
      taskRef:
        name: git-clone
        bundle: gcr.io/tekton-releases/catalog/upstream/git-clone:0.9
    - name: approve
      onError: continue
      taskRef:
        apiVersion: openshift-pipelines.org/v1alpha1
        kind: ApprovalTask
        name: approve
`
	r := Ingest([]File{file("mixed.yaml", doc)})
	cases := []struct {
		code string
		node string
		sev  workflowmodel.Severity
		want string
	}{
		{CodeTaskRefBundle, "legacy", workflowmodel.SeverityWarning, "gcr.io/tekton-releases"},
		{CodeCustomTask, "approve", workflowmodel.SeverityUnsupported, "ApprovalTask"},
		{CodeOnError, "approve", workflowmodel.SeverityWarning, "onError"},
		{CodePipelineResources, "", workflowmodel.SeverityUnsupported, "PipelineResources"},
	}
	for _, c := range cases {
		f := findingByCode(t, r, c.code)
		if f.Node != c.node || f.Severity != c.sev || !strings.Contains(f.Detail, c.want) {
			t.Errorf("%s = %+v", c.code, f)
		}
	}
}

func TestIngestRunAfterCycleAndUnknown(t *testing.T) {
	const doc = `apiVersion: tekton.dev/v1
kind: Pipeline
metadata:
  name: broken
spec:
  tasks:
    - name: a
      runAfter: [b]
      taskRef: {name: t}
    - name: b
      runAfter: [a, ghost]
      taskRef: {name: t}
`
	r := Ingest([]File{file("broken.yaml", doc)})
	p := findPipeline(t, r, "broken")
	if p.Levels != 2 {
		t.Errorf("levels = %d, want 2 after breaking the cycle", p.Levels)
	}
	if _, err := workflowmodel.Levels(p.Nodes); err != nil {
		t.Errorf("Levels on emitted nodes: %v", err)
	}
	unresolved := findingByCode(t, r, CodeRunAfterUnresolved)
	if unresolved.Node != "b" || !strings.Contains(unresolved.Detail, "ghost") || unresolved.Severity != workflowmodel.SeverityUnknown {
		t.Errorf("runafter-unresolved = %+v", unresolved)
	}
	cycle := findingByCode(t, r, CodeRunAfterCycle)
	if cycle.Severity != workflowmodel.SeverityUnknown {
		t.Errorf("runafter-cycle = %+v", cycle)
	}
}

func TestIngestMatrix(t *testing.T) {
	const doc = `apiVersion: tekton.dev/v1
kind: Pipeline
metadata:
  name: matrix
spec:
  tasks:
    - name: static
      taskRef: {name: t}
      matrix:
        params:
          - name: platform
            value: [linux, darwin]
          - name: arch
            value: [amd64, arm64]
        include:
          - name: extra
            params:
              - name: platform
                value: windows
    - name: dynamic
      taskRef: {name: t}
      matrix:
        params:
          - name: platform
            value: ["$(params.platforms[*])"]
`
	r := Ingest([]File{file("matrix.yaml", doc)})
	p := findPipeline(t, r, "matrix")
	static := findNode(t, p, "static").Matrix
	want := &workflowmodel.Matrix{Keys: []string{"platform", "arch"}, Include: 1}
	if !reflect.DeepEqual(static, want) {
		t.Errorf("static matrix = %+v, want %+v", static, want)
	}
	dynamic := findNode(t, p, "dynamic").Matrix
	if dynamic == nil || !dynamic.Dynamic || !reflect.DeepEqual(dynamic.Keys, []string{"platform"}) {
		t.Errorf("dynamic matrix = %+v", dynamic)
	}
}

func TestIngestNotTektonAndParseError(t *testing.T) {
	cases := []struct {
		name string
		data string
		code string
	}{
		{"deployment", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n", CodeNotTekton},
		{"bad yaml", "apiVersion: tekton.dev/v1\nkind: Pipeline\nspec: [unclosed\n", CodeYAML},
		{"empty", "", CodeNotTekton},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Ingest([]File{file("x.yaml", c.data)})
			if len(r.Pipelines) != 0 {
				t.Errorf("pipelines = %v, want none", pipelineNames(r))
			}
			f := findingByCode(t, r, c.code)
			if f.Severity != workflowmodel.SeverityUnknown {
				t.Errorf("severity = %q", f.Severity)
			}
			if r.Coverage.Files != 1 || r.Coverage.Parsed != 0 {
				t.Errorf("coverage = %+v", r.Coverage)
			}
			if len(r.Sources) != 1 {
				t.Errorf("sources = %d, want 1 even for bad files", len(r.Sources))
			}
		})
	}
}

func TestIngestDeterministic(t *testing.T) {
	files := []File{
		file("z/pipeline.yaml", pipelineWithTask),
		file("a/build.yaml", pacRun),
		file("m/list.yaml", "apiVersion: v1\nkind: List\nitems:\n- apiVersion: tekton.dev/v1\n  kind: Pipeline\n  metadata: {name: zeta}\n  spec: {tasks: [{name: t, taskRef: {name: t}}]}\n- apiVersion: tekton.dev/v1\n  kind: Pipeline\n  metadata: {name: alpha}\n  spec: {tasks: [{name: t, taskRef: {name: t}}]}\n"),
	}
	first := Ingest(files)
	if got := pipelineNames(first); !reflect.DeepEqual(got, []string{"build-on-pr", "alpha", "zeta", "verify"}) {
		t.Errorf("pipeline order = %v", got)
	}
	for i := 0; i < 5; i++ {
		again := Ingest(files)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n%+v\n%+v", i, first, again)
		}
	}
	for i := 1; i < len(first.Coverage.Findings); i++ {
		a, b := first.Coverage.Findings[i-1], first.Coverage.Findings[i]
		if a.Path > b.Path || (a.Path == b.Path && a.Node > b.Node) || (a.Path == b.Path && a.Node == b.Node && a.Code > b.Code) {
			t.Errorf("findings not sorted at %d: %+v > %+v", i, a, b)
		}
	}
}

func TestIngestDedupesTriggersAcrossRuns(t *testing.T) {
	const pipeline = `apiVersion: tekton.dev/v1
kind: Pipeline
metadata:
  name: shared
spec:
  tasks:
    - name: t
      taskRef: {name: t}
`
	run := func(name, branches string) string {
		return `apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  name: ` + name + `
  annotations:
    pipelinesascode.tekton.dev/on-cel-expression: event == "pull_request" && target_branch == "main"
    pipelinesascode.tekton.dev/on-event: "[pull_request, push]"
    pipelinesascode.tekton.dev/on-target-branch: "` + branches + `"
spec:
  pipelineRef:
    name: shared
`
	}
	files := []File{file("pipeline.yaml", pipeline)}
	// Five runs with identical annotations, one with the same branches in
	// a different order, one with a genuinely different branch set.
	for i, b := range []string{"[main, release-1.35]", "[main, release-1.35]", "[main, release-1.35]", "[main, release-1.35]", "[main, release-1.35]", "[release-1.35, main]", "[release-1.36]"} {
		files = append(files, file("run-"+string(rune('a'+i))+".yaml", run("r"+string(rune('a'+i)), b)))
	}
	p := findPipeline(t, Ingest(files), "shared")
	want := []workflowmodel.Trigger{
		{Event: "pull_request", Branches: []string{"main", "release-1.35"}},
		{Event: "push", Branches: []string{"main", "release-1.35"}},
		{Event: "cel", Types: []string{`event == "pull_request" && target_branch == "main"`}},
		{Event: "pull_request", Branches: []string{"release-1.36"}},
		{Event: "push", Branches: []string{"release-1.36"}},
	}
	if !reflect.DeepEqual(p.Triggers, want) {
		t.Errorf("triggers =\n%+v\nwant\n%+v", p.Triggers, want)
	}
	if p.Role != workflowmodel.RolePRVerify {
		t.Errorf("role = %q", p.Role)
	}
}

func TestIngestCELExpression(t *testing.T) {
	const doc = `apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  name: nightly
  annotations:
    pipelinesascode.tekton.dev/on-cel-expression: event == "push" && target_branch == "main"
spec:
  pipelineSpec:
    tasks:
      - name: t
        taskRef: {name: t}
`
	r := Ingest([]File{file("cel.yaml", doc)})
	p := findPipeline(t, r, "nightly")
	if p.Role != workflowmodel.RolePushVerify {
		t.Errorf("role = %q, want push-verify", p.Role)
	}
	if len(p.Triggers) != 2 || p.Triggers[0].Event != "cel" || p.Triggers[1].Event != "push" {
		t.Errorf("triggers = %+v", p.Triggers)
	}
}

func TestIngestHelpers(t *testing.T) {
	if !LooksTekton([]byte("kind: Pipeline\n")) || !LooksTekton([]byte("apiVersion: tekton.dev/v1\n")) || LooksTekton([]byte("kind: Deployment\n")) {
		t.Error("LooksTekton misclassified")
	}
	if !IsTektonDocument(map[string]any{"apiVersion": "triggers.tekton.dev/v1beta1"}) || IsTektonDocument(map[string]any{"apiVersion": "v1"}) || IsTektonDocument(nil) {
		t.Error("IsTektonDocument misclassified")
	}
	got := parsePaCList(`["pull_request", 'push' ,incoming]`)
	if !reflect.DeepEqual(got, []string{"pull_request", "push", "incoming"}) {
		t.Errorf("parsePaCList = %v", got)
	}
	docs, err := SplitDocuments([]byte("---\n---\na: 1\n---\n- x\n---\nkind: List\nitems: [{kind: Task}, 3]\n"))
	if err != nil || len(docs) != 2 {
		t.Errorf("SplitDocuments = %d docs, %v; want 2 mappings", len(docs), err)
	}
}

func TestIngestKonfluxFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "konflux-build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r := Ingest([]File{{Path: ".tekton/hive-pull-request.yaml", Data: data}})
	if len(r.Pipelines) != 1 {
		t.Fatalf("pipelines = %v", pipelineNames(r))
	}
	p := r.Pipelines[0]
	if p.Name != "hive-on-pull-request" || p.Role != workflowmodel.RolePRVerify {
		t.Errorf("name/role = %q %q", p.Name, p.Role)
	}
	if !hasEvent(p.Triggers, "pull_request") || !hasEvent(p.Triggers, "cel") {
		t.Errorf("triggers = %+v", p.Triggers)
	}
	wantLevels := map[string]int{
		"init": 0, "clone-repository": 1, "prefetch-dependencies": 2, "build-container": 3, "build-image-index": 4,
		"clair-scan": 5, "sast-snyk-check": 5, "clamav-scan": 5, "deprecated-base-image-check": 5,
		"show-sbom": 6, "show-summary": 6,
	}
	if len(p.Nodes) != len(wantLevels) {
		t.Errorf("nodes = %d, want %d", len(p.Nodes), len(wantLevels))
	}
	for id, lvl := range wantLevels {
		if got := findNode(t, p, id).Level; got != lvl {
			t.Errorf("%s level = %d, want %d", id, got, lvl)
		}
	}
	// Six task levels plus the finally level.
	if p.Levels != 7 {
		t.Errorf("levels = %d, want 7", p.Levels)
	}
	bc := findNode(t, p, "build-container")
	if bc.Ref != "bundles:buildah-remote-oci-ta" || bc.Matrix == nil || !bc.Matrix.Dynamic || !reflect.DeepEqual(bc.Matrix.Keys, []string{"PLATFORM"}) {
		t.Errorf("build-container = %+v matrix %+v", bc, bc.Matrix)
	}
	if !reflect.DeepEqual(bc.Needs, []string{"init", "prefetch-dependencies"}) {
		t.Errorf("build-container needs = %v (when on init.results plus runAfter)", bc.Needs)
	}
	if !reflect.DeepEqual(findNode(t, p, "clone-repository").When, []string{"$(tasks.init.results.build) in [true]"}) {
		t.Errorf("clone when = %v", findNode(t, p, "clone-repository").When)
	}
	for _, n := range p.Nodes {
		if n.Kind == workflowmodel.NodeFinally && n.Level != p.Levels-1 {
			t.Errorf("finally %s at level %d, want last", n.ID, n.Level)
		}
	}
	for _, code := range []string{CodeFinally} {
		if !hasFinding(r, code) {
			t.Errorf("missing finding %s; have %v", code, findingCodes(r))
		}
	}
	for _, code := range []string{CodeNoTrigger, CodeRunAfterUnresolved, CodeRunAfterCycle, CodeNotTekton} {
		if hasFinding(r, code) {
			t.Errorf("unexpected finding %s", code)
		}
	}
}
