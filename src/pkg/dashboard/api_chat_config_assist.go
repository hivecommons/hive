package dashboard

import (
	"net/http"
	"regexp"
	"strconv"
)

// Chat config assist, phase 1 (RFC #19788, issue #19979): the owner-only,
// audited Settings toggle for chat.config_assist.enabled, and the owner-only
// hint that points at it. No proposals exist yet.

// handleChatConfigAssistGet serves GET /api/config/chat/config-assist.
func (s *Server) handleChatConfigAssistGet(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"enabled":         s.deps.Config.Chat.ConfigAssistEnabled(),
		"runner_attached": s.chatGroundedRunner() != nil,
	})
}

// handleChatConfigAssistPut serves PUT /api/config/chat/config-assist with
// {"enabled": bool}. Owner-only; an absent key changes nothing. The change is
// persisted and audited as config_chat_assist with before and after values.
func (s *Server) handleChatConfigAssistPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	before := cfg.Chat.ConfigAssistEnabled()
	if body.Enabled == nil {
		jsonResponse(w, map[string]interface{}{"enabled": before, "changed": false})
		return
	}
	after := *body.Enabled
	next := cfg.Chat
	v := after
	next.ConfigAssist.Enabled = &v
	cfg.Chat = next
	if err := s.saveConfig(); err != nil {
		prev := before
		cfg.Chat.ConfigAssist.Enabled = &prev
		s.logger.Error("failed to persist chat config assist toggle", "error", err)
		jsonError(w, "could not save the setting: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_chat_assist",
		auditDetail("section", "chat", "before", strconv.FormatBool(before), "after", strconv.FormatBool(after)), "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{"enabled": after, "changed": before != after})
}

var chatConfigShapedRE = regexp.MustCompile(`(?i)\b(switch|change|set|use|configure|reconfigure|update)\b.*\b(model|reasoning|effort|cadence|interval|schedule|agent|enable|disable|every)\b`)

// chatConfigAssistHint is the one-line NUX note appended for an owner whose
// config-shaped question arrives while config assist is off. Non-owners and
// hives with the flag on get "".
func (s *Server) chatConfigAssistHint(r *http.Request, query string) string {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.Config.Chat.ConfigAssistEnabled() {
		return ""
	}
	if !isOwnerRole(r.Header.Get("X-Hive-Role")) || r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		return ""
	}
	if !chatConfigShapedRE.MatchString(query) {
		return ""
	}
	return "Tip: chat can propose config changes (model, reasoning effort, cadence) for you to confirm. Turn it on in Settings → Features → Chat → Config assist."
}
