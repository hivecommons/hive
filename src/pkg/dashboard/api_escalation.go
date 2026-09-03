package dashboard

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/config"
)

// handleEscalationGet returns the top-level escalation breaker settings
// (Config.Escalation) so the Governor Health tab can prefill its controls.
//
// OWNER-ONLY, matching the write side and the rest of the governor-config
// surface: the escalation breaker decides when a struggling agent is handed to
// a human, so its state is operator-sensitive.
func (s *Server) handleEscalationGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, escalationSectionResponse(s.deps.Config))
}

// handleEscalationPut updates the escalation breaker settings. Every field is a
// pointer, so an absent key leaves that setting untouched — the same "only what
// you send is changed" contract the other governor-config writers use.
// saveConfig() persists a secret-free overlay that the entrypoint merges on
// restart.
func (s *Server) handleEscalationPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Disabled  *bool `json:"disabled"`
		Threshold *int  `json:"threshold"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.Threshold != nil && *body.Threshold < 0 {
		jsonError(w, "threshold must be 0 (default) or greater", http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.Disabled != nil {
		cfg.Escalation.Disabled = *body.Disabled
	}
	if body.Threshold != nil {
		cfg.Escalation.Threshold = *body.Threshold
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after escalation update", "error", err)
	}
	s.auditFromRequest(r, "config_escalation", auditDetail("section", "escalation"), "")
	s.refreshAndPersist()
	jsonResponse(w, escalationSectionResponse(cfg))
}

// escalationSectionResponse renders EscalationConfig for the dashboard,
// resolving the zero-threshold default so the UI never has to know it.
func escalationSectionResponse(cfg *config.Config) map[string]interface{} {
	e := cfg.Escalation
	return map[string]interface{}{
		"disabled":            e.Disabled,
		"threshold":           e.Threshold,
		"effective_threshold": e.EffectiveThreshold(),
	}
}
