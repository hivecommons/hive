package dashboard

import (
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Meeting attendee model (hivecommons/hive#18517): Settings, Meeting
// attendee picks the model the persona and Spektacular live mode answer
// with: meetings.llm.mode, and on the agent-backend mode
// meetings.llm.backend and meetings.llm.model. The picker lists the
// backends and models from the same catalog as the agents' own backend and
// model pickers (/api/config/backends); this endpoint says which of them
// can answer for meetings and what the defaults are. The meetings feature
// resolves the model at boot, so a saved change applies after a restart.

// meetingsModelEnv are the env overrides that win over a saved choice.
var meetingsModelEnv = []string{config.MeetingsLLMModeEnv, config.MeetingsLLMBackendEnv, config.MeetingsLLMModelEnv}

// MeetingsModelView is GET and PUT /api/meetings/model.
type MeetingsModelView struct {
	Mode    string `json:"mode"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
	// Label is what the meeting panel shows for the resolved model.
	Label string `json:"label"`
	// Modes are the selectable meetings.llm.mode values.
	Modes []string `json:"modes"`
	// SupportedBackends can answer for meetings (CLI backends with a
	// completion profile, LiteLLM, vllm, llm-d and the hive's named model
	// gateways); any other backend is listed by the picker but cannot be
	// chosen.
	SupportedBackends []string `json:"supported_backends"`
	// RefusedBackends says, per known backend that cannot answer for
	// meetings, why (#18549).
	RefusedBackends map[string]string `json:"refused_backends"`
	// AgentBackends are the backends the hive's agents are configured with.
	AgentBackends []string `json:"agent_backends"`
	// DefaultBackend and DefaultModel apply when backend and model are
	// empty: the hive's most common agent backend, and the meetings default
	// model for it.
	DefaultBackend string `json:"default_backend"`
	DefaultModel   string `json:"default_model"`
	// DefaultModels is the default model per supported backend.
	DefaultModels   map[string]string `json:"default_models"`
	EnvOverrides    []string          `json:"env_overrides"`
	RestartRequired bool              `json:"restart_required"`
}

// MeetingsModelRequest is PUT /api/meetings/model; absent fields keep
// their value.
type MeetingsModelRequest struct {
	Mode    *string `json:"mode"`
	Backend *string `json:"backend"`
	Model   *string `json:"model"`
}

func (s *Server) registerMeetingsModelRoutes() {
	s.mux.HandleFunc("GET /api/meetings/model", s.handleMeetingsModelGet)
	s.mux.HandleFunc("PUT /api/meetings/model", s.handleMeetingsModelPut)
}

func meetingsModelView(r *http.Request, cfg *config.Config, getenv func(string) string) MeetingsModelView {
	l := cfg.Meetings.LLM
	defBackend, _ := cfg.MeetingsAgentDefault()
	v := MeetingsModelView{
		Mode: l.Mode, Backend: l.Backend, Model: l.Model,
		Label:             cfg.ResolveMeetingsLLM(r.Context()).Label(),
		Modes:             append([]string{}, config.MeetingsLLMModes...),
		SupportedBackends: cfg.MeetingsLLMSupportedBackendsFor(),
		RefusedBackends:   config.MeetingsRefusedBackendReasons(),
		AgentBackends:     []string{},
		DefaultBackend:    defBackend,
		DefaultModel:      cfg.MeetingsDefaultModelFor(defBackend),
		DefaultModels:     map[string]string{},
		EnvOverrides:      []string{},
	}
	if v.Mode == "" {
		v.Mode = config.MeetingsLLMModeAuto
	}
	seen := map[string]bool{}
	for _, a := range cfg.Agents {
		b := strings.ToLower(strings.TrimSpace(a.Backend))
		if b != "" && !seen[b] {
			seen[b] = true
			v.AgentBackends = append(v.AgentBackends, b)
		}
	}
	sort.Strings(v.AgentBackends)
	// A gateway the picker lists that cannot answer (a watsonx one, or one
	// with no endpoint) says why too.
	for _, gw := range cfg.Governor.Gateways {
		name := strings.ToLower(strings.TrimSpace(gw.Name))
		if why := cfg.MeetingsLLMBackendRefusalFor(name); name != "" && why != "" {
			if _, known := v.RefusedBackends[name]; !known {
				v.RefusedBackends[name] = why
			}
		}
	}
	for _, b := range v.SupportedBackends {
		v.DefaultModels[b] = cfg.MeetingsDefaultModelFor(b)
	}
	for _, k := range meetingsModelEnv {
		if strings.TrimSpace(getenv(k)) != "" {
			v.EnvOverrides = append(v.EnvOverrides, k)
		}
	}
	return v
}

func (s *Server) handleMeetingsModelGet(w http.ResponseWriter, r *http.Request) {
	if !s.meetingsActionsConfigReady(w, r) {
		return
	}
	jsonResponse(w, meetingsModelView(r, s.deps.Config, os.Getenv))
}

// handleMeetingsModelPut saves meetings.llm.mode, backend and model (owner
// only). A backend that cannot answer for meetings is refused with the
// reason.
func (s *Server) handleMeetingsModelPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.meetingsActionsConfigReady(w, r) {
		return
	}
	var req MeetingsModelRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	next := s.deps.Config.Meetings.LLM
	if req.Mode != nil {
		next.Mode = strings.ToLower(strings.TrimSpace(*req.Mode))
	}
	if req.Backend != nil {
		next.Backend = strings.ToLower(strings.TrimSpace(*req.Backend))
	}
	if req.Model != nil {
		next.Model = strings.TrimSpace(*req.Model)
	}
	if err := s.deps.Config.ValidateMeetingsLLMSelection(next.Mode, next.Backend); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	prev := s.deps.Config.Meetings.LLM
	changed := prev.Mode != next.Mode || prev.Backend != next.Backend || prev.Model != next.Model
	s.deps.Config.Meetings.LLM = next
	if err := s.saveConfig(); err != nil {
		s.deps.Config.Meetings.LLM = prev
		jsonError(w, "saving config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	view := meetingsModelView(r, s.deps.Config, os.Getenv)
	view.RestartRequired = changed
	s.auditFromRequest(r, "meetings_model_settings", auditDetail("mode", next.Mode, "backend", next.Backend, "model", next.Model), "")
	jsonResponse(w, view)
}
