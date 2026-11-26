package dashboard

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/repoagents"
)

// defaultDiscoveredBackend is the backend an approved repo-discovered agent
// gets when the owner names none, the same default the import uses.
const defaultDiscoveredBackend = "copilot"

// discoveredAgentModes are the modes an owner may pick when approving, the
// same set the agent config editor accepts.
var discoveredAgentModes = map[string]bool{"ADVISORY": true, "ISSUES_ONLY": true, "ISSUES_AND_PRS": true, "ISSUES_PRS_MERGE": true, "NO_GITHUB": true}

// registerRepoAgentRoutes wires the approval queue for agents discovered in
// managed repos' .hive/agents/ (hivecommons/hive#18491).
func (s *Server) registerRepoAgentRoutes() {
	s.mux.HandleFunc("GET /api/agents/discovered", s.handleDiscoveredAgentsList)
	s.mux.HandleFunc("POST /api/agents/discovered/approve", s.handleDiscoveredAgentApprove)
	s.mux.HandleFunc("POST /api/agents/discovered/reject", s.handleDiscoveredAgentReject)
	s.mux.HandleFunc("POST /api/agents/discovered/refresh", s.handleDiscoveredAgentsRefresh)
}

func (s *Server) repoAgents() *repoagents.Discoverer {
	if s.deps == nil {
		return nil
	}
	return s.deps.RepoAgents
}

// handleDiscoveredAgentsList serves GET /api/agents/discovered.
func (s *Server) handleDiscoveredAgentsList(w http.ResponseWriter, _ *http.Request) {
	d := s.repoAgents()
	if d == nil {
		jsonResponse(w, map[string]any{"enabled": false, "entries": []repoagents.Entry{}})
		return
	}
	entries := d.List()
	pending := 0
	for _, e := range entries {
		if e.Status == repoagents.StatusPending {
			pending++
		}
	}
	jsonResponse(w, map[string]any{"enabled": true, "pending": pending, "entries": entries})
}

// handleDiscoveredAgentApprove serves POST /api/agents/discovered/approve.
// Owner-only. It re-fetches the file through the allowlist gate, creates the
// agent with definition_source (marked discovered) and, when the repo has a
// sibling <name>.md, prompt_source. Backend, model and mode come from the
// owner's request, never the file; tools are left unset for the owner to grant.
func (s *Server) handleDiscoveredAgentApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	d := s.repoAgents()
	if d == nil {
		jsonError(w, "repo agent discovery is not running", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Backend string `json:"backend"`
		Model   string `json:"model"`
		Mode    string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Mode != "" && !discoveredAgentModes[body.Mode] {
		jsonError(w, "mode must be one of: ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE, NO_GITHUB", http.StatusBadRequest)
		return
	}
	entry, ok := d.Get(body.ID)
	if !ok {
		jsonError(w, repoagents.ErrNotFound.Error(), http.StatusNotFound)
		return
	}
	if entry.Status != repoagents.StatusPending {
		jsonError(w, fmt.Sprintf("only a pending definition can be approved (this one is %s)", entry.Status), http.StatusConflict)
		return
	}
	if !s.deps.Config.GitHubDefinitionAllowed(entry.Repo) {
		jsonError(w, fmt.Sprintf("repo %q is no longer on the repo-sourced prompt allowlist", entry.Repo), http.StatusForbidden)
		return
	}
	def, _, err := d.FetchDefinition(r.Context(), body.ID)
	if err != nil {
		jsonError(w, "could not read the definition: "+err.Error(), http.StatusBadGateway)
		return
	}
	name := body.Name
	if name == "" {
		name = repoagents.AgentName(def.Metadata.Name)
	}
	agentCfg := discoveredAgentConfig(def, entry, body.Backend, body.Model, body.Mode)
	if status, err := s.registerDefinedAgent(name, agentCfg, def.Spec); err != nil {
		jsonError(w, err.Error(), status)
		return
	}
	if _, err := d.Approve(body.ID, name, requestUser(r)); err != nil {
		// The agent exists; only the queue entry lagged (a concurrent scan
		// moved it). Report it rather than undo a registered agent.
		s.logger.Warn("discovered agent registered but its entry was not marked approved", "id", body.ID, "agent", name, "error", err)
	}
	s.auditFromRequest(r, "approve-discovered-agent", auditDetail("repo", entry.Repo, "path", entry.Path), name)
	s.logger.Info("repo-discovered agent approved", "agent", name, "repo", entry.Repo, "path", entry.Path)
	okResponse(w, map[string]string{"status": "approved", "name": name})
}

// discoveredAgentConfig maps an approved definition to the agent config: the
// import mapping, with backend, model, mode and tools replaced by the owner's
// choices and the source pointers set to the repo file.
func discoveredAgentConfig(def *agentDefinition, e repoagents.Entry, backend, model, mode string) config.AgentConfig {
	agentCfg := agentConfigFromDefinition(def)
	agentCfg.Backend = valueOrDefault(backend, defaultDiscoveredBackend)
	agentCfg.Model = model
	agentCfg.Mode = mode
	agentCfg.Tools = nil
	// An approval is an operator create (#5706), like an import.
	agentCfg.PauseOwner = config.FieldOwnerOperator
	agentCfg.DefinitionSource = &config.DefinitionSourceConfig{
		Type:       "github",
		Owner:      e.Owner(),
		Repo:       e.RepoName(),
		Path:       e.Path,
		Discovered: true,
	}
	if e.PromptPath != "" {
		agentCfg.PromptSource = &config.PromptSourceConfig{
			Type:  "github",
			Owner: e.Owner(),
			Repo:  e.RepoName(),
			Path:  e.PromptPath,
		}
	}
	return agentCfg
}

// handleDiscoveredAgentReject serves POST /api/agents/discovered/reject.
// Owner-only. A rejected definition stays rejected until its file is removed.
func (s *Server) handleDiscoveredAgentReject(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	d := s.repoAgents()
	if d == nil {
		jsonError(w, "repo agent discovery is not running", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	e, err := d.Reject(body.ID, requestUser(r))
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, repoagents.ErrNotFound) {
			status = http.StatusNotFound
		}
		jsonError(w, err.Error(), status)
		return
	}
	s.auditFromRequest(r, "reject-discovered-agent", auditDetail("repo", e.Repo, "path", e.Path), "")
	okResponse(w, map[string]string{"status": "rejected", "id": e.ID})
}

// handleDiscoveredAgentsRefresh serves POST /api/agents/discovered/refresh.
// Owner-only. It rescans every managed repo now instead of waiting for the
// timer, and answers with the refreshed list.
func (s *Server) handleDiscoveredAgentsRefresh(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	d := s.repoAgents()
	if d == nil {
		jsonError(w, "repo agent discovery is not running", http.StatusServiceUnavailable)
		return
	}
	d.ScanAll(r.Context())
	s.handleDiscoveredAgentsList(w, r)
}
