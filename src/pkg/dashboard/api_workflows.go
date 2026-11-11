package dashboard

import (
	"errors"
	"fmt"
	"net/http"
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
	s.mux.HandleFunc("GET /api/workflows/bindings", s.handleWorkflowBindingsGet)
	s.mux.HandleFunc("PUT /api/workflows/bindings", s.handleWorkflowBindingPut)
	s.mux.HandleFunc("GET /api/workflows/{name}/{version}", s.handleWorkflowGet)
	s.mux.HandleFunc("PUT /api/workflows/{name}/{version}", s.handleWorkflowSave)
	s.mux.HandleFunc("DELETE /api/workflows/{name}/{version}", s.handleWorkflowDelete)
}

func (s *Server) workflowStore(w http.ResponseWriter) *wfstore.Store {
	if s.deps == nil || s.deps.WorkflowStore == nil {
		jsonError(w, "workflow store not enabled", http.StatusServiceUnavailable)
		return nil
	}
	return s.deps.WorkflowStore
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
