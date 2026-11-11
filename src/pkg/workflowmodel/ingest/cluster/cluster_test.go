package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// fakeKubectl answers kubectl get <plural> from a table; unknown plurals
// fail like a missing CRD. It records every argv for assertions.
type fakeKubectl struct {
	byPlural map[string]any
	errs     map[string]error
	calls    [][]string
}

func (f *fakeKubectl) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	plural := ""
	for i, a := range args {
		if a == "get" && i+1 < len(args) {
			plural = args[i+1]
		}
	}
	if err, ok := f.errs[plural]; ok {
		return nil, err
	}
	items, ok := f.byPlural[plural]
	if !ok {
		return nil, fmt.Errorf("kubectl get %s: exit status 1: error: the server doesn't have a resource type %q", plural, strings.Split(plural, ".")[0])
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}

func obj(kind, ns, name string, extra map[string]any) map[string]any {
	o := map[string]any{"apiVersion": "tekton.dev/v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns, "uid": "u-" + name, "resourceVersion": "9", "managedFields": []any{map[string]any{"manager": "kubectl"}}}}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

func run(ns, name, pipeline, status, when string) map[string]any {
	r := obj("PipelineRun", ns, name, map[string]any{
		"spec":   map[string]any{"pipelineRef": map[string]any{"name": pipeline}},
		"status": map[string]any{"completionTime": when, "conditions": []any{map[string]any{"type": "Succeeded", "status": status, "reason": map[string]string{"True": "Succeeded", "False": "Failed", "Unknown": "Running"}[status]}}},
	})
	r["metadata"].(map[string]any)["annotations"] = map[string]any{"pipelinesascode.tekton.dev/on-event": "[pull_request]", "pipelinesascode.tekton.dev/on-target-branch": "[main]"}
	return r
}

func pipelineSpec() map[string]any {
	return map[string]any{"tasks": []any{
		map[string]any{"name": "clone", "taskRef": map[string]any{"name": "git-clone"}},
		map[string]any{"name": "test", "runAfter": []any{"clone"}, "taskRef": map[string]any{"name": "golang-test"}},
	}}
}

func fixture() *fakeKubectl {
	return &fakeKubectl{byPlural: map[string]any{
		"pipelines.tekton.dev": []any{obj("Pipeline", "ci", "build", map[string]any{"spec": pipelineSpec(), "status": map[string]any{"x": 1}})},
		"tasks.tekton.dev":     []any{obj("Task", "ci", "golang-test", map[string]any{"spec": map[string]any{"steps": []any{}}})},
		"pipelineruns.tekton.dev": []any{
			run("ci", "build-run-1", "build", "True", "2026-01-01T00:00:00Z"),
			run("ci", "build-run-2", "build", "False", "2026-01-03T00:00:00Z"),
			run("ci", "build-run-3", "build", "Unknown", "2026-01-02T00:00:00Z"),
			run("ci", "build-run-4", "build", "True", "2026-01-04T00:00:00Z"),
			run("ci", "orphan-run", "missing", "True", "2026-01-01T00:00:00Z"),
		},
		"eventlisteners.triggers.tekton.dev":   []any{obj("EventListener", "ci", "gh", map[string]any{"apiVersion": "triggers.tekton.dev/v1beta1", "spec": map[string]any{}})},
		"triggertemplates.triggers.tekton.dev": []any{},
		"triggerbindings.triggers.tekton.dev":  []any{},
		"triggers.triggers.tekton.dev":         []any{},
	}}
}

func TestFetchFilesRunsAndCitations(t *testing.T) {
	k := fixture()
	got, err := Fetch(context.Background(), k, Source{Context: "kind-ci", Namespace: "ci", Selector: "team=a", RunsPerPipeline: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Objects != 6 || len(got.Files) != 6 {
		t.Fatalf("objects %d files %d", got.Objects, len(got.Files))
	}
	var paths []string
	for _, f := range got.Files {
		paths = append(paths, f.Path)
		if strings.Contains(string(f.Data), "managedFields") || strings.Contains(string(f.Data), "resourceVersion") {
			t.Errorf("%s keeps server noise:\n%s", f.Path, f.Data)
		}
		if strings.HasPrefix(f.Path, "cluster/ci/pipeline/") && strings.Contains(string(f.Data), "status:") {
			t.Errorf("pipeline keeps status")
		}
	}
	want := []string{"cluster/ci/eventlistener/gh.yaml", "cluster/ci/pipeline/build.yaml", "cluster/ci/pipelinerun/build-run-2.yaml", "cluster/ci/pipelinerun/build-run-4.yaml", "cluster/ci/pipelinerun/orphan-run.yaml", "cluster/ci/task/golang-test.yaml"}
	for _, w := range want {
		if !contains(paths, w) {
			t.Errorf("missing %s in %v", w, paths)
		}
	}
	if contains(paths, "cluster/ci/pipelinerun/build-run-1.yaml") || contains(paths, "cluster/ci/pipelinerun/build-run-3.yaml") {
		t.Errorf("older runs kept: %v", paths)
	}
	if len(got.Runs) != 2 || got.Runs[0].Pipeline != "build" || got.Runs[0].Status != "Succeeded" || got.Runs[0].Run != "build-run-4" || got.Runs[1].Pipeline != "missing" {
		t.Fatalf("runs %+v", got.Runs)
	}
	if len(got.Namespaces) != 1 || got.Namespaces[0] != "ci" {
		t.Fatalf("namespaces %v", got.Namespaces)
	}
	var codes []string
	for _, f := range got.Findings {
		codes = append(codes, f.Code)
	}
	if !contains(codes, CodeRunsTrimmed) {
		t.Fatalf("findings %v", codes)
	}
	first := strings.Join(k.calls[0], " ")
	if first != "--context kind-ci get pipelines.tekton.dev -o json -n ci -l team=a" {
		t.Fatalf("argv %q", first)
	}
}

func TestFetchDefaultsAndStatusReasons(t *testing.T) {
	k := fixture()
	k.byPlural["pipelineruns.tekton.dev"] = []any{
		run("ci", "r1", "build", "False", ""),
		func() map[string]any {
			r := run("ci", "r2", "build", "Unknown", "")
			r["status"] = map[string]any{"startTime": "2026-02-01T00:00:00Z"}
			return r
		}(),
		func() map[string]any {
			r := run("ci", "r3", "", "False", "2026-03-01T00:00:00Z")
			r["spec"] = map[string]any{"pipelineSpec": pipelineSpec()}
			r["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": "False"}}
			return r
		}(),
	}
	got, err := Fetch(context.Background(), k, Source{Namespace: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	byPipe := map[string]RunStatus{}
	for _, r := range got.Runs {
		byPipe[r.Pipeline] = r
	}
	// r2 has only a startTime and no conditions: newest by startTime, status unknown.
	if byPipe["build"].Run != "r2" || byPipe["build"].Status != "Unknown" {
		t.Errorf("startTime ordering / unknown: %+v", byPipe["build"])
	}
	k2 := fixture()
	k2.byPlural["pipelineruns.tekton.dev"] = []any{run("ci", "r", "build", "Unknown", "")}
	got2, _ := Fetch(context.Background(), k2, Source{Namespace: "ci"})
	if got2.Runs[0].Status != "Running" {
		t.Errorf("unknown condition status is running: %+v", got2.Runs)
	}
	if byPipe["r3"].Status != "Failed" {
		t.Errorf("false without reason: %+v", byPipe["r3"])
	}
	// No context flag, namespace flag, no selector.
	if strings.Join(k.calls[0], " ") != "get pipelines.tekton.dev -o json -n ci" {
		t.Fatalf("argv %v", k.calls[0])
	}
}

func TestFetchErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Fetch(ctx, nil, Source{Namespace: "ci"}); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("nil runner: %v", err)
	}
	if _, err := Fetch(ctx, fixture(), Source{}); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("no namespace: %v", err)
	}
	noCRD := &fakeKubectl{byPlural: map[string]any{}}
	if _, err := Fetch(ctx, noCRD, Source{Namespace: "ci"}); !errors.Is(err, ErrNoCRD) {
		t.Fatalf("no crd: %v", err)
	}
	denied := fixture()
	denied.errs = map[string]error{"pipelines.tekton.dev": errors.New(`exit status 1: Error from server (Forbidden): pipelines.tekton.dev is forbidden: User "system:serviceaccount:hive:hive" cannot list resource`)}
	if _, err := Fetch(ctx, denied, Source{Namespace: "ci"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	noCtx := fixture()
	noCtx.errs = map[string]error{"pipelines.tekton.dev": errors.New(`exit status 1: error: context "nope" does not exist`)}
	if _, err := Fetch(ctx, noCtx, Source{Context: "nope", Namespace: "ci"}); !errors.Is(err, ErrNoContext) {
		t.Fatalf("no context: %v", err)
	}
	other := fixture()
	other.errs = map[string]error{"tasks.tekton.dev": errors.New("exit status 1: dial tcp: connection refused")}
	if _, err := Fetch(ctx, other, Source{Namespace: "ci"}); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("other: %v", err)
	}
	// Optional kinds missing their CRD are findings, not failures.
	noTriggers := fixture()
	for _, p := range []string{"eventlisteners.triggers.tekton.dev", "triggertemplates.triggers.tekton.dev", "triggerbindings.triggers.tekton.dev", "triggers.triggers.tekton.dev", "tasks.tekton.dev"} {
		delete(noTriggers.byPlural, p)
	}
	got, err := Fetch(ctx, noTriggers, Source{Namespace: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range got.Findings {
		if f.Code == CodeTriggersCRDMissing {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("triggers findings %d: %+v", n, got.Findings)
	}
	bad := &fakeKubectl{byPlural: map[string]any{}}
	bad.byPlural["pipelines.tekton.dev"] = nil
	garbage := RunnerFunc(func(context.Context, ...string) ([]byte, error) { return []byte("{not json"), nil })
	if _, err := Fetch(ctx, garbage, Source{Namespace: "ci"}); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("garbage: %v", err)
	}
}

func TestFetchCaps(t *testing.T) {
	ctx := context.Background()
	many := &fakeKubectl{byPlural: map[string]any{}}
	var items []any
	for i := 0; i <= MaxObjects; i++ {
		items = append(items, obj("Pipeline", "ci", fmt.Sprintf("p%d", i), map[string]any{"spec": pipelineSpec()}))
	}
	many.byPlural["pipelines.tekton.dev"] = items
	if _, err := Fetch(ctx, many, Source{Namespace: "ci"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("objects cap: %v", err)
	}

	big := &fakeKubectl{byPlural: map[string]any{}}
	big.byPlural["pipelines.tekton.dev"] = []any{obj("Pipeline", "ci", "p", map[string]any{"spec": map[string]any{"description": strings.Repeat("x", MaxTotalBytes+1)}})}
	if _, err := Fetch(ctx, big, Source{Namespace: "ci"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("bytes cap: %v", err)
	}

	wide := &fakeKubectl{byPlural: map[string]any{}}
	var pipes, runs []any
	for i := 0; i < MaxNamespaces+2; i++ {
		ns := fmt.Sprintf("ns%02d", i)
		pipes = append(pipes, obj("Pipeline", ns, "build", map[string]any{"spec": pipelineSpec()}))
		runs = append(runs, run(ns, "r", "build", "True", "2026-01-01T00:00:00Z"))
	}
	wide.byPlural["pipelines.tekton.dev"] = pipes
	wide.byPlural["pipelineruns.tekton.dev"] = runs
	got, err := Fetch(ctx, wide, Source{AllNamespaces: true, RunsPerPipeline: MaxRunsPerPipeline + 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Namespaces) != MaxNamespaces || len(got.Runs) != MaxNamespaces {
		t.Fatalf("namespaces %d runs %d", len(got.Namespaces), len(got.Runs))
	}
	found := false
	for _, f := range got.Findings {
		found = found || f.Code == CodeNamespaceCapped
	}
	if !found {
		t.Fatalf("no namespace-capped finding: %+v", got.Findings)
	}
	if !contains(wide.calls[0], "--all-namespaces") {
		t.Fatalf("argv %v", wide.calls[0])
	}
}

func TestIngestBindsPipelinesAndRecordsObservedRuns(t *testing.T) {
	res, err := Ingest(context.Background(), fixture(), Source{Context: "kind-ci", Namespace: "ci"}, workflowmodel.Scope{Forge: "github.com", Repository: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Model
	if res.Source != "cluster:kind-ci/ci" || m.Metadata.Annotations[AnnotationSource] != res.Source {
		t.Fatalf("source %q ann %v", res.Source, m.Metadata.Annotations)
	}
	if len(m.Spec.Pipelines) != 1 || m.Spec.Pipelines[0].Name != "build" || m.Spec.Pipelines[0].Path != "cluster/ci/pipeline/build.yaml" {
		t.Fatalf("pipelines %+v", m.Spec.Pipelines)
	}
	if m.Spec.Pipelines[0].Role != workflowmodel.RolePRVerify {
		t.Fatalf("role %q: PaC annotations on runs should attach", m.Spec.Pipelines[0].Role)
	}
	if m.Spec.Meta.Source != workflowmodel.SourceTekton || m.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed {
		t.Fatalf("meta %+v", m.Spec.Meta)
	}
	id := m.Spec.Pipelines[0].ID
	if m.Metadata.Annotations[AnnotationObservedRun+id] != "Succeeded" {
		t.Fatalf("observed annotation missing: %v", m.Metadata.Annotations)
	}
	observed := 0
	for _, f := range m.Spec.Coverage.Findings {
		if f.Code == CodeObservedRun {
			observed++
			if f.Node != id || f.Detail != "last run Succeeded" {
				t.Errorf("finding %+v", f)
			}
		}
	}
	if observed != 1 {
		t.Fatalf("observed findings %d (orphan run must not be recorded)", observed)
	}
	// Observations never become inferred lifecycle elements: the CI state
	// cites the cluster file, and the observed status is not a state/event.
	bound := 0
	for _, s := range m.Spec.States {
		if s.Lifecycle.Column == hivedefault.ColumnCIReview {
			if len(s.Pipelines) != 1 || len(s.Lifecycle.Citations) != 1 || s.Lifecycle.Citations[0].Path != "cluster/ci/pipeline/build.yaml" {
				t.Fatalf("ci state %q: %v %v", s.ID, s.Pipelines, s.Lifecycle.Citations)
			}
			bound++
		}
	}
	if bound == 0 {
		t.Fatal("no CI / Review state bound to the cluster pipeline")
	}
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, s := range m.Spec.States {
		for _, c := range s.Lifecycle.Citations {
			if !strings.HasPrefix(c.Path, "cluster/ci/") {
				t.Errorf("citation %+v not a cluster path", c)
			}
		}
	}
	if res.Files != 7 || len(res.Runs) != 2 || len(res.Namespaces) != 1 {
		t.Fatalf("files %d runs %d ns %v", res.Files, len(res.Runs), res.Namespaces)
	}
	if _, err := Ingest(context.Background(), fixture(), Source{}, workflowmodel.Scope{Repository: "x"}); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("ingest error passthrough: %v", err)
	}
}

func TestSourceString(t *testing.T) {
	if s := (Source{}).String(); s != "cluster:current-context/" {
		t.Fatal(s)
	}
	if s := (Source{Context: "c", AllNamespaces: true}).String(); s != "cluster:c/*" {
		t.Fatal(s)
	}
}

func TestFileForNamesClusterScopedAndGenerateName(t *testing.T) {
	f, err := fileFor(map[string]any{"kind": "ClusterTask", "metadata": map[string]any{"generateName": "lint-"}}, "ClusterTask")
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "cluster/_cluster/clustertask/lint.yaml" {
		t.Fatal(f.Path)
	}
}

// TestHelperProcess is the fake kubectl for TestKubectlRun.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if contains(args, "fail") {
		fmt.Fprint(os.Stderr, "Error from server (Forbidden): nope")
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, strings.Join(args, " "))
	os.Exit(0)
}

func TestKubectlRun(t *testing.T) {
	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--", name}, args...)...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
	out, err := Kubectl{Kubeconfig: "/kc"}.Run(context.Background(), "get", "pods")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "kubectl --kubeconfig /kc get pods" {
		t.Fatalf("%q", out)
	}
	out, err = Kubectl{}.Run(context.Background(), "get", "pods")
	if err != nil || string(out) != "kubectl get pods" {
		t.Fatalf("%q %v", out, err)
	}
	_, err = Kubectl{}.Run(context.Background(), "get", "fail")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("stderr must be folded into the error: %v", err)
	}
}

func TestAvailable(t *testing.T) {
	origLook, origStat, origEnv, origHome := lookPath, statFile, getenv, userHomeDir
	t.Cleanup(func() { lookPath, statFile, getenv, userHomeDir = origLook, origStat, origEnv, origHome })
	files := map[string]bool{}
	statFile = func(p string) (os.FileInfo, error) {
		if files[p] {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	env := map[string]string{}
	getenv = func(k string) string { return env[k] }
	userHomeDir = func() (string, error) { return "/home/hive", nil }

	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if ok, why := Available(); ok || !strings.Contains(why, "kubectl") {
		t.Fatalf("%v %q", ok, why)
	}
	lookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	if ok, why := Available(); ok || !strings.Contains(why, "kubeconfig") {
		t.Fatalf("%v %q", ok, why)
	}
	env["KUBECONFIG"] = "/a:/b"
	if ok, _ := Available(); ok {
		t.Fatal("KUBECONFIG pointing at missing files is not available")
	}
	files["/b"] = true
	if ok, _ := Available(); !ok {
		t.Fatal("KUBECONFIG second path")
	}
	delete(env, "KUBECONFIG")
	files["/home/hive/.kube/config"] = true
	if ok, _ := Available(); !ok {
		t.Fatal("~/.kube/config")
	}
	delete(files, "/home/hive/.kube/config")
	userHomeDir = func() (string, error) { return "", errors.New("no home") }
	files[inClusterTokenPath] = true
	if ok, _ := Available(); !ok {
		t.Fatal("in-cluster token")
	}
}

func TestContexts(t *testing.T) {
	if _, err := Contexts(context.Background(), nil); !errors.Is(err, ErrNoRunner) {
		t.Fatal(err)
	}
	var lines []string
	for i := 0; i < maxContexts+3; i++ {
		lines = append(lines, fmt.Sprintf("ctx%d", i))
	}
	r := RunnerFunc(func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "config get-contexts -o name" {
			return nil, fmt.Errorf("argv %v", args)
		}
		return []byte("\n" + strings.Join(lines, "\n") + "\n\n"), nil
	})
	got, err := Contexts(context.Background(), r)
	if err != nil || len(got) != maxContexts || got[0] != "ctx0" {
		t.Fatalf("%d %v", len(got), err)
	}
	if _, err := Contexts(context.Background(), RunnerFunc(func(context.Context, ...string) ([]byte, error) { return nil, errors.New("boom") })); err == nil {
		t.Fatal("error passthrough")
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
