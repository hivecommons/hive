package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// knowledgeConnectorLookup is implemented by the connector syncer so the
// dashboard can reach a connector's optional interfaces (Authenticator).
type knowledgeConnectorLookup interface {
	Connector(name string) (connector.Connector, bool)
}

// knowledgeAuthTimeout bounds one auth status/connect/disconnect request,
// which may spawn the connector's MCP server.
const knowledgeAuthTimeout = 90 * time.Second

// Interactive connector sign-in (Plaud OAuth through its MCP server): status,
// connect (returns the authorization URL) and disconnect. Tokens stay in the
// vendor tool's own store and are never returned.
func (s *Server) registerKnowledgeConnectorAuthRoutes() {
	s.mux.HandleFunc("GET /api/knowledge/connectors/{name}/auth", s.handleKnowledgeConnectorAuthStatus)
	s.mux.HandleFunc("POST /api/knowledge/connectors/{name}/auth/connect", s.handleKnowledgeConnectorAuthConnect)
	s.mux.HandleFunc("POST /api/knowledge/connectors/{name}/auth/disconnect", s.handleKnowledgeConnectorAuthDisconnect)
}

// knowledgeAuthenticator resolves the named connector's Authenticator,
// writing the matching error response when it is unavailable.
func (s *Server) knowledgeAuthenticator(w http.ResponseWriter, name string) (connector.Authenticator, bool) {
	rt := s.knowledgeConnectors()
	if rt == nil {
		jsonError(w, "no knowledge connectors are configured", http.StatusNotFound)
		return nil, false
	}
	lookup, ok := rt.(knowledgeConnectorLookup)
	if !ok {
		jsonError(w, "connector sign-in is not available on this runtime", http.StatusServiceUnavailable)
		return nil, false
	}
	c, ok := lookup.Connector(name)
	if !ok {
		jsonError(w, "unknown knowledge connector", http.StatusNotFound)
		return nil, false
	}
	auth, ok := c.(connector.Authenticator)
	if !ok {
		jsonError(w, "this connector type does not use interactive sign-in", http.StatusBadRequest)
		return nil, false
	}
	return auth, true
}

func knowledgeAuthContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), knowledgeAuthTimeout)
}

// handleKnowledgeConnectorAuthStatus returns the cached sign-in state, or a
// fresh one when ?refresh=1 (which may start the MCP server briefly).
func (s *Server) handleKnowledgeConnectorAuthStatus(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.knowledgeAuthenticator(w, r.PathValue("name"))
	if !ok {
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	ctx, cancel := knowledgeAuthContext(r)
	defer cancel()
	jsonResponse(w, auth.AuthStatus(ctx, refresh))
}

// handleKnowledgeConnectorAuthConnect (owner) starts the sign-in flow and
// answers 202 with the state; auth_url is the page the owner must open.
func (s *Server) handleKnowledgeConnectorAuthConnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := r.PathValue("name")
	auth, ok := s.knowledgeAuthenticator(w, name)
	if !ok {
		return
	}
	ctx, cancel := knowledgeAuthContext(r)
	defer cancel()
	st, err := auth.Connect(ctx)
	if err != nil {
		jsonError(w, "connect failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.auditFromRequest(r, "knowledge_connector_connect", auditDetail("connector", name), "")
	jsonStatusResponse(w, http.StatusAccepted, st)
}

// handleKnowledgeConnectorAuthDisconnect (owner) signs out upstream.
func (s *Server) handleKnowledgeConnectorAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := r.PathValue("name")
	auth, ok := s.knowledgeAuthenticator(w, name)
	if !ok {
		return
	}
	ctx, cancel := knowledgeAuthContext(r)
	defer cancel()
	st, err := auth.Disconnect(ctx)
	if err != nil {
		jsonError(w, "disconnect failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.auditFromRequest(r, "knowledge_connector_disconnect", auditDetail("connector", name), "")
	jsonResponse(w, st)
}
