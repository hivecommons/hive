package dashboard

import (
	"errors"
	"net/http"
	"strings"
)

type campaignJamAgentRequest struct {
	ThreadID     string `json:"thread_id"`
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	Reply        string `json:"reply"`
	ProposedText string `json:"proposed_text"`
}

func (s *Server) handleCampaignJamAgentsPost(w http.ResponseWriter, r *http.Request) {
	if !requireJamMaintainerRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	var req campaignJamAgentRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	agent := strings.TrimSpace(req.Agent)
	if agent == "" {
		agent = "spektacular"
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.campaignJamAgentModel(agent)
	}
	state, err := s.mutateCampaignJam(id, func(state *CampaignJamState) error {
		return s.inviteCampaignJamAgent(state, req, agent, model)
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "campaign_jam_agent_invite", auditDetail("campaign", id), agent)
	jsonResponse(w, map[string]any{"ok": true, "jam": state})
}

func (s *Server) inviteCampaignJamAgent(state *CampaignJamState, req campaignJamAgentRequest, agent, model string) error {
	if !s.campaignJamAgentAllowed(agent) {
		return errors.New("agent is not configured for Jam replies")
	}
	threadID := strings.TrimSpace(req.ThreadID)
	if threadID == "" {
		return errors.New("thread_id required")
	}
	reply := strings.TrimSpace(req.Reply)
	if reply == "" {
		reply = campaignJamAgentReply(agent, strings.TrimSpace(req.Prompt))
	}
	actor := CampaignJamActor{Type: "agent", Name: agent, Agent: agent, Model: model}
	now := jamNow()
	for i := range state.Threads {
		thread := &state.Threads[i]
		if thread.ID != threadID {
			continue
		}
		thread.Comments = append(thread.Comments, CampaignComment{ID: jamID("comment"), Body: reply, Author: actor, CreatedAt: now})
		thread.UpdatedAt = now
		proposed := strings.TrimSpace(req.ProposedText)
		if proposed == "" {
			proposed = campaignJamAgentSuggestionText(thread.Section, reply)
		}
		state.Suggestions = append(state.Suggestions, CampaignSuggestion{
			ID: jamID("suggestion"), ThreadID: thread.ID, Section: thread.Section, Body: "Agent-proposed spec text from " + agent,
			ProposedText: proposed, Status: jamSuggestionOpen, Author: actor, CreatedAt: now,
		})
		return nil
	}
	return errors.New("thread not found")
}

func (s *Server) campaignJamAgentAllowed(agent string) bool {
	if strings.EqualFold(agent, "spektacular") {
		return true
	}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		_, ok := s.deps.Config.Agents[agent]
		return ok
	}
	return false
}

func (s *Server) campaignJamAgentModel(agent string) string {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		if cfg, ok := s.deps.Config.Agents[agent]; ok {
			return strings.TrimSpace(cfg.Model)
		}
	}
	return ""
}

func campaignJamAgentReply(agent, prompt string) string {
	if prompt == "" {
		return agent + " reviewed the Jam thread and proposed follow-up spec text."
	}
	return agent + " reviewed the Jam thread: " + prompt
}

func campaignJamAgentSuggestionText(section, reply string) string {
	if strings.TrimSpace(section) == "" {
		return reply
	}
	return "Update " + strings.TrimSpace(section) + ":\n" + reply
}
