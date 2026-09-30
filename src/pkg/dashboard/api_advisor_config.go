package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Advisor-lane Settings (hivecommons/hive#9725, phase 4 of #9638): the
// dashboard's editor for the fleet advisor block, each agent's override or
// opt-out, and the named model_roles map. Every edit is validated by the same
// checks hive.yaml loading runs (config.ValidateAdvisorLane) BEFORE anything
// is mutated, then persisted to hive.yaml; the agent's next launch picks the
// new value up, exactly as it would a hand edit of the file. Reads and writes
// are owner-only, like the other governor settings sections — no new way in,
// no new authority.

// advisorAgentView is one agent's row in the Settings view: its own override
// (nil = inherits the fleet block), the value in force, and whether the
// advisor actually reviews it on its backend.
type advisorAgentView struct {
	Name         string                  `json:"name"`
	Backend      string                  `json:"backend,omitempty"`
	Override     *config.AdvisorOverride `json:"override"`
	Effective    config.AdvisorEffective `json:"effective"`
	Active       bool                    `json:"active"`
	ActiveReason string                  `json:"active_reason,omitempty"`
}

// advisorSettingsDefaults carries the built-in values the view falls back to,
// and the backend bound the consecutive-block limit must stay under.
type advisorSettingsDefaults struct {
	TimeoutS             int `json:"timeout_s"`
	MaxConsecutiveBlocks int `json:"max_consecutive_blocks"`
	BlockBound           int `json:"block_bound"`
}

// advisorSettingsResponse is the GET/PUT /api/config/advisor* payload.
type advisorSettingsResponse struct {
	Fleet      config.AdvisorConfig        `json:"fleet"`
	ModelRoles map[string]config.ModelRole `json:"model_roles"`
	Defaults   advisorSettingsDefaults     `json:"defaults"`
	Agents     []advisorAgentView          `json:"agents"`
}

func advisorSettingsSection(cfg *config.Config) advisorSettingsResponse {
	roles := make(map[string]config.ModelRole, len(cfg.ModelRoles))
	for name, role := range cfg.ModelRoles {
		roles[name] = role
	}
	resp := advisorSettingsResponse{
		Fleet:      cfg.Advisor,
		ModelRoles: roles,
		Defaults: advisorSettingsDefaults{
			TimeoutS:             config.DefaultAdvisorTimeoutS,
			MaxConsecutiveBlocks: config.DefaultAdvisorMaxConsecutiveBlocks,
			BlockBound:           config.AdvisorCopilotBlockBound,
		},
		Agents: []advisorAgentView{},
	}
	for _, name := range cfg.AdvisorAgentNames() {
		agent := cfg.Agents[name]
		active, reason := cfg.AdvisorActiveForAgent(name)
		resp.Agents = append(resp.Agents, advisorAgentView{
			Name:         name,
			Backend:      agent.Backend,
			Override:     agent.Advisor,
			Effective:    cfg.AdvisorEffectiveFor(name),
			Active:       active,
			ActiveReason: reason,
		})
	}
	return resp
}

// advisorLaneCandidate copies exactly the parts of cfg the advisor-lane
// validator reads, so an edit can be validated without touching the live
// config.
func advisorLaneCandidate(cfg *config.Config) *config.Config {
	candidate := &config.Config{
		Advisor:    cfg.Advisor,
		ModelRoles: make(map[string]config.ModelRole, len(cfg.ModelRoles)),
		Agents:     make(map[string]config.AgentConfig, len(cfg.Agents)),
	}
	for name, role := range cfg.ModelRoles {
		candidate.ModelRoles[name] = role
	}
	for name, agent := range cfg.Agents {
		candidate.Agents[name] = config.AgentConfig{Backend: agent.Backend, Advisor: agent.Advisor}
	}
	return candidate
}

// advisorSettingsReady answers 503 when there is no config to edit.
func (s *Server) advisorSettingsReady(w http.ResponseWriter) bool {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// persistAdvisorSettings saves hive.yaml after an advisor edit, audits it, and
// answers with the refreshed section. A failed save is reported, never
// swallowed: the dashboard must not claim a value is in force for the next
// launch when hive.yaml still says otherwise.
func (s *Server) persistAdvisorSettings(w http.ResponseWriter, r *http.Request, detail ...string) {
	if err := s.saveConfig(); err != nil {
		if s.logger != nil {
			s.logger.Error("failed to persist config after advisor settings update", "error", err)
		}
		jsonError(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_advisor", auditDetail(append([]string{"section", "advisor"}, detail...)...), "")
	jsonResponse(w, advisorSettingsSection(s.deps.Config))
}

// handleAdvisorSettingsGet serves GET /api/config/advisor: the fleet block,
// the roles map, and every agent's override and in-force value.
func (s *Server) handleAdvisorSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.advisorSettingsReady(w) {
		return
	}
	jsonResponse(w, advisorSettingsSection(s.deps.Config))
}

// advisorFleetBody is the PUT /api/config/advisor request: only fields
// present in the body change.
type advisorFleetBody struct {
	Enabled              *bool   `json:"enabled"`
	Model                *string `json:"model"`
	Instructions         *string `json:"instructions"`
	DailyBudgetTokens    *int    `json:"daily_budget_tokens"`
	TimeoutS             *int    `json:"timeout_s"`
	MaxConsecutiveBlocks *int    `json:"max_consecutive_blocks"`
}

// validateAdvisorInts rejects negative budget/timeout/limit values with the
// field name, before the lane validator runs.
func validateAdvisorInts(fields map[string]*int) error {
	for name, v := range fields {
		if v != nil && *v < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	return nil
}

// handleAdvisorSettingsPut serves PUT /api/config/advisor: edits the
// fleet-wide advisor block.
func (s *Server) handleAdvisorSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.advisorSettingsReady(w) {
		return
	}
	var body advisorFleetBody
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateAdvisorInts(map[string]*int{
		"daily_budget_tokens":    body.DailyBudgetTokens,
		"timeout_s":              body.TimeoutS,
		"max_consecutive_blocks": body.MaxConsecutiveBlocks,
	}); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	next := s.deps.Config.Advisor
	if body.Enabled != nil {
		v := *body.Enabled
		next.Enabled = &v
	}
	if body.Model != nil {
		next.Model = strings.TrimSpace(*body.Model)
	}
	if body.Instructions != nil {
		next.Instructions = strings.TrimSpace(*body.Instructions)
	}
	if body.DailyBudgetTokens != nil {
		next.DailyBudgetTokens = *body.DailyBudgetTokens
	}
	if body.TimeoutS != nil {
		next.TimeoutS = *body.TimeoutS
	}
	if body.MaxConsecutiveBlocks != nil {
		next.MaxConsecutiveBlocks = *body.MaxConsecutiveBlocks
	}
	candidate := advisorLaneCandidate(s.deps.Config)
	candidate.Advisor = next
	if err := candidate.ValidateAdvisorLane(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.deps.Config.Advisor = next
	s.persistAdvisorSettings(w, r, "scope", "fleet", "enabled", fmt.Sprint(next.IsEnabled()), "model", next.Model)
}

// handleAdvisorRolesPut serves PUT /api/config/advisor/roles: replaces the
// named model_roles map. A role still referenced by the advisor block or an
// agent override cannot be removed — the validator names the reference.
func (s *Server) handleAdvisorRolesPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.advisorSettingsReady(w) {
		return
	}
	var body struct {
		ModelRoles map[string]config.ModelRole `json:"model_roles"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	roles := make(map[string]config.ModelRole, len(body.ModelRoles))
	for rawName, role := range body.ModelRoles {
		name := strings.TrimSpace(rawName)
		if name == "" || strings.ContainsAny(name, " \t@") {
			jsonError(w, fmt.Sprintf("model_roles: invalid role name %q (no spaces or @)", rawName), http.StatusBadRequest)
			return
		}
		roles[name] = config.ModelRole{
			Backend:         strings.TrimSpace(role.Backend),
			Model:           strings.TrimSpace(role.Model),
			ReasoningEffort: strings.TrimSpace(role.ReasoningEffort),
		}
	}
	for name, role := range roles {
		if role.Backend == "" {
			continue
		}
		if err := s.deps.Config.Governor.ValidateBackend(role.Backend); err != nil {
			jsonError(w, fmt.Sprintf("model_roles.%s: %v", name, err), http.StatusBadRequest)
			return
		}
	}
	candidate := advisorLaneCandidate(s.deps.Config)
	candidate.ModelRoles = roles
	if err := candidate.ValidateAdvisorLane(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(roles) == 0 {
		roles = nil
	}
	s.deps.Config.ModelRoles = roles
	s.persistAdvisorSettings(w, r, "scope", "model_roles", "roles", fmt.Sprint(len(roles)))
}

// handleAdvisorAgentPut serves PUT /api/config/advisor/agent/{name}: sets or
// clears one agent's advisor override. {"override": null} (or an override
// with no fields) clears it, so the agent inherits the fleet block again;
// {"override": {"enabled": false}} opts the agent out.
func (s *Server) handleAdvisorAgentPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.advisorSettingsReady(w) {
		return
	}
	name := r.PathValue("name")
	agent, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}
	var body struct {
		Override *config.AdvisorOverride `json:"override"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	o := body.Override
	if o != nil {
		if err := validateAdvisorInts(map[string]*int{
			"daily_budget_tokens":    o.DailyBudgetTokens,
			"timeout_s":              o.TimeoutS,
			"max_consecutive_blocks": o.MaxConsecutiveBlocks,
		}); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if o.Model != nil {
			v := strings.TrimSpace(*o.Model)
			o.Model = &v
		}
		if o.Instructions != nil {
			v := strings.TrimSpace(*o.Instructions)
			o.Instructions = &v
		}
		if *o == (config.AdvisorOverride{}) {
			o = nil
		}
	}
	candidate := advisorLaneCandidate(s.deps.Config)
	candidate.Agents[name] = config.AgentConfig{Backend: agent.Backend, Advisor: o}
	if err := candidate.ValidateAdvisorLane(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	agent.Advisor = o
	s.deps.Config.Agents[name] = agent
	scope := "agent-override"
	if o == nil {
		scope = "agent-inherit"
	}
	s.persistAdvisorSettings(w, r, "scope", scope, "agent", name)
}
