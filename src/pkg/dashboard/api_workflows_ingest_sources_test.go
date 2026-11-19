package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest/cluster"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest/spec"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// fakeClusterRunner answers kubectl get <plural> with a one-pipeline Tekton
// namespace and kubectl config get-contexts with two contexts.
type fakeClusterRunner struct {
	err   error
	calls int
}

func (f *fakeClusterRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	argv := strings.Join(args, " ")
	if strings.HasPrefix(argv, "config get-contexts") {
		return []byte("kind-ci\nprod\n"), nil
	}
	items := []any{}
	meta := func(kind, name string) map[string]any {
		return map[string]any{"apiVersion": "tekton.dev/v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": "ci"}}
	}
	switch {
	case strings.Contains(argv, "get pipelines."):
		p := meta("Pipeline", "build")
		p["spec"] = map[string]any{"tasks": []any{map[string]any{"name": "test", "taskRef": map[string]any{"name": "go-test"}}}}
		items = append(items, p)
	case strings.Contains(argv, "get pipelineruns."):
		r := meta("PipelineRun", "build-1")
		r["metadata"].(map[string]any)["annotations"] = map[string]any{"pipelinesascode.tekton.dev/on-event": "[pull_request]"}
		r["spec"] = map[string]any{"pipelineRef": map[string]any{"name": "build"}}
		r["status"] = map[string]any{"completionTime": "2026-01-01T00:00:00Z", "conditions": []any{map[string]any{"type": "Succeeded", "status": "False", "reason": "Failed"}}}
		items = append(items, r)
	case strings.Contains(argv, "triggers.tekton.dev"):
		return nil, errors.New(`error: the server doesn't have a resource type "eventlisteners"`)
	}
	return json.Marshal(map[string]any{"items": items})
}

type fakeTurnRunner struct {
	answers []string
	err     error
	calls   int
	system  string
	user    string
}

func (f *fakeTurnRunner) RunDeciderTurn(_ context.Context, _, system, user string) (string, error) {
	f.calls++
	f.system, f.user = system, user
	if f.err != nil {
		return "", f.err
	}
	return f.answers[(f.calls-1)%len(f.answers)], nil
}

func configureSpecLLM(s *Server, ready bool) {
	g := &s.deps.Config.Governor
	if ready {
		g.Trajectory.Endpoint = "https://reviewer.local"
		g.Trajectory.Model = "reviewer/model-1"
		return
	}
	g.Trajectory.Endpoint, g.Trajectory.Model = "", ""
	g.LiteLLM.Endpoint, g.LiteLLM.DefaultModel = "", ""
}

const specSample = "# Release process\n\n1. Request: the author opens an issue.\n2. Review by a maintainer. If rejected it goes back to request.\n3. Approve\n4. Merge\n"

func wfDecode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
}

func TestWorkflowIngestCapabilities(t *testing.T) {
	s := workflowTestServer(t, false)
	configureSpecLLM(s, false)
	s.deps.WorkflowClusterRunner = nil
	if rec := wfReq(s, http.MethodGet, "/api/workflows/ingest/capabilities", nil, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner %d", rec.Code)
	}
	rec := wfReq(s, http.MethodGet, "/api/workflows/ingest/capabilities", nil, true)
	var caps struct {
		Cluster       bool     `json:"cluster"`
		ClusterReason string   `json:"clusterReason"`
		Contexts      []string `json:"contexts"`
		Spec          struct {
			LLM   bool   `json:"llm"`
			Model string `json:"model"`
		} `json:"spec"`
	}
	wfDecode(t, rec, &caps)
	if caps.Cluster || caps.Contexts == nil || len(caps.Contexts) != 0 || caps.Spec.LLM || caps.Spec.Model != "" {
		t.Fatalf("%+v", caps)
	}

	s.deps.WorkflowClusterRunner = &fakeClusterRunner{}
	s.deps.WorkflowSpecLLM = &fakeTurnRunner{}
	configureSpecLLM(s, true)
	rec = wfReq(s, http.MethodGet, "/api/workflows/ingest/capabilities", nil, true)
	wfDecode(t, rec, &caps)
	if !caps.Cluster || strings.Join(caps.Contexts, ",") != "kind-ci,prod" || !caps.Spec.LLM || caps.Spec.Model != "reviewer/model-1" {
		t.Fatalf("%+v", caps)
	}
	// A runner that cannot list contexts still reports cluster=true with a reason.
	s.deps.WorkflowClusterRunner = &fakeClusterRunner{err: errors.New("kubeconfig unreadable")}
	rec = wfReq(s, http.MethodGet, "/api/workflows/ingest/capabilities", nil, true)
	wfDecode(t, rec, &caps)
	if !caps.Cluster || !strings.Contains(caps.ClusterReason, "kubeconfig unreadable") || len(caps.Contexts) != 0 {
		t.Fatalf("%+v", caps)
	}
	// A gateway runner wired but unconfigured is not advertised.
	configureSpecLLM(s, false)
	rec = wfReq(s, http.MethodGet, "/api/workflows/ingest/capabilities", nil, true)
	wfDecode(t, rec, &caps)
	if caps.Spec.LLM {
		t.Fatalf("%+v", caps)
	}
}

func TestWorkflowIngestClusterPreviewAndSave(t *testing.T) {
	s := workflowTestServer(t, true)
	runner := &fakeClusterRunner{}
	s.deps.WorkflowClusterRunner = runner
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/cluster", map[string]any{"namespace": "ci"}, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/cluster", "{bad", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body %d", rec.Code)
	}
	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/cluster", map[string]any{"context": "kind-ci", "namespace": "ci", "selector": "team=a"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Compiled   bool                       `json:"compiled"`
		Pipelines  int                        `json:"pipelines"`
		Files      int                        `json:"files"`
		Source     string                     `json:"source"`
		Ref        string                     `json:"ref"`
		Runs       []cluster.RunStatus        `json:"runs"`
		Namespaces []string                   `json:"namespaces"`
		Model      workflowmodel.Model        `json:"model"`
		Diags      []workflowmodel.Diagnostic `json:"diagnostics"`
		Profile    *workflowmodel.Profile     `json:"profile"`
		Entry      *wfstore.Entry             `json:"entry"`
	}
	wfDecode(t, rec, &resp)
	if !resp.Compiled || resp.Pipelines != 1 || resp.Files != 2 || resp.Source != "cluster:kind-ci/ci" || resp.Ref != resp.Source || resp.Profile == nil || resp.Entry != nil {
		t.Fatalf("%+v", resp)
	}
	if len(resp.Runs) != 1 || resp.Runs[0].Status != "Failed" || strings.Join(resp.Namespaces, ",") != "ci" {
		t.Fatalf("runs %+v ns %v", resp.Runs, resp.Namespaces)
	}
	if resp.Model.Spec.Scope.Repository != "cluster/cluster-kind-ci-ci" || resp.Model.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed {
		t.Fatalf("scope %+v meta %+v", resp.Model.Spec.Scope, resp.Model.Spec.Meta)
	}
	if resp.Model.Metadata.Annotations[cluster.AnnotationObservedRun+resp.Model.Spec.Pipelines[0].ID] != "Failed" {
		t.Fatalf("observed run annotation %v", resp.Model.Metadata.Annotations)
	}

	before := time.Now().Add(-time.Minute)
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/cluster", map[string]any{"namespace": "ci", "repository": "acme/widgets", "name": "ci-draft", "save": true}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wfDecode(t, rec, &resp)
	if resp.Entry == nil || resp.Entry.Key() != "ci-draft@1" || resp.Model.Spec.Scope.Repository != "acme/widgets" || resp.Source != "cluster:current-context/ci" {
		t.Fatalf("%+v", resp)
	}
	audits := s.audit.RecentWithPrefixSince(before, "workflow_ingest")
	if len(audits) != 1 || !strings.Contains(audits[0].Detail, "ci-draft@1 from cluster:current-context/ci files=2 compiled=true") {
		t.Fatalf("audit %+v", audits)
	}
	// Saving needs the store.
	s2 := workflowTestServer(t, false)
	s2.deps.WorkflowClusterRunner = runner
	if rec := wfReq(s2, http.MethodPost, "/api/workflows/ingest/cluster", map[string]any{"namespace": "ci", "save": true}, true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store %d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowIngestClusterErrors(t *testing.T) {
	s := workflowTestServer(t, false)
	s.deps.WorkflowClusterRunner = nil
	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/cluster", map[string]any{"namespace": "ci"}, true)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not available on this spoke") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	cases := []struct {
		name string
		body map[string]any
		err  error
		code int
	}{
		{"namespace required", map[string]any{}, nil, http.StatusBadRequest},
		{"forbidden", map[string]any{"namespace": "ci"}, errors.New("Error from server (Forbidden): pipelines.tekton.dev is forbidden"), http.StatusForbidden},
		{"no context", map[string]any{"namespace": "ci", "context": "nope"}, errors.New(`error: context "nope" does not exist`), http.StatusNotFound},
		{"no crd", map[string]any{"namespace": "ci"}, errors.New(`error: the server doesn't have a resource type "pipelines"`), http.StatusUnprocessableEntity},
		{"other", map[string]any{"namespace": "ci"}, errors.New("dial tcp: connection refused"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.deps.WorkflowClusterRunner = &fakeClusterRunner{err: tc.err}
			rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/cluster", tc.body, true)
			if rec.Code != tc.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if clusterErrorStatus(cluster.ErrTooLarge) != http.StatusRequestEntityTooLarge || clusterErrorStatus(cluster.ErrNoRunner) != http.StatusUnprocessableEntity {
		t.Fatal("status mapping")
	}
}

type specResponse struct {
	Compiled bool                       `json:"compiled"`
	Source   string                     `json:"source"`
	LLM      spec.LLMReport             `json:"llm"`
	Sections []spec.Section             `json:"sections"`
	Model    workflowmodel.Model        `json:"model"`
	Diags    []workflowmodel.Diagnostic `json:"diagnostics"`
	Profile  *workflowmodel.Profile     `json:"profile"`
	Entry    *wfstore.Entry             `json:"entry"`
}

func TestWorkflowIngestSpecTextDeterministic(t *testing.T) {
	s := workflowTestServer(t, true)
	configureSpecLLM(s, false)
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample}, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", "{bad", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body %d", rec.Code)
	}
	for _, body := range []map[string]any{{}, {"text": "x", "url": "https://a/b"}, {"text": "x", "repo": "a/b", "path": "p"}} {
		if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", body, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v -> %d", body, rec.Code)
		}
	}
	// No gateway: default is deterministic; asking for the LLM is 422.
	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample, "useLLM": true}, true)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no model gateway") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp specResponse
	wfDecode(t, rec, &resp)
	if !resp.Compiled || resp.LLM.Used || resp.Source != "spec:pasted" || len(resp.Sections) != 1 || resp.Profile == nil || resp.Entry != nil {
		t.Fatalf("%+v", resp)
	}
	if resp.Model.Spec.Scope.Repository != "spec/pasted" || resp.Model.Metadata.Name != "release-process" || resp.Model.Spec.Meta.Source != workflowmodel.SourceSpec {
		t.Fatalf("%+v %+v", resp.Model.Metadata, resp.Model.Spec.Meta)
	}
	for _, st := range resp.Model.Spec.States {
		if st.Lifecycle.Confidence != workflowmodel.ConfidenceInferred || !strings.HasPrefix(st.Lifecycle.Citations[0].Path, "spec:Release process#L") {
			t.Fatalf("state %+v", st)
		}
	}
	if _, ok := resp.Model.Metadata.Annotations[spec.AnnotationLLMAssisted]; ok {
		t.Fatal("deterministic result must not claim LLM assistance")
	}
	// Save + audit.
	before := time.Now().Add(-time.Minute)
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample, "name": "proc", "repository": "acme/widgets", "save": true}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wfDecode(t, rec, &resp)
	if resp.Entry == nil || resp.Entry.Key() != "proc@1" || resp.Model.Spec.Scope.Repository != "acme/widgets" {
		t.Fatalf("%+v", resp.Entry)
	}
	audits := s.audit.RecentWithPrefixSince(before, "workflow_ingest")
	if len(audits) != 1 || !strings.Contains(audits[0].Detail, "proc@1 from spec:pasted llm=false attempts=0") {
		t.Fatalf("audit %+v", audits)
	}
	// Size caps.
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": strings.Repeat("x", workflowSpecMaxTextChars+1)}, true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", rec.Code)
	}
}

func TestWorkflowIngestSpecWithLLM(t *testing.T) {
	s := workflowTestServer(t, false)
	configureSpecLLM(s, true)
	// The gateway answers with the deterministic skeleton relabelled, which
	// compiles; the handler must mark it inferred and name the model.
	secs := spec.Split(specSample)
	good := spec.Extract("pasted", "", workflowmodel.Scope{Forge: "github.com", Repository: "spec/pasted"}, secs)
	good.Spec.States[0].Label = "Request a release"
	raw, _ := json.Marshal(good)
	runner := &fakeTurnRunner{answers: []string{"```json\n" + string(raw) + "\n```"}}
	s.deps.WorkflowSpecLLM = runner

	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp specResponse
	wfDecode(t, rec, &resp)
	if !resp.LLM.Used || !resp.LLM.Accepted || resp.LLM.Attempts != 1 || resp.LLM.Model != "reviewer/model-1" || !resp.Compiled {
		t.Fatalf("%+v", resp.LLM)
	}
	if resp.Model.Spec.States[0].Label != "Request a release" || resp.Model.Metadata.Annotations[spec.AnnotationLLMAssisted] != "LLM-assisted (model reviewer/model-1)" {
		t.Fatalf("%+v %v", resp.Model.Spec.States[0], resp.Model.Metadata.Annotations)
	}
	if runner.system != spec.SystemPrompt || !strings.Contains(runner.user, "# Specification (pasted)") {
		t.Fatal("gateway must receive the package prompt")
	}
	// useLLM=false opts out even when configured.
	runner.calls = 0
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample, "useLLM": false}, true)
	wfDecode(t, rec, &resp)
	if rec.Code != http.StatusOK || resp.LLM.Used || runner.calls != 0 {
		t.Fatalf("%d %+v calls=%d", rec.Code, resp.LLM, runner.calls)
	}
	// A gateway failure falls back to the skeleton and is reported, not a 5xx.
	s.deps.WorkflowSpecLLM = &fakeTurnRunner{err: errors.New("502 from gateway")}
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"text": specSample}, true)
	wfDecode(t, rec, &resp)
	if rec.Code != http.StatusOK || !resp.LLM.Used || resp.LLM.Accepted || !strings.Contains(resp.LLM.Error, "502 from gateway") || !resp.Compiled {
		t.Fatalf("%d %+v", rec.Code, resp.LLM)
	}
	found := false
	for _, f := range resp.Model.Spec.Coverage.Findings {
		found = found || f.Code == spec.CodeLLMFellBack
	}
	if !found {
		t.Fatalf("findings %+v", resp.Model.Spec.Coverage.Findings)
	}
}

func TestWorkflowIngestSpecFromRepoAndURL(t *testing.T) {
	s := workflowTestServer(t, false)
	configureSpecLLM(s, false)
	// repo+path validation and missing forge client.
	s.deps.WorkflowRepoFile = nil
	for body, code := range map[string]int{
		`{"repo":"acme/widgets"}`:                    http.StatusBadRequest,
		`{"path":"docs/p.md"}`:                       http.StatusBadRequest,
		`{"repo":"nope","path":"docs/p.md"}`:         http.StatusBadRequest,
		`{"repo":"acme/widgets","path":"docs/p.md"}`: http.StatusServiceUnavailable,
	} {
		if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", body, true); rec.Code != code {
			t.Fatalf("%s -> %d %s", body, rec.Code, rec.Body)
		}
	}
	var asked string
	s.deps.WorkflowRepoFile = func(_ context.Context, repo, path string) (string, error) {
		asked = repo + ":" + path
		switch path {
		case "missing.md":
			return "", errors.New("404")
		case "huge.md":
			return strings.Repeat("x", workflowSpecMaxTextChars+1), nil
		}
		return specSample, nil
	}
	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"repo": "acme/widgets", "path": "/docs/process.md"}, true)
	if rec.Code != http.StatusOK || asked != "acme/widgets:/docs/process.md" {
		t.Fatalf("%d %s asked=%q", rec.Code, rec.Body, asked)
	}
	var resp specResponse
	wfDecode(t, rec, &resp)
	if resp.Source != "spec:acme/widgets:docs/process.md" || resp.Model.Spec.Scope.Repository != "acme/widgets" {
		t.Fatalf("%+v", resp)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"repo": "acme/widgets", "path": "missing.md"}, true); rec.Code != http.StatusBadGateway {
		t.Fatalf("%d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"repo": "acme/widgets", "path": "huge.md"}, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", rec.Code)
	}

	// URL: https only, no private hosts, size cap, upstream status.
	for _, u := range []string{"http://example.com/spec.md", "ftp://x/y", "https://", "not a url", "https://127.0.0.1/spec.md", "https://localhost/spec.md"} {
		if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"url": u}, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d %s", u, rec.Code, rec.Body)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/spec.md":
			fmt.Fprint(w, specSample)
		case "/huge.md":
			fmt.Fprint(w, strings.Repeat("x", workflowSpecMaxTextChars+1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	orig := workflowSpecHTTP
	origResolver := privateURLResolver
	t.Cleanup(func() { workflowSpecHTTP, privateURLResolver = orig, origResolver })
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		if host == "specs.example.com" {
			return []string{"93.184.216.34"}, nil
		}
		return nil, errors.New("no such host")
	}
	workflowSpecHTTP = srv.Client()
	// The handler insists on https and a public host; route the public name
	// to the test server by rewriting in the transport.
	workflowSpecHTTP.Transport = rewriteTransport{base: srv.Client().Transport, to: srv.URL}

	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"url": "https://specs.example.com/spec.md"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wfDecode(t, rec, &resp)
	if resp.Source != "spec:https://specs.example.com/spec.md" || resp.Model.Spec.Scope.Repository != "spec/specs-example-com-spec-md" {
		t.Fatalf("%+v", resp)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"url": "https://specs.example.com/huge.md"}, true); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "exceeds") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest/spec", map[string]any{"url": "https://specs.example.com/nope.md"}, true); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "status 404") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// rewriteTransport sends every request to a fixed test server URL.
type rewriteTransport struct {
	base http.RoundTripper
	to   string
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(rt.to, "http://")
	return rt.base.RoundTrip(clone)
}
