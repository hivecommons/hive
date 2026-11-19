package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// workflowIngestTimeout bounds one repository fetch + ingest.
const workflowIngestTimeout = 2 * time.Minute

// workflowIngestRequest is the body of POST /api/workflows/ingest. Exactly
// one of Repo (fetch the default branch through the forge) or Files
// (pasted/uploaded CI definitions, repo-relative path -> contents) is set.
type workflowIngestRequest struct {
	Repo  string            `json:"repo"`
	Files map[string]string `json:"files,omitempty"`
	// Save stores the result as a draft under Name (default: the ingest's
	// slug of the repository) at the next free integer version.
	Save bool   `json:"save,omitempty"`
	Name string `json:"name,omitempty"`
}

// handleWorkflowIngest turns a repository's CI definitions into a lifecycle
// model (RFC #17620 P3): hive-default's stages plus the repository's
// pipelines, provenance ingested-unreviewed. Owner-gated because it reads
// the forge on the hive's credential and may write the store.
func (s *Server) handleWorkflowIngest(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req workflowIngestRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if (req.Repo == "") == (len(req.Files) == 0) {
		jsonError(w, "set exactly one of repo or files", http.StatusBadRequest)
		return
	}
	var files map[string][]byte
	ref := ""
	repo := req.Repo
	switch {
	case len(req.Files) > 0:
		if repo == "" {
			repo = "local/pasted"
		}
		files = make(map[string][]byte, len(req.Files))
		for p, body := range req.Files {
			files[strings.TrimPrefix(strings.TrimSpace(p), "/")] = []byte(body)
		}
	default:
		if !strings.Contains(repo, "/") {
			jsonError(w, "repo must be owner/name", http.StatusBadRequest)
			return
		}
		if s.deps == nil || s.deps.WorkflowRepoFiles == nil {
			jsonError(w, "repository ingest not available: no forge client", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), workflowIngestTimeout)
		defer cancel()
		var err error
		files, ref, err = s.deps.WorkflowRepoFiles(ctx, repo)
		if err != nil {
			jsonError(w, fmt.Sprintf("fetch %s: %v", repo, err), http.StatusBadGateway)
			return
		}
	}

	m := ingest.Lifecycle(workflowmodel.Scope{Forge: "github.com", Repository: repo}, files)
	if req.Name != "" {
		m.Metadata.Name = req.Name
	}
	p, diags, cerr := workflowmodel.Compile(m)
	if diags == nil {
		diags = []workflowmodel.Diagnostic{}
	}
	resp := map[string]interface{}{
		"model": m, "compiled": cerr == nil, "diagnostics": diags,
		"ref": ref, "files": len(files), "pipelines": len(m.Spec.Pipelines),
	}
	if p != nil {
		resp["profile"] = p
	}
	if !req.Save {
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
	s.auditFromRequest(r, "workflow_ingest", fmt.Sprintf("%s from %s@%s files=%d compiled=%v", e.Key(), repo, ref, len(files), e.Compiled), "")
	resp["entry"] = e
	jsonStatusResponse(w, http.StatusCreated, resp)
}

// nextWorkflowVersion is one past the highest integer version stored under
// name ("1" for a new name); non-integer versions are ignored.
func nextWorkflowVersion(st *wfstore.Store, name string) (string, error) {
	entries, err := st.Versions(name)
	if err != nil && !errors.Is(err, wfstore.ErrNotFound) {
		return "", err
	}
	max := 0
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Version); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1), nil
}
