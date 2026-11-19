package dashboard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Meeting drafts API (hivecommons/hive#18406): items people asked the
// meeting persona to draft (an issue, an RFC, a Spek session, a knowledge
// note) wait here for an owner or read-write user to approve, edit or
// reject them. pkg/dashboard never imports pkg/meetings; the queue arrives
// through MeetingsActionsService, which cmd/hive implements.
//
// Routes sit under /api/meetings/actions. The list and settings routes are
// literals, so they win over GET /api/meetings/{id}; the per-draft routes
// have five segments with a literal third, so they never overlap a
// /api/meetings/{id}/... route.

// Meeting draft actions (POST /api/meetings/actions/{aid}/{action}).
const (
	MeetingActionApprove = "approve"
	MeetingActionReject  = "reject"
)

// ErrMeetingActionsDisabled is returned while action requests are off.
var ErrMeetingActionsDisabled = errors.New("meeting action requests are turned off (meetings.persona.actions)")

// MeetingActionEditRequest is POST /api/meetings/actions/{aid}/edit:
// omitted fields are kept.
type MeetingActionEditRequest struct {
	Type  *string `json:"type,omitempty"`
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	Repo  *string `json:"repo,omitempty"`
}

// MeetingsActionsService is the draft queue as the dashboard sees it.
// Errors wrap the ErrMeeting* sentinels.
type MeetingsActionsService interface {
	// List returns {drafts, pending}.
	List() (any, error)
	Edit(ctx context.Context, id string, req MeetingActionEditRequest, by string) (any, error)
	// Act runs approve or reject on one draft.
	Act(ctx context.Context, id, action, by string) (any, error)
}

type meetingsActionsHolder struct{ svc MeetingsActionsService }

// SetMeetingsActionsService installs the draft queue (nil removes it).
func (s *Server) SetMeetingsActionsService(svc MeetingsActionsService) {
	s.meetingsActions.Store(&meetingsActionsHolder{svc: svc})
}

func (s *Server) meetingsActionsService(w http.ResponseWriter) (MeetingsActionsService, bool) {
	h := s.meetingsActions.Load()
	if h == nil || h.svc == nil {
		jsonError(w, "meetings are not enabled on this hive (Settings, Meetings)", http.StatusServiceUnavailable)
		return nil, false
	}
	return h.svc, true
}

func (s *Server) registerMeetingsActionsRoutes() {
	s.mux.HandleFunc("GET /api/meetings/actions", s.handleMeetingsActionsList)
	s.mux.HandleFunc("GET /api/meetings/actions/settings", s.handleMeetingsActionsSettingsGet)
	s.mux.HandleFunc("PUT /api/meetings/actions/settings", s.handleMeetingsActionsSettingsPut)
	s.mux.HandleFunc("POST /api/meetings/actions/{aid}/edit", s.handleMeetingsActionEdit)
	s.mux.HandleFunc("POST /api/meetings/actions/{aid}/approve", s.meetingsActionAct(MeetingActionApprove))
	s.mux.HandleFunc("POST /api/meetings/actions/{aid}/reject", s.meetingsActionAct(MeetingActionReject))
}

func (s *Server) meetingsActionsError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrMeetingActionsDisabled) {
		s.writeMeetingsError(w, r, err, http.StatusConflict)
		return
	}
	s.meetingsError(w, r, err)
}

// handleMeetingsActionsList is the "Drafts from meetings" list.
func (s *Server) handleMeetingsActionsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireMeetingsScheduleRole(w, r); !ok {
		return
	}
	svc, ok := s.meetingsActionsService(w)
	if !ok {
		return
	}
	out, err := svc.List()
	if err != nil {
		s.meetingsActionsError(w, r, err)
		return
	}
	jsonResponse(w, out)
}

func (s *Server) handleMeetingsActionEdit(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireMeetingsScheduleRole(w, r); !ok {
		return
	}
	svc, ok := s.meetingsActionsService(w)
	if !ok {
		return
	}
	var req MeetingActionEditRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	id := r.PathValue("aid")
	out, err := svc.Edit(r.Context(), id, req, strings.TrimSpace(r.Header.Get("X-Hive-User")))
	if err != nil {
		s.meetingsActionsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_action_edit", auditDetail("draft", id), "")
	jsonResponse(w, out)
}

// meetingsActionAct answers approve and reject. Speaker identity in a
// meeting is only a display name, so this is the only way a draft becomes
// a real item: an owner or read-write user decides.
func (s *Server) meetingsActionAct(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireMeetingsScheduleRole(w, r); !ok {
			return
		}
		svc, ok := s.meetingsActionsService(w)
		if !ok {
			return
		}
		id := r.PathValue("aid")
		out, err := svc.Act(r.Context(), id, action, strings.TrimSpace(r.Header.Get("X-Hive-User")))
		if err != nil {
			s.meetingsActionsError(w, r, err)
			return
		}
		s.auditFromRequest(r, "meeting_action_"+action, auditDetail("draft", id), "")
		jsonResponse(w, out)
	}
}

// MeetingsActionsSettingsView is GET /api/meetings/actions/settings:
// meetings.persona.actions as configured and as it takes effect.
type MeetingsActionsSettingsView struct {
	Enabled bool `json:"enabled"`
	// Types are the allowed types; AllTypes every type in display order.
	Types    []string `json:"types"`
	AllTypes []string `json:"all_types"`
	// DefaultRepo is meetings.persona.actions.default_repo as set;
	// EffectiveDefaultRepo is where a draft goes when the request names no
	// monitored repo.
	DefaultRepo          string          `json:"default_repo"`
	EffectiveDefaultRepo string          `json:"effective_default_repo"`
	Repos                []string        `json:"repos"`
	AutoApprove          map[string]bool `json:"auto_approve"`
	// AutoApprovable are the types auto_approve may turn on (knowledge).
	AutoApprovable []string `json:"auto_approvable"`
	// EnvOverrides names the HIVE_MEETINGS_PERSONA_ACTIONS_* variables
	// that are set and win over a saved value.
	EnvOverrides []string `json:"env_overrides"`
}

// MeetingsActionsSettingsRequest is PUT /api/meetings/actions/settings:
// omitted fields are kept.
type MeetingsActionsSettingsRequest struct {
	Enabled     *bool           `json:"enabled,omitempty"`
	Types       *[]string       `json:"types,omitempty"`
	DefaultRepo *string         `json:"default_repo,omitempty"`
	AutoApprove map[string]bool `json:"auto_approve,omitempty"`
}

// meetingsActionsEnv are the env overrides the settings view reports.
var meetingsActionsEnv = []string{
	config.MeetingsActionsEnabledEnv, config.MeetingsActionsTypesEnv,
	config.MeetingsActionsDefaultRepoEnv, config.MeetingsActionsAutoApproveKnowledgeEnv,
}

func meetingsActionsSettingsView(cfg *config.Config, getenv func(string) string) MeetingsActionsSettingsView {
	a := cfg.Meetings.Persona.Actions
	res := a.Resolve()
	v := MeetingsActionsSettingsView{
		Enabled: res.Enabled, Types: append([]string{}, res.Types...), AllTypes: append([]string{}, config.MeetingsActionTypes...),
		DefaultRepo: a.DefaultRepo, EffectiveDefaultRepo: cfg.MeetingsActionDefaultRepo(),
		Repos: append([]string{}, cfg.MeetingsActionRepos()...), AutoApprove: res.AutoApprove,
		AutoApprovable: []string{config.MeetingsActionKnowledge}, EnvOverrides: []string{},
	}
	for _, k := range meetingsActionsEnv {
		if strings.TrimSpace(getenv(k)) != "" {
			v.EnvOverrides = append(v.EnvOverrides, k)
		}
	}
	return v
}

func (s *Server) meetingsActionsConfigReady(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := requireMeetingsScheduleRole(w, r); !ok {
		return false
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) handleMeetingsActionsSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !s.meetingsActionsConfigReady(w, r) {
		return
	}
	jsonResponse(w, meetingsActionsSettingsView(s.deps.Config, os.Getenv))
}

// handleMeetingsActionsSettingsPut saves meetings.persona.actions (owner
// only). The queue reads the settings on every request, so a change applies
// without a restart.
func (s *Server) handleMeetingsActionsSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.meetingsActionsConfigReady(w, r) {
		return
	}
	var req MeetingsActionsSettingsRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	next := s.deps.Config.Meetings.Persona.Actions
	if req.Enabled != nil {
		v := *req.Enabled
		next.Enabled = &v
	}
	if req.Types != nil {
		next.Types = normalizeMeetingActionTypes(*req.Types)
		if len(next.Types) == 0 {
			// An empty list means every type in hive.yaml; here it would
			// silently undo "none", so ask for the switch instead.
			jsonError(w, "pick at least one action type, or turn action requests off with enabled: false", http.StatusBadRequest)
			return
		}
	}
	if req.DefaultRepo != nil {
		next.DefaultRepo = strings.Trim(strings.TrimSpace(*req.DefaultRepo), "/")
	}
	if req.AutoApprove != nil {
		auto := map[string]bool{}
		for k, on := range req.AutoApprove {
			if on {
				auto[strings.ToLower(strings.TrimSpace(k))] = true
			}
		}
		next.AutoApprove = auto
	}
	if err := next.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.deps.Config.Meetings.Persona.Actions = next
	if err := s.saveConfig(); err != nil {
		jsonError(w, "saving config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	view := meetingsActionsSettingsView(s.deps.Config, os.Getenv)
	auto := meetingsAutoApproved(view.AutoApprove)
	s.auditFromRequest(r, "meetings_actions_settings", auditDetail("enabled", boolString(view.Enabled),
		"types", strings.Join(view.Types, ","), "default_repo", view.EffectiveDefaultRepo, "auto_approve", strings.Join(auto, ",")), "")
	jsonResponse(w, view)
}

// normalizeMeetingActionTypes lowercases and de-duplicates; an empty list
// stays empty (all types).
func normalizeMeetingActionTypes(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// meetingsAutoApproved lists the auto-approved types in display order.
func meetingsAutoApproved(auto map[string]bool) []string {
	out := []string{}
	for _, t := range config.MeetingsActionTypes {
		if auto[t] {
			out = append(out, t)
		}
	}
	return out
}
