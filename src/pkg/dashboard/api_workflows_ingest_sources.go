package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest/cluster"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest/spec"
)

// RFC hivecommons/hive#17620 phase 6: ingest Tekton from a live cluster
// context and written process specs (LLM-assisted, every element
// inferred). This file owns the three routes; api_workflows.go only calls
// registerWorkflowIngestSourceRoutes.

const (
	// workflowSpecURLTimeout bounds fetching a spec by URL.
	workflowSpecURLTimeout = 20 * time.Second
	// workflowSpecLLMTimeout bounds the whole spec ingest including up to
	// spec.MaxAttempts gateway turns.
	workflowSpecLLMTimeout = 3 * time.Minute
	// workflowSpecLLMProfile is the delegatedecision profile label for the
	// spec-ingest turn (audit/logging only).
	workflowSpecLLMProfile = "workflow-spec-ingest"
	// workflowSpecMaxTextChars caps a pasted or fetched spec, mirroring
	// spec.MaxBytes so the handler refuses before decoding a huge body.
	workflowSpecMaxTextChars = spec.MaxBytes
)

func (s *Server) registerWorkflowIngestSourceRoutes() {
	s.mux.HandleFunc("GET /api/workflows/ingest/capabilities", s.handleWorkflowIngestCapabilities)
	s.mux.HandleFunc("POST /api/workflows/ingest/cluster", s.handleWorkflowIngestCluster)
	s.mux.HandleFunc("POST /api/workflows/ingest/spec", s.handleWorkflowIngestSpec)
}

// workflowSpecLLMConfigured says whether the optional LLM stage can run:
// a runner is wired and the reviewer gateway resolves an endpoint and a
// model. The model id is reported so the UI and the model annotation can
// name it.
func (s *Server) workflowSpecLLMConfigured() (bool, string) {
	if s.deps == nil || s.deps.WorkflowSpecLLM == nil || s.deps.Config == nil {
		return false, ""
	}
	if !s.deps.Config.Governor.ReviewerReady() {
		return false, ""
	}
	_, _, model := s.deps.Config.Governor.ResolveReviewer()
	return true, model
}

func (s *Server) workflowClusterRunner() cluster.Runner {
	if s.deps == nil {
		return nil
	}
	return s.deps.WorkflowClusterRunner
}

// handleWorkflowIngestCapabilities answers GET /api/workflows/ingest/capabilities:
// {cluster, clusterReason, contexts[], spec: {llm, model}}. Owner-gated
// like the ingest routes it describes.
func (s *Server) handleWorkflowIngestCapabilities(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	resp := map[string]interface{}{"cluster": false, "contexts": []string{}}
	if runner := s.workflowClusterRunner(); runner != nil {
		resp["cluster"] = true
		ctx, cancel := context.WithTimeout(r.Context(), cluster.KubectlTimeout)
		defer cancel()
		contexts, err := cluster.Contexts(ctx, runner)
		if err != nil {
			resp["clusterReason"] = "kube contexts unavailable: " + err.Error()
		} else if contexts != nil {
			resp["contexts"] = contexts
		}
	} else {
		_, reason := cluster.Available()
		resp["clusterReason"] = reason
	}
	llm, model := s.workflowSpecLLMConfigured()
	resp["spec"] = map[string]interface{}{"llm": llm, "model": model}
	jsonResponse(w, resp)
}

// workflowIngestClusterRequest is the body of POST /api/workflows/ingest/cluster.
type workflowIngestClusterRequest struct {
	Context       string `json:"context"`
	Namespace     string `json:"namespace"`
	AllNamespaces bool   `json:"allNamespaces,omitempty"`
	Selector      string `json:"selector,omitempty"`
	// RunsPerPipeline is how many recent PipelineRuns per pipeline to keep
	// (default cluster.DefaultRunsPerPipeline, capped).
	RunsPerPipeline int `json:"runsPerPipeline,omitempty"`
	// Repository scopes the model (owner/name); defaults to the context
	// and namespace so the draft is still addressable.
	Repository string `json:"repository,omitempty"`
	Save       bool   `json:"save,omitempty"`
	Name       string `json:"name,omitempty"`
}

// handleWorkflowIngestCluster lists Tekton objects from a kube context and
// ingests them through the file-based Tekton parser. Same response shape as
// POST /api/workflows/ingest plus source, runs and namespaces.
func (s *Server) handleWorkflowIngestCluster(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req workflowIngestClusterRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	runner := s.workflowClusterRunner()
	if runner == nil {
		_, reason := cluster.Available()
		jsonError(w, "cluster ingest not available on this spoke: "+reason, http.StatusUnprocessableEntity)
		return
	}
	src := cluster.Source{
		Context:         strings.TrimSpace(req.Context),
		Namespace:       strings.TrimSpace(req.Namespace),
		AllNamespaces:   req.AllNamespaces,
		Selector:        strings.TrimSpace(req.Selector),
		RunsPerPipeline: req.RunsPerPipeline,
	}
	repo := strings.TrimSpace(req.Repository)
	if repo == "" {
		repo = "cluster/" + workflowmodel.Slug(src.String())
	}
	ctx, cancel := context.WithTimeout(r.Context(), workflowIngestTimeout)
	defer cancel()
	res, err := cluster.Ingest(ctx, runner, src, workflowmodel.Scope{Forge: "github.com", Repository: repo})
	if err != nil {
		jsonError(w, err.Error(), clusterErrorStatus(err))
		return
	}
	m := res.Model
	if req.Name != "" {
		m.Metadata.Name = req.Name
	}
	p, diags, cerr := workflowmodel.Compile(m)
	if diags == nil {
		diags = []workflowmodel.Diagnostic{}
	}
	resp := map[string]interface{}{
		"model": m, "compiled": cerr == nil, "diagnostics": diags,
		"ref": res.Source, "source": res.Source, "files": res.Files, "pipelines": len(m.Spec.Pipelines),
		"runs": res.Runs, "namespaces": res.Namespaces,
	}
	if p != nil {
		resp["profile"] = p
	}
	s.finishWorkflowIngest(w, r, req.Save, m, resp, fmt.Sprintf("from %s files=%d", res.Source, res.Files))
}

// clusterErrorStatus maps cluster ingest errors to HTTP statuses with the
// package's clear messages.
func clusterErrorStatus(err error) int {
	switch {
	case errors.Is(err, cluster.ErrNamespaceRequired):
		return http.StatusBadRequest
	case errors.Is(err, cluster.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, cluster.ErrNoContext):
		return http.StatusNotFound
	case errors.Is(err, cluster.ErrNoCRD), errors.Is(err, cluster.ErrNoRunner):
		return http.StatusUnprocessableEntity
	case errors.Is(err, cluster.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadGateway
}

// workflowIngestSpecRequest is the body of POST /api/workflows/ingest/spec.
// Exactly one of Text, Repo+Path or URL is set.
type workflowIngestSpecRequest struct {
	Text string `json:"text,omitempty"`
	Repo string `json:"repo,omitempty"`
	Path string `json:"path,omitempty"`
	URL  string `json:"url,omitempty"`
	// UseLLM asks for the gateway refinement; nil means "if configured".
	UseLLM *bool `json:"useLLM,omitempty"`
	// Repository scopes the model (owner/name); defaults to Repo or
	// spec/<origin slug>.
	Repository string `json:"repository,omitempty"`
	Save       bool   `json:"save,omitempty"`
	Name       string `json:"name,omitempty"`
}

// handleWorkflowIngestSpec turns a written process spec into a draft model.
// Response: {model, compiled, diagnostics, profile?, source, llm:{used,
// model, attempts, accepted, error?}, sections[], entry?}.
func (s *Server) handleWorkflowIngestSpec(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req workflowIngestSpecRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Repo, req.Path, req.URL = strings.TrimSpace(req.Repo), strings.TrimSpace(req.Path), strings.TrimSpace(req.URL)
	inputs := 0
	for _, set := range []bool{strings.TrimSpace(req.Text) != "", req.Repo != "" || req.Path != "", req.URL != ""} {
		if set {
			inputs++
		}
	}
	if inputs != 1 {
		jsonError(w, "set exactly one of text, repo+path or url", http.StatusBadRequest)
		return
	}
	llmReady, llmModel := s.workflowSpecLLMConfigured()
	useLLM := llmReady
	if req.UseLLM != nil {
		useLLM = *req.UseLLM
	}
	if useLLM && !llmReady {
		jsonError(w, "AI assistance requested but no model gateway is configured (Settings → Model Gateways); retry with useLLM=false for the deterministic draft", http.StatusUnprocessableEntity)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), workflowSpecLLMTimeout)
	defer cancel()
	text, origin, repo, status, err := s.loadWorkflowSpecText(ctx, req)
	if err != nil {
		jsonError(w, err.Error(), status)
		return
	}
	if strings.TrimSpace(req.Repository) != "" {
		repo = strings.TrimSpace(req.Repository)
	}
	opts := spec.Options{Origin: origin, Name: strings.TrimSpace(req.Name), Scope: workflowmodel.Scope{Forge: "github.com", Repository: repo}}
	if useLLM {
		runner := s.deps.WorkflowSpecLLM
		opts.ModelID = llmModel
		opts.Completer = spec.CompleterFunc(func(ctx context.Context, system, user string) (string, error) {
			return runner.RunDeciderTurn(ctx, workflowSpecLLMProfile, system, user)
		})
	}
	res, err := spec.Ingest(ctx, text, opts)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, spec.ErrTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		jsonError(w, err.Error(), status)
		return
	}
	diags := res.Diagnostics
	if diags == nil {
		diags = []workflowmodel.Diagnostic{}
	}
	resp := map[string]interface{}{
		"model": res.Model, "compiled": res.Compiled, "diagnostics": diags,
		"source": res.Source, "llm": res.LLM, "sections": res.Sections,
	}
	if res.Profile != nil {
		resp["profile"] = res.Profile
	}
	s.finishWorkflowIngest(w, r, req.Save, res.Model, resp, fmt.Sprintf("from %s llm=%v attempts=%d", res.Source, res.LLM.Accepted, res.LLM.Attempts))
}

// loadWorkflowSpecText resolves the spec body from the request: pasted
// text, a default-branch file through the forge, or an https URL (public
// hosts only, size-capped). It returns the text, its origin label, the
// repository to scope the model to and, on error, the HTTP status.
func (s *Server) loadWorkflowSpecText(ctx context.Context, req workflowIngestSpecRequest) (text, origin, repo string, status int, err error) {
	switch {
	case strings.TrimSpace(req.Text) != "":
		if len(req.Text) > workflowSpecMaxTextChars {
			return "", "", "", http.StatusRequestEntityTooLarge, spec.ErrTooLarge
		}
		return req.Text, "pasted", "spec/pasted", 0, nil
	case req.URL != "":
		u, perr := url.Parse(req.URL)
		if perr != nil || u.Scheme != "https" || u.Host == "" {
			return "", "", "", http.StatusBadRequest, errors.New("url must be https://host/path")
		}
		if isPrivateURL(ctx, req.URL) {
			return "", "", "", http.StatusBadRequest, errors.New("url resolves to a private or internal host")
		}
		body, ferr := fetchWorkflowSpecURL(ctx, req.URL)
		if ferr != nil {
			return "", "", "", http.StatusBadGateway, fmt.Errorf("fetch %s: %w", req.URL, ferr)
		}
		return body, req.URL, "spec/" + workflowmodel.Slug(u.Host+u.Path), 0, nil
	default:
		if req.Repo == "" || req.Path == "" || !strings.Contains(req.Repo, "/") {
			return "", "", "", http.StatusBadRequest, errors.New("repo (owner/name) and path are both required")
		}
		if s.deps == nil || s.deps.WorkflowRepoFile == nil {
			return "", "", "", http.StatusServiceUnavailable, errors.New("repository spec ingest not available: no forge client")
		}
		body, ferr := s.deps.WorkflowRepoFile(ctx, req.Repo, req.Path)
		if ferr != nil {
			return "", "", "", http.StatusBadGateway, fmt.Errorf("fetch %s:%s: %w", req.Repo, req.Path, ferr)
		}
		if len(body) > workflowSpecMaxTextChars {
			return "", "", "", http.StatusRequestEntityTooLarge, spec.ErrTooLarge
		}
		return body, req.Repo + ":" + strings.TrimPrefix(req.Path, "/"), req.Repo, 0, nil
	}
}

// workflowSpecHTTP is the client for URL specs; a seam for tests.
var workflowSpecHTTP = &http.Client{Timeout: workflowSpecURLTimeout, CheckRedirect: noRedirectToPrivate}

func fetchWorkflowSpecURL(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/markdown, text/plain;q=0.9, */*;q=0.1")
	resp, err := workflowSpecHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(workflowSpecMaxTextChars)+1))
	if err != nil {
		return "", err
	}
	if len(body) > workflowSpecMaxTextChars {
		return "", spec.ErrTooLarge
	}
	return string(body), nil
}

// finishWorkflowIngest either answers the preview or saves the model as
// the next draft version, audits it and answers 201 with the entry; the
// same tail POST /api/workflows/ingest has.
func (s *Server) finishWorkflowIngest(w http.ResponseWriter, r *http.Request, save bool, m *workflowmodel.Model, resp map[string]interface{}, detail string) {
	if !save {
		jsonResponse(w, resp)
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	version, err := nextWorkflowVersion(st, m.Metadata.Name)
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	m.Spec.Meta.Version = version
	e, err := st.Save(m, requestUser(r), time.Now().UTC())
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	s.auditFromRequest(r, "workflow_ingest", fmt.Sprintf("%s %s compiled=%v", e.Key(), detail, e.Compiled), "")
	resp["entry"] = e
	jsonStatusResponse(w, http.StatusCreated, resp)
}
