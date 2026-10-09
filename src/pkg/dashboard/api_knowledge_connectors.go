package dashboard

import (
	"context"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// KnowledgePublishRuntime is the running publish mirror (#11076) as seen by
// the dashboard. It is nil when publishing is not configured or failed to
// start, in which case "Publish now" answers 503.
type KnowledgePublishRuntime interface {
	Status() connector.PublishStatus
	Trigger()
}

// Knowledge connectors (#11069): read-only status of every configured
// `knowledge.connectors` entry and the publish mirror, plus owner-only
// "sync now" and "publish now" triggers.
func (s *Server) registerKnowledgeConnectorRoutes() {
	s.mux.HandleFunc("GET /api/knowledge/connectors", s.handleKnowledgeConnectorsList)
	s.mux.HandleFunc("POST /api/knowledge/connectors/{name}/sync", s.handleKnowledgeConnectorSync)
	s.mux.HandleFunc("POST /api/knowledge/publish/sync", s.handleKnowledgePublishSync)
}

func (s *Server) knowledgePublish() KnowledgePublishRuntime {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.KnowledgePublish
}

// knowledgePublishJSON is the publish mirror block of the connectors
// responses. Page counts come from the last batch report.
type knowledgePublishJSON struct {
	Connector       string   `json:"connector"`
	Layers          []string `json:"layers"`
	Root            string   `json:"root"`
	DryRun          bool     `json:"dry_run"`
	Active          bool     `json:"active"`
	Running         bool     `json:"running"`
	LastRun         string   `json:"last_run,omitempty"`
	LastSuccess     string   `json:"last_success,omitempty"`
	Pages           int      `json:"pages"`
	PagesCreated    int      `json:"pages_created"`
	PagesUpdated    int      `json:"pages_updated"`
	PagesDeprecated int      `json:"pages_deprecated"`
	PagesUnchanged  int      `json:"pages_unchanged"`
	Collisions      []string `json:"collisions"`
	LastError       string   `json:"last_error,omitempty"`
}

// knowledgePublishView reports the publish mirror, or nil when publishing is
// neither configured nor running. Active is false when knowledge.publish is
// set but the mirror is not running (bad connector, or not restarted yet).
func (s *Server) knowledgePublishView() *knowledgePublishJSON {
	rt := s.knowledgePublish()
	v := &knowledgePublishJSON{Layers: []string{}, Collisions: []string{}}
	configured := false
	if s != nil && s.deps != nil && s.deps.Config != nil && s.deps.Config.Knowledge.Publish.IsConfigured() {
		pub := s.deps.Config.Knowledge.Publish
		configured = true
		v.Connector, v.Root, v.DryRun = pub.Connector, pub.Root, pub.DryRun
		v.Layers = append(v.Layers, pub.Layers...)
	}
	if rt == nil {
		if !configured {
			return nil
		}
		return v
	}
	st := rt.Status()
	v.Active, v.Running, v.Pages, v.LastError = true, st.Running, st.Pages, st.LastError
	v.Connector, v.Root, v.DryRun = st.Connector, st.Root, st.DryRun
	if !st.LastRun.IsZero() {
		v.LastRun = st.LastRun.UTC().Format(time.RFC3339)
	}
	if !st.LastSuccess.IsZero() {
		v.LastSuccess = st.LastSuccess.UTC().Format(time.RFC3339)
	}
	if r := st.LastReport; r != nil {
		v.PagesCreated, v.PagesUpdated, v.PagesDeprecated, v.PagesUnchanged = len(r.Created), len(r.Updated), len(r.Deprecated), r.Unchanged
		v.Collisions = append(v.Collisions, r.Collisions...)
	}
	return v
}

func (s *Server) knowledgeConnectors() KnowledgeConnectorRuntime {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.KnowledgeConnectors
}

// knowledgeConnectorStatus finds one connector's status by name.
func knowledgeConnectorStatus(rt KnowledgeConnectorRuntime, name string) (connector.Status, bool) {
	for _, st := range rt.Statuses() {
		if st.Name == name {
			return st, true
		}
	}
	return connector.Status{}, false
}

// handleKnowledgeConnectorsList returns {"connectors": [...]} with one status
// per configured connector, in config order. Empty when none are configured.
func (s *Server) handleKnowledgeConnectorsList(w http.ResponseWriter, _ *http.Request) {
	statuses := []connector.Status{}
	if syncer := s.knowledgeConnectors(); syncer != nil {
		statuses = syncer.Statuses()
	}
	jsonResponse(w, map[string]any{"connectors": statuses, "publish": s.knowledgePublishView()})
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
	st, ok := knowledgeConnectorStatus(syncer, name)
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

// handleKnowledgePublishSync asks the publish mirror for an immediate batch
// (owner-only) and answers 202 with its status. 503 when the mirror is not
// running and 409 while a batch is already in progress; the outcome lands in
// the publish status.
func (s *Server) handleKnowledgePublishSync(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	rt := s.knowledgePublish()
	if rt == nil {
		jsonError(w, "publish mirror is not running (configure knowledge.publish and restart the hive)", http.StatusServiceUnavailable)
		return
	}
	if rt.Status().Running {
		jsonError(w, "publish already in progress", http.StatusConflict)
		return
	}
	rt.Trigger()
	s.auditFromRequest(r, "knowledge_publish_sync", auditDetail("connector", rt.Status().Connector), "")
	jsonStatusResponse(w, http.StatusAccepted, s.knowledgePublishView())
}
