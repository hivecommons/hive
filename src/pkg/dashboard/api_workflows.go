package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// Workflow-model library and bindings (hivecommons/hive#17620, RFC phase 2).
//
// Reads are open to any dashboard user; writes are owner-gated and audited.
// The store is optional: without one every endpoint answers 503 so the UI can
// say "workflows not enabled" instead of showing an empty library.
func (s *Server) registerWorkflowRoutes() {
	s.mux.HandleFunc("GET /api/workflows", s.handleWorkflowList)
	s.mux.HandleFunc("POST /api/workflows", s.handleWorkflowSave)
	s.mux.HandleFunc("POST /api/workflows/compile", s.handleWorkflowCompile)
	s.mux.HandleFunc("POST /api/workflows/ingest", s.handleWorkflowIngest)
	s.mux.HandleFunc("GET /api/workflows/bindings", s.handleWorkflowBindingsGet)
	s.mux.HandleFunc("PUT /api/workflows/bindings", s.handleWorkflowBindingPut)
	s.mux.HandleFunc("PUT /api/workflows/enabled", s.handleWorkflowEnabledPut)
	s.mux.HandleFunc("GET /api/workflows/{name}/{version}", s.handleWorkflowGet)
	s.mux.HandleFunc("PUT /api/workflows/{name}/{version}", s.handleWorkflowSave)
	s.mux.HandleFunc("DELETE /api/workflows/{name}/{version}", s.handleWorkflowDelete)
	s.registerWorkflowVersionRoutes()
	s.mux.HandleFunc("POST /api/workflows/{name}/{version}/simulate", s.handleWorkflowSimulate)
	s.registerWorkflowEditorRoutes()
	s.registerWorkflowIngestSourceRoutes()
	s.registerWorkflowMarketplaceRoutes()
}

func (s *Server) workflowStore(w http.ResponseWriter) *wfstore.Store {
	if s.deps == nil || s.deps.WorkflowStore == nil {
		jsonError(w, "workflow store not enabled", http.StatusServiceUnavailable)
		return nil
	}
	return s.deps.WorkflowStore
}

// workflowProfileLookup is the store's Profile method, or nil without a store.
func (s *Server) workflowProfileLookup() workflowProfileLookup {
	if s.deps == nil || s.deps.WorkflowStore == nil {
		return nil
	}
	return s.deps.WorkflowStore.Profile
}

func workflowStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, wfstore.ErrNotFound):
		jsonError(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, wfstore.ErrReadOnly):
		jsonError(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, wfstore.ErrBadName), errors.Is(err, wfstore.ErrBadVersion), errors.Is(err, wfstore.ErrNotCompiled):
		jsonError(w, err.Error(), http.StatusBadRequest)
	default:
		jsonError(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleWorkflowList(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	entries, err := st.List()
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"workflows": entries,
		"bindings":  s.workflowBindings(),
		"enabled":   s.deps.Config != nil && s.deps.Config.Workflow.Enabled,
	})
}

func (s *Server) handleWorkflowGet(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	m, e, err := st.Get(r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	resp := map[string]interface{}{"entry": e, "model": m}
	if p, _, cerr := workflowmodel.Compile(m); cerr == nil {
		resp["profile"] = p
	}
	jsonResponse(w, resp)
}

// handleWorkflowCompile compiles a model without saving it so the editor can
// show diagnostics as the operator types.
func (s *Server) handleWorkflowCompile(w http.ResponseWriter, r *http.Request) {
	var m workflowmodel.Model
	if err := decodeBody(r, &m); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, diags, err := workflowmodel.Compile(&m)
	resp := map[string]interface{}{"compiled": err == nil, "diagnostics": diags}
	if diags == nil {
		resp["diagnostics"] = []workflowmodel.Diagnostic{}
	}
	if p != nil {
		resp["profile"] = p
	}
	jsonResponse(w, resp)
}

func (s *Server) handleWorkflowSave(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	var m workflowmodel.Model
	if err := decodeBody(r, &m); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if name := r.PathValue("name"); name != "" && (name != m.Metadata.Name || r.PathValue("version") != m.Spec.Meta.Version) {
		jsonError(w, fmt.Sprintf("body is %s@%s, path is %s@%s", m.Metadata.Name, m.Spec.Meta.Version, name, r.PathValue("version")), http.StatusBadRequest)
		return
	}
	// The editor posts meta.version "" to mean "next free integer version".
	if r.PathValue("name") == "" && strings.TrimSpace(m.Spec.Meta.Version) == "" && m.Metadata.Name != "" {
		v, err := st.NextVersion(m.Metadata.Name)
		if err != nil {
			workflowStoreError(w, err)
			return
		}
		m.Spec.Meta.Version = v
	}
	e, err := st.Save(&m, requestUser(r), time.Now().UTC())
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	s.auditFromRequest(r, "workflow_save", fmt.Sprintf("%s compiled=%v", e.Key(), e.Compiled), "")
	jsonStatusResponse(w, http.StatusCreated, map[string]interface{}{"entry": e})
}

func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	if s.deps.Config != nil {
		for _, b := range s.deps.Config.Project.WorkflowBindings {
			if b.Name == name && b.Version == version {
				jsonError(w, fmt.Sprintf("%s@%s is bound to %s; unbind first", name, version, b.Repo), http.StatusConflict)
				return
			}
		}
	}
	if err := st.Delete(name, version); err != nil {
		workflowStoreError(w, err)
		return
	}
	s.auditFromRequest(r, "workflow_delete", name+"@"+version, "")
	okResponse(w, nil)
}

func (s *Server) workflowBindings() []config.WorkflowBinding {
	if s.deps == nil || s.deps.Config == nil || s.deps.Config.Project.WorkflowBindings == nil {
		return []config.WorkflowBinding{}
	}
	return s.deps.Config.Project.WorkflowBindings
}

func (s *Server) handleWorkflowBindingsGet(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"bindings": s.workflowBindings(),
		"enabled":  s.deps.Config.Workflow.Enabled,
	})
}

// handleWorkflowBindingPut binds (name+version set) or unbinds (name empty) a
// repository. A bound model must exist and compile so a stale binding can
// never leave the tracker without columns.
func (s *Server) handleWorkflowBindingPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body config.WorkflowBinding
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := "workflow_unbind"
	if body.Name != "" {
		st := s.workflowStore(w)
		if st == nil {
			return
		}
		if _, err := st.Profile(body.Name, body.Version); err != nil {
			workflowStoreError(w, err)
			return
		}
		action = "workflow_bind"
	}
	changed, err := s.deps.Config.SetWorkflowBinding(body.Repo, body.Name, body.Version)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if changed {
		if err := s.saveConfig(); err != nil {
			jsonError(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, action, fmt.Sprintf("%s -> %s@%s", body.Repo, body.Name, body.Version), "")
	}
	jsonResponse(w, map[string]interface{}{"changed": changed, "bindings": s.workflowBindings()})
}

// workflowProvisionTimeout bounds the label provisioning PUT
// /api/workflows/enabled runs across every bound repository when it turns
// derivation on: one label list plus a create per missing label per
// repository, all within the request.
const workflowProvisionTimeout = 60 * time.Second

// handleWorkflowEnabledPut flips workflow.enabled (hivecommons/hive#17620)
// from the dashboard so a hosted hive with no hive.yaml access can turn
// model-based stage derivation on or off. The stage engine
// (cmd/hive stageEngineWorkflowFor) and the tracker read the live config
// on every call, so the change takes effect on the next derivation pass
// with no restart or reload. Turning it on also provisions every bound
// repository's model labels now through Dependencies.WorkflowProvisionLabels;
// a provisioning failure is reported in the response as a warning, never as
// a failure of the flip itself, because the sweep retries it on its next
// pass. Audited as workflow_enabled / workflow_disabled; a no-op flip is
// not audited.
func (s *Server) handleWorkflowEnabledPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Enabled == nil {
		jsonError(w, "body must be {\"enabled\": true|false}", http.StatusBadRequest)
		return
	}
	changed := s.deps.Config.Workflow.Enabled != *body.Enabled
	if changed {
		s.deps.Config.Workflow.Enabled = *body.Enabled
		if err := s.saveConfig(); err != nil {
			s.deps.Config.Workflow.Enabled = !*body.Enabled
			jsonError(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		action := "workflow_disabled"
		if *body.Enabled {
			action = "workflow_enabled"
		}
		s.auditFromRequest(r, action, fmt.Sprintf("%d bound repositories", len(s.workflowBindings())), "")
	}
	resp := map[string]interface{}{
		"enabled":  s.deps.Config.Workflow.Enabled,
		"changed":  changed,
		"bindings": s.workflowBindings(),
	}
	if *body.Enabled {
		if warnings := s.provisionBoundWorkflowLabels(r.Context()); len(warnings) > 0 {
			resp["warnings"] = warnings
		}
	}
	jsonResponse(w, resp)
}

// provisionBoundWorkflowLabels runs WorkflowProvisionLabels for every bound
// repository and returns one message per repository it could not provision.
func (s *Server) provisionBoundWorkflowLabels(ctx context.Context) []string {
	if s.deps.WorkflowProvisionLabels == nil {
		return nil
	}
	bindings := s.workflowBindings()
	if len(bindings) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, workflowProvisionTimeout)
	defer cancel()
	var warnings []string
	for _, b := range bindings {
		if err := s.deps.WorkflowProvisionLabels(ctx, b.Repo); err != nil {
			s.logger.Warn("workflow labels: provisioning on enable failed; the next sweep retries", "repo", b.Repo, "workflow", b.Key(), "err", err)
			warnings = append(warnings, fmt.Sprintf("%s: %s", b.Repo, err.Error()))
		}
	}
	return warnings
}
