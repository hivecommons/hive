package dashboard

import (
	"context"
	"net/http"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// Knowledge connectors (#11069): read-only status of every configured
// `knowledge.connectors` entry, plus an owner-only "sync now" trigger.
func (s *Server) registerKnowledgeConnectorRoutes() {
	s.mux.HandleFunc("GET /api/knowledge/connectors", s.handleKnowledgeConnectorsList)
	s.mux.HandleFunc("POST /api/knowledge/connectors/{name}/sync", s.handleKnowledgeConnectorSync)
}

func (s *Server) knowledgeConnectors() *connector.Syncer {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.KnowledgeConnectors
}

// handleKnowledgeConnectorsList returns {"connectors": [...]} with one status
// per configured connector, in config order. Empty when none are configured.
func (s *Server) handleKnowledgeConnectorsList(w http.ResponseWriter, _ *http.Request) {
	statuses := []connector.Status{}
	if syncer := s.knowledgeConnectors(); syncer != nil {
		statuses = syncer.Statuses()
	}
	jsonResponse(w, map[string]any{"connectors": statuses})
}

// handleKnowledgeConnectorSync starts one sync of the named connector in the
// background and answers 202 with its status. 404 for an unknown name and
// 409 while a sync is already running; the outcome lands in the status.
func (s *Server) handleKnowledgeConnectorSync(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	syncer := s.knowledgeConnectors()
	name := r.PathValue("name")
	if syncer == nil {
		jsonError(w, "no knowledge connectors are configured", http.StatusNotFound)
		return
	}
	st, ok := syncer.Status(name)
	if !ok {
		jsonError(w, "unknown knowledge connector", http.StatusNotFound)
		return
	}
	if st.Running {
		jsonError(w, "sync already in progress", http.StatusConflict)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	go func() { _, _ = syncer.SyncNow(ctx, name) }()
	st.Running = true
	jsonStatusResponse(w, http.StatusAccepted, st)
}
