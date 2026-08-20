package dashboard

import (
	"net/http"
	"strings"

	"github.com/kubestellar/hive/pkg/config"
)

// Bounds for the sandbox kick timeout the dashboard may set. Zero is the
// UNSET sentinel that config defaulting resolves back to the built-in
// default, so it is accepted; negative values are not.
const minAgentSandboxTimeoutS = 0

// handleAgentSandboxGet serves GET /api/config/agent-sandbox so the governor
// Security tab can prefill the Agent Sandbox controls. OWNER-ONLY, matching
// the rest of the governor-config surface: the sandbox posture decides how
// (and whether) agents are credential-isolated.
func (s *Server) handleAgentSandboxGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, agentSandboxSectionResponse(s.deps.Config))
}

// handleAgentSandboxPut serves PUT /api/config/agent-sandbox. Every field is
// a pointer so an absent key leaves that setting untouched — the same "only
// what you send is changed" contract the governor-config writers use.
// saveConfig() persists a secret-free overlay to the PVC that the entrypoint
// merges on restart (see handleGovernorFeatures).
func (s *Server) handleAgentSandboxPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled      *bool     `json:"enabled"`
		Image        *string   `json:"image"`
		EnvAllowlist *[]string `json:"env_allowlist"`
		NetworkMode  *string   `json:"network_mode"`
		TimeoutS     *int      `json:"timeout_s"`
		WorkspaceDir *string   `json:"workspace_dir"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.NetworkMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*body.NetworkMode))
		switch mode {
		case "", "none", "bridge", "host":
		default:
			jsonError(w, "network_mode must be one of: none, bridge, host", http.StatusBadRequest)
			return
		}
	}
	if body.TimeoutS != nil && *body.TimeoutS < minAgentSandboxTimeoutS {
		jsonError(w, "timeout_s must be 0 (default) or greater", http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	sb := &cfg.AgentSandbox
	if body.Enabled != nil {
		sb.Enabled = *body.Enabled
	}
	if body.Image != nil {
		sb.Image = sanitizeString(strings.TrimSpace(*body.Image))
	}
	if body.EnvAllowlist != nil {
		sb.EnvAllowlist = sanitizeStringSlice(*body.EnvAllowlist)
	}
	if body.NetworkMode != nil {
		sb.NetworkMode = strings.ToLower(strings.TrimSpace(*body.NetworkMode))
	}
	if body.TimeoutS != nil {
		sb.TimeoutS = *body.TimeoutS
	}
	if body.WorkspaceDir != nil {
		sb.WorkspaceDir = sanitizeString(strings.TrimSpace(*body.WorkspaceDir))
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent-sandbox update", "error", err)
	}
	s.auditFromRequest(r, "config_agent_sandbox", auditDetail("section", "agent_sandbox"), "")
	s.refreshAndPersist()
	jsonResponse(w, agentSandboxSectionResponse(cfg))
}

// agentSandboxSectionResponse renders the top-level AgentSandboxConfig for
// the dashboard. It is secret-free by construction: the sandbox config holds
// an image reference, an env-var NAME allowlist and paths — never values.
func agentSandboxSectionResponse(cfg *config.Config) map[string]interface{} {
	sb := cfg.AgentSandbox
	allow := sb.EnvAllowlist
	if allow == nil {
		allow = []string{}
	}
	return map[string]interface{}{
		"enabled":       sb.Enabled,
		"image":         sb.Image,
		"env_allowlist": allow,
		"network_mode":  sb.NetworkMode,
		"timeout_s":     sb.TimeoutS,
		"workspace_dir": sb.WorkspaceDir,
	}
}
