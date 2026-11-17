package tekton

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddd"
)

func observeCfg() workflowmodel.TektonObserve {
	return workflowmodel.TektonObserve{InCluster: true, Namespace: "ci", LabelSelector: "tekton.dev/pipeline=release"}
}

// run builds a decoded PipelineRun. ann and params may be nil.
func run(name, start, succeeded, reason string, ann map[string]any, params []any) map[string]any {
	md := map[string]any{"name": name, "creationTimestamp": start}
	if ann != nil {
		md["annotations"] = ann
	}
	pr := map[string]any{
		"apiVersion": "tekton.dev/v1",
		"kind":       "PipelineRun",
		"metadata":   md,
		"spec":       map[string]any{},
		"status":     map[string]any{"startTime": start},
	}
	if params != nil {
		pr["spec"] = map[string]any{"params": params}
	}
	if succeeded != "" {
		pr["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": succeeded, "reason": reason}}
	}
	return pr
}

func param(name string, value any) map[string]any {
	return map[string]any{"name": name, "value": value}
}

type fakeLister struct {
	items []map[string]any
	got   workflowmodel.TektonObserve
}

func (f *fakeLister) ListPipelineRuns(_ context.Context, cfg workflowmodel.TektonObserve) ([]map[string]any, error) {
	f.got = cfg
	return f.items, nil
}

func TestRunRevisionSources(t *testing.T) {
	cfg := observeCfg()
	cases := []struct {
		name   string
		pr     map[string]any
		cfg    workflowmodel.TektonObserve
		want   string
		reason string
	}{
		{"default annotation", run("r", "", "", "", map[string]any{"pipelinesascode.tekton.dev/sha": strings.ToUpper(shaA)}, []any{param("revision", shaB)}), cfg, shaA, ""},
		{"falls back to revision param", run("r", "", "", "", nil, []any{param("revision", shaB)}), cfg, shaB, ""},
		{"bad annotation falls back to param", run("r", "", "", "", map[string]any{"pipelinesascode.tekton.dev/sha": "main"}, []any{param("revision", shaB)}), cfg, shaB, ""},
		{"named annotation", run("r", "", "", "", map[string]any{"example.com/commit": shaC, "pipelinesascode.tekton.dev/sha": shaA}, nil),
			workflowmodel.TektonObserve{InCluster: true, Namespace: "ci", LabelSelector: "x", RevisionAnnotation: "example.com/commit"}, shaC, ""},
		{"named param", run("r", "", "", "", nil, []any{param("revision", shaB), param("git-sha", shaD)}),
			workflowmodel.TektonObserve{InCluster: true, Namespace: "ci", LabelSelector: "x", RevisionParam: "git-sha"}, shaD, ""},
		{"nothing", run("r", "", "", "", nil, nil), cfg, "", "no annotation pipelinesascode.tekton.dev/sha; no param revision"},
		{"short sha", run("r", "", "", "", nil, []any{param("revision", "abc1234")}), cfg, "", `param revision="abc1234" is not a full commit SHA`},
		{"unresolved reference", run("r", "", "", "", nil, []any{param("revision", "$(body.head_commit.id)")}), cfg, "", "is not a full commit SHA"},
		{"array param", run("r", "", "", "", nil, []any{param("revision", []any{shaA})}), cfg, "", "param revision is not a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := RunRevision(c.pr, c.cfg)
			if got != c.want {
				t.Fatalf("revision = %q, want %q (reason %q)", got, c.want, reason)
			}
			if c.reason == "" && reason != "" {
				t.Fatalf("unexpected reason %q", reason)
			}
			if c.reason != "" && !strings.Contains(reason, c.reason) {
				t.Fatalf("reason = %q, want it to contain %q", reason, c.reason)
			}
		})
	}
}

func TestRunConclusion(t *testing.T) {
	cases := map[string]struct {
		status, reason string
		want           workflowmodel.RunConclusion
	}{
		"success":   {"True", "Succeeded", workflowmodel.RunSuccess},
		"failure":   {"False", "Failed", workflowmodel.RunFailure},
		"timeout":   {"False", "PipelineRunTimeout", workflowmodel.RunFailure},
		"cancelled": {"False", "Cancelled", "cancelled"},
		"running":   {"Unknown", "Running", "running"},
		"queued":    {"", "", "queued"},
	}
	for name, c := range cases {
		if got := RunConclusion(run("r", "", c.status, c.reason, nil, nil)); got != c.want {
			t.Errorf("%s: conclusion = %q, want %q", name, got, c.want)
		}
	}
}

// TestObserveUndeterminableRevisionStaysUnknown is the maintainer's
// required case: a PipelineRun whose revision cannot be read must leave
// the PRs it might cover unknown, with the reason recorded, and must not
// be guessed into success.
func TestObserveUndeterminableRevisionStaysUnknown(t *testing.T) {
	l := &fakeLister{items: []map[string]any{
		// Listed out of order on purpose: Observe sorts by start time.
		run("release-2", "2026-11-02T00:00:00Z", "True", "Succeeded", nil, []any{param("revision", "main")}),
		run("release-1", "2026-11-01T00:00:00Z", "True", "Succeeded", map[string]any{"pipelinesascode.tekton.dev/sha": shaB}, nil),
	}}
	cfg := observeCfg()
	runs, err := Observe(context.Background(), l, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if l.got.Namespace != "ci" || l.got.LabelSelector != "tekton.dev/pipeline=release" {
		t.Fatalf("lister got %+v", l.got)
	}
	if len(runs) != 2 || runs[0].ID != "release-1" || runs[1].ID != "release-2" {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[1].Revision != "" || !strings.Contains(runs[1].Reason, `param revision="main" is not a full commit SHA`) {
		t.Fatalf("undeterminable run = %+v", runs[1])
	}

	history := []string{shaA, shaB, shaC, shaD}
	prs := []workflowmodel.MergedPR{{Number: 1, MergeCommit: shaB}, {Number: 2, MergeCommit: shaC}}
	got := Attribute("release", runs, history, shaA, prs)

	if a := got[1]["release"]; a.Status != workflowmodel.AttributionSuccess || a.RunID != "release-1" {
		t.Fatalf("PR 1 = %+v, want success by release-1", a)
	}
	a := got[2]["release"]
	if a.Status != workflowmodel.AttributionUnknown {
		t.Fatalf("PR 2 = %+v, want unknown", a)
	}
	if !strings.Contains(a.Reason, "run release-2: revision could not be determined") {
		t.Fatalf("PR 2 reason = %q", a.Reason)
	}
}

func TestObserveFailureThenSuccess(t *testing.T) {
	runs := ObserveRuns([]map[string]any{
		run("deploy-1", "2026-11-01T00:00:00Z", "False", "Failed", map[string]any{"pipelinesascode.tekton.dev/sha": shaC}, nil),
		run("deploy-x", "2026-11-01T12:00:00Z", "False", "Cancelled", map[string]any{"pipelinesascode.tekton.dev/sha": shaD}, nil),
		run("deploy-2", "2026-11-02T00:00:00Z", "True", "Succeeded", nil, []any{param("revision", shaD)}),
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "noise"}},
	}, observeCfg())
	if len(runs) != 3 {
		t.Fatalf("runs = %+v", runs)
	}
	prs := []workflowmodel.MergedPR{{Number: 7, MergeCommit: shaB}}
	history := []string{shaA, shaB, shaC, shaD}
	if a := Attribute("deploy", runs[:2], history, shaA, prs)[7]["deploy"]; a.Status != workflowmodel.AttributionFailed || a.RunID != "deploy-1" {
		t.Fatalf("after failure = %+v", a)
	}
	if a := Attribute("deploy", runs, history, shaA, prs)[7]["deploy"]; a.Status != workflowmodel.AttributionSuccess || a.RunID != "deploy-2" {
		t.Fatalf("after success = %+v", a)
	}
}

func TestObserveRejectsImplicitConfig(t *testing.T) {
	for name, cfg := range map[string]workflowmodel.TektonObserve{
		"no cluster":  {Namespace: "ci", LabelSelector: "x"},
		"both":        {Context: "prod", InCluster: true, Namespace: "ci", LabelSelector: "x"},
		"no ns":       {InCluster: true, LabelSelector: "x"},
		"no selector": {Context: "prod", Namespace: "ci"},
	} {
		if _, err := Observe(context.Background(), &fakeLister{}, cfg); err == nil {
			t.Errorf("%s: Observe accepted %+v", name, cfg)
		}
	}
	if err := (workflowmodel.TektonObserve{Context: "prod", Namespace: "ci", LabelSelector: "x"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestModelValidatesTektonRuns(t *testing.T) {
	m := workflowmodel.HiveDefault(workflowmodel.Scope{Forge: "github.com", Repository: "example/repo"})
	m.Spec.Pipelines = append(m.Spec.Pipelines, workflowmodel.Pipeline{ID: "release", System: workflowmodel.SystemTekton,
		TektonRuns: &workflowmodel.TektonObserve{Namespace: "ci"}})
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "labelSelector is required") {
		t.Fatalf("Validate = %v", err)
	}
	m.Spec.Pipelines[len(m.Spec.Pipelines)-1].TektonRuns = &workflowmodel.TektonObserve{InCluster: true, Namespace: "ci", LabelSelector: "app=release"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	// Existing models without the block are unchanged on the wire.
	b, _ := json.Marshal(workflowmodel.Pipeline{ID: "p"})
	if strings.Contains(string(b), "tektonRuns") {
		t.Fatalf("empty pipeline marshals tektonRuns: %s", b)
	}
	cfg := workflowmodel.TektonObserve{}
	if cfg.Annotation() != "pipelinesascode.tekton.dev/sha" || cfg.Param() != "revision" || cfg.ColumnFor("release") != "release" {
		t.Fatalf("defaults = %q %q %q", cfg.Annotation(), cfg.Param(), cfg.ColumnFor("release"))
	}
}

func TestKubectlListerArgs(t *testing.T) {
	var gotArgs []string
	k := &KubectlLister{run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte(`{"items":[{"apiVersion":"tekton.dev/v1","kind":"PipelineRun","metadata":{"name":"r1"}}]}`), nil
	}}
	items, err := k.ListPipelineRuns(context.Background(), workflowmodel.TektonObserve{Context: "prod", Namespace: "ci", LabelSelector: "app=release"})
	if err != nil {
		t.Fatal(err)
	}
	want := "kubectl --context prod get pipelineruns.tekton.dev --namespace ci --selector app=release --output json"
	if strings.Join(gotArgs, " ") != want {
		t.Fatalf("args = %v", gotArgs)
	}
	if len(items) != 1 || metadataName(items[0]) != "r1" {
		t.Fatalf("items = %v", items)
	}
	if _, ok := NewLister(workflowmodel.TektonObserve{Context: "prod"}).(*KubectlLister); !ok {
		t.Fatal("context config must use kubectl")
	}
	if _, ok := NewLister(workflowmodel.TektonObserve{InCluster: true}).(*InClusterLister); !ok {
		t.Fatal("inCluster config must use the in-cluster API")
	}
}

func TestInClusterLister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/tekton.dev/v1/namespaces/ci/pipelineruns" || r.URL.Query().Get("labelSelector") != "app=release" {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"r1","annotations":{"pipelinesascode.tekton.dev/sha":"` + shaA + `"}},"status":{"conditions":[{"type":"Succeeded","status":"True"}]}}]}`))
	}))
	defer srv.Close()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &InClusterLister{APIServer: srv.URL, TokenPath: tokenPath, Client: srv.Client()}
	runs, err := Observe(context.Background(), l, workflowmodel.TektonObserve{InCluster: true, Namespace: "ci", LabelSelector: "app=release"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Revision != shaA || runs[0].Conclusion != workflowmodel.RunSuccess {
		t.Fatalf("runs = %+v", runs)
	}
	l.TokenPath = filepath.Join(t.TempDir(), "missing")
	if _, err := l.ListPipelineRuns(context.Background(), observeCfg()); err == nil {
		t.Fatal("missing token must fail")
	}
}

type errLister struct{}

func (errLister) ListPipelineRuns(context.Context, workflowmodel.TektonObserve) ([]map[string]any, error) {
	return nil, errors.New("boom")
}

func TestObserveListerError(t *testing.T) {
	_, err := Observe(context.Background(), errLister{}, observeCfg())
	if err == nil || !strings.Contains(err.Error(), "tekton observe ci/tekton.dev/pipeline=release: boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestKubectlListerFailures(t *testing.T) {
	cfg := workflowmodel.TektonObserve{Context: "prod", Namespace: "ci", LabelSelector: "app=release"}
	if _, err := (&KubectlLister{Kubectl: filepath.Join(t.TempDir(), "no-such-kubectl")}).ListPipelineRuns(context.Background(), cfg); err == nil {
		t.Fatal("missing binary must fail")
	}
	// echo exits 0 but prints its arguments, which are not a JSON list.
	if _, err := (&KubectlLister{Kubectl: "echo"}).ListPipelineRuns(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "decode pipelinerun list") {
		t.Fatalf("non-JSON output err = %v", err)
	}
	k := &KubectlLister{run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("denied") }}
	if _, err := k.ListPipelineRuns(context.Background(), cfg); err == nil {
		t.Fatal("run error must propagate")
	}
}

func TestInClusterListerErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := workflowmodel.TektonObserve{InCluster: true, Namespace: "ci", LabelSelector: "app=release"}

	// Non-200 response.
	l := &InClusterLister{APIServer: srv.URL, TokenPath: tokenPath, Client: srv.Client()}
	if _, err := l.ListPipelineRuns(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("non-200 err = %v", err)
	}

	// CA handling when no client is injected.
	l = &InClusterLister{APIServer: srv.URL, TokenPath: tokenPath, CAPath: filepath.Join(dir, "missing-ca")}
	if _, err := l.ListPipelineRuns(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "serviceaccount CA") {
		t.Fatalf("missing CA err = %v", err)
	}
	badCA := filepath.Join(dir, "bad-ca")
	if err := os.WriteFile(badCA, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.CAPath = badCA
	if _, err := l.ListPipelineRuns(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("bad CA err = %v", err)
	}
	goodCA := filepath.Join(dir, "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(goodCA, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	l.CAPath = goodCA
	// Trusted CA reaches the server, which answers 403: proves the pool was built.
	if _, err := l.ListPipelineRuns(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("good CA err = %v", err)
	}

	// Unreachable server.
	l = &InClusterLister{APIServer: "http://127.0.0.1:1", TokenPath: tokenPath, Client: &http.Client{}}
	if _, err := l.ListPipelineRuns(context.Background(), cfg); err == nil {
		t.Fatal("unreachable server must fail")
	}
}
