package dashboard

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Chat config assist (RFC #19788, issues #19979 and #20092): the owner-only,
// audited Settings panel for chat.config_assist.enabled and
// chat.config_assist.model, and the owner-only hint that points at it.

// chatConfigAssistMaxModel caps the model id a PUT may store.
const chatConfigAssistMaxModel = 200

// handleChatConfigAssistGet serves GET /api/config/chat/config-assist.
// Owner-only, like the PUT, so a non-owner gets the panel's "owner only"
// note instead of controls that would answer 403 on save.
func (s *Server) handleChatConfigAssistGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	chat := s.deps.Config.Chat
	jsonResponse(w, map[string]interface{}{
		"enabled":         chat.ConfigAssistEnabled(),
		"model":           strings.TrimSpace(chat.ConfigAssist.Model),
		"fallback_model":  chat.GroundedAnswersModel(),
		"runner_attached": s.chatGroundedRunner() != nil,
	})
}

// handleChatConfigAssistPut serves PUT /api/config/chat/config-assist with
// {"enabled": bool, "model": string}. Owner-only; an absent key changes
// nothing and an empty model clears the override (back to
// grounded_answers.model). The change is persisted and audited as
// config_chat_assist with before and after values.
func (s *Server) handleChatConfigAssistPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled *bool   `json:"enabled"`
		Model   *string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	before := cfg.Chat.ConfigAssistEnabled()
	prev := cfg.Chat.ConfigAssist
	beforeModel := strings.TrimSpace(prev.Model)
	if body.Enabled == nil && body.Model == nil {
		jsonResponse(w, map[string]interface{}{"enabled": before, "model": beforeModel, "changed": false})
		return
	}
	after, afterModel := before, beforeModel
	if body.Enabled != nil {
		after = *body.Enabled
	}
	if body.Model != nil {
		afterModel = strings.TrimSpace(*body.Model)
		if len(afterModel) > chatConfigAssistMaxModel || strings.ContainsAny(afterModel, " \t\r\n") {
			jsonError(w, "model must be a single model id of at most 200 characters", http.StatusBadRequest)
			return
		}
	}
	next := cfg.Chat
	if body.Enabled != nil {
		v := after
		next.ConfigAssist.Enabled = &v
	}
	next.ConfigAssist.Model = afterModel
	cfg.Chat = next
	// saveConfigStrict, not saveConfig: with no config source path the plain
	// save is an in-memory no-op, and a 200 would claim a change that is
	// lost on restart (409). Roll back to the original values so a failed
	// save does not leave an explicit false that a later save would persist.
	if code, err := s.saveConfigStrict(); err != nil {
		cfg.Chat.ConfigAssist = prev
		if s.logger != nil {
			s.logger.Error("failed to persist chat config assist settings", "error", err)
		}
		jsonError(w, "could not save the setting: "+err.Error(), code)
		return
	}
	s.auditFromRequest(r, "config_chat_assist",
		auditDetail("section", "chat", "before", strconv.FormatBool(before), "after", strconv.FormatBool(after),
			"model", auditChange(beforeModel, afterModel)), "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{
		"enabled": after,
		"model":   afterModel,
		"changed": before != after || beforeModel != afterModel,
	})
}

var chatConfigShapedRE = regexp.MustCompile(`(?i)\b(switch|change|set|use|configure|reconfigure|update|run|disable|stop|pause)\b.*\b(model|reasoning|effort|cadence|interval|schedule|agent|enable|disable|every)\b`)

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
