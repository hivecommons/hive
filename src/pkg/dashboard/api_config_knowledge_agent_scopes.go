package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// knowledgeAgentScopesMu serializes read-modify-write of knowledge.agent_scopes
// and its reads by agent-identified knowledge requests.
var knowledgeAgentScopesMu sync.Mutex

// knowledgeAgentScope returns the knowledge.agent_scopes scope of the agent a
// request names with ?agent=, the parameter the other agent-scoped endpoints
// use. ok is false for an unidentified caller or an unrestricted agent.
func (s *Server) knowledgeAgentScope(r *http.Request) (knowledge.TOCScope, bool) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" || s.deps == nil || s.deps.Config == nil {
		return knowledge.TOCScope{}, false
	}
	knowledgeAgentScopesMu.Lock()
	sc, ok := s.deps.Config.KnowledgeAgentScope(agent)
	knowledgeAgentScopesMu.Unlock()
	if !ok {
		return knowledge.TOCScope{}, false
	}
	return knowledge.AgentScope(sc.Layers, sc.Repos, sc.Types, sc.Tags, sc.IncludeStates), true
}

// cleanScopeList trims entries and drops blanks; an empty result is nil so
// the field stays unrestricted and is omitted from hive.yaml.
func cleanScopeList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func knowledgeAgentScopesResponse(cfg *config.Config) map[string]interface{} {
	scopes := cfg.Knowledge.AgentScopes
	if scopes == nil {
		scopes = map[string]config.KnowledgeAgentScope{}
	}
	return map[string]interface{}{
		"agent_scopes": scopes,
		"layers":       []string{"personal", "project", "org", "community"},
		"states":       []string{"draft", "approved", "deprecated", "superseded"},
	}
}

// handleKnowledgeAgentScopesGet returns knowledge.agent_scopes.
func (s *Server) handleKnowledgeAgentScopesGet(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	knowledgeAgentScopesMu.Lock()
	defer knowledgeAgentScopesMu.Unlock()
	jsonResponse(w, knowledgeAgentScopesResponse(s.deps.Config))
}

// handleKnowledgeAgentScopesPut replaces knowledge.agent_scopes with the
// posted map (owner-only). The whole map is validated before anything is
// applied, and the change is persisted and audited.
func (s *Server) handleKnowledgeAgentScopesPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		AgentScopes map[string]json.RawMessage `json:"agent_scopes"`
	}
	if err := decodeBody(r, &body); err != nil || body.AgentScopes == nil {
		jsonError(w, "invalid body: expected {\"agent_scopes\": {...}}", http.StatusBadRequest)
		return
	}
	next := make(map[string]config.KnowledgeAgentScope, len(body.AgentScopes))
	for name, raw := range body.AgentScopes {
		var sc config.KnowledgeAgentScope
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sc); err != nil {
			jsonError(w, "agent_scopes."+name+": "+err.Error(), http.StatusBadRequest)
			return
		}
		next[strings.TrimSpace(name)] = config.KnowledgeAgentScope{
			Layers:        cleanScopeList(sc.Layers),
			Repos:         cleanScopeList(sc.Repos),
			Types:         cleanScopeList(sc.Types),
			Tags:          cleanScopeList(sc.Tags),
			IncludeStates: cleanScopeList(sc.IncludeStates),
		}
	}
	if err := config.ValidateKnowledgeAgentScopes(next); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(next) == 0 {
		next = nil
	}

	knowledgeAgentScopesMu.Lock()
	defer knowledgeAgentScopesMu.Unlock()
	s.deps.Config.Knowledge.AgentScopes = next
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after knowledge agent scopes update", "error", err)
	}
	names := make([]string, 0, len(next))
	for name := range next {
		names = append(names, name)
	}
	sort.Strings(names)
	s.auditFromRequest(r, "config_knowledge_agent_scopes", auditDetail("agents", strings.Join(names, ",")), "")
	s.refreshAndPersist()
	jsonResponse(w, knowledgeAgentScopesResponse(s.deps.Config))
}
