package dashboard

import (
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/retro"
)

// advisorSmallNBeads is the per-arm closed-bead count below which the
// rank-versus-holdout comparison is too thin to read as a result.
const advisorSmallNBeads = 30

const advisorSmallNCaveat = "Small sample: fewer than 30 closed beads in the deliver or holdout arm, so the means below are not evidence yet."

// advisorDecisionSummarizer is the decision-log side of the summary source;
// *retro.AdvisorDecisionStore implements it.
type advisorDecisionSummarizer interface {
	Summarize(since time.Time) retro.AdvisorDecisionSummary
}

type knowledgeAdvisorSource struct {
	decisions advisorDecisionSummarizer
	// arms is retro.Lane.AdvisorArmSummary; nil when the retro lane is off.
	arms func() []retro.ArmSummary
}

// SetKnowledgeAdvisorSummary installs the sources behind
// GET /api/knowledge/advisor/summary. arms may be nil (retro lane disabled).
func (s *Server) SetKnowledgeAdvisorSummary(decisions advisorDecisionSummarizer, arms func() []retro.ArmSummary) {
	if decisions == nil && arms == nil {
		s.advisorSummary.Store(nil)
		return
	}
	s.advisorSummary.Store(&knowledgeAdvisorSource{decisions: decisions, arms: arms})
}

type advisorArmJSON struct {
	Arm                string  `json:"arm"`
	Beads              int     `json:"beads"`
	MeanFixAttempts    float64 `json:"mean_fix_attempts"`
	MeanCIFailureCount float64 `json:"mean_ci_failure_count"`
	MeanKicksReceived  float64 `json:"mean_kicks_received"`
}

func toAdvisorArmJSON(a retro.ArmSummary) advisorArmJSON {
	return advisorArmJSON{Arm: a.Arm, Beads: a.Beads, MeanFixAttempts: a.MeanFixAttempts, MeanCIFailureCount: a.MeanCIFailureCount, MeanKicksReceived: a.MeanKicksReceived}
}

// handleKnowledgeAdvisorSummary serves GET /api/knowledge/advisor/summary: the
// read-only rank-versus-holdout comparison for the knowledge advisor
// (hivecommons/hive#18897). Counts and means only; it never returns a slug,
// ref, agent, kick id or any content. ?since=<RFC3339> narrows the decision
// counts and cost means; the per-arm bead means always cover every closed bead
// (retro.Lane.AdvisorArmSummary has no window).
func (s *Server) handleKnowledgeAdvisorSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if role := r.Header.Get("X-Hive-Role"); role != "" && !config.RoleAtLeast(role, config.RoleRead) {
		jsonError(w, "read access required", http.StatusForbidden)
		return
	}
	var since time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(w, "since must be an RFC 3339 timestamp", http.StatusBadRequest)
			return
		}
		since = t
	}

	settings := map[string]interface{}{"enabled": false, "mode": config.AdvisorLLMModeOff}
	if s.deps != nil && s.deps.Config != nil {
		k := s.deps.Config.Knowledge
		a := k.AdvisorLLM
		mode := strings.TrimSpace(a.Mode)
		model := strings.TrimSpace(a.Model)
		if model == "" {
			model = strings.TrimSpace(s.deps.Config.Governor.LiteLLM.DefaultModel)
		}
		if !k.Advisor || mode == "" || model == "" {
			mode = config.AdvisorLLMModeOff
		}
		settings = map[string]interface{}{
			"enabled":                  k.Advisor,
			"mode":                     mode,
			"model":                    model,
			"holdout_percent":          a.HoldoutPercent,
			"max_calls_per_day":        orDefaultInt(a.MaxCallsPerDay, config.AdvisorLLMMaxCallsPerDayCap),
			"max_calls_per_agent_hour": orDefaultInt(a.MaxCallsPerAgentHour, config.AdvisorLLMMaxCallsPerAgentHourCap),
			"candidates":               orDefaultInt(a.Candidates, config.DefaultAdvisorLLMCandidates),
			"on_error":                 firstNonEmptyString(strings.TrimSpace(a.OnError), config.AdvisorLLMOnErrorSilence),
		}
	}

	var sum retro.AdvisorDecisionSummary
	var arms []retro.ArmSummary
	if src := s.advisorSummary.Load(); src != nil {
		if src.decisions != nil {
			sum = src.decisions.Summarize(since)
		}
		if src.arms != nil {
			arms = src.arms()
		}
	}
	byMode, byArm, byOutcome := sum.ByMode, sum.ByArm, sum.ByOutcome
	if byMode == nil {
		byMode = map[string]int{}
	}
	if byArm == nil {
		byArm = map[string]int{}
	}
	if byOutcome == nil {
		byOutcome = map[string]int{}
	}

	armRows := []advisorArmJSON{}
	var mixed, none *advisorArmJSON
	deliverBeads, holdoutBeads := 0, 0
	for _, a := range arms {
		row := toAdvisorArmJSON(a)
		switch a.Arm {
		case retro.AdvisorArmMixed:
			mixed = &row
		case retro.AdvisorArmNone:
			none = &row
		default:
			armRows = append(armRows, row)
			if a.Arm == retro.AdvisorArmDeliver {
				deliverBeads = a.Beads
			}
			if a.Arm == retro.AdvisorArmHoldout {
				holdoutBeads = a.Beads
			}
		}
	}
	smallN := deliverBeads < advisorSmallNBeads || holdoutBeads < advisorSmallNBeads

	window := map[string]interface{}{
		"since":           nil,
		"first_decision":  nilIfEmpty(sum.FirstDecision),
		"last_decision":   nilIfEmpty(sum.LastDecision),
		"rank_mode_start": nilIfEmpty(sum.RankModeStart),
		"decisions":       sum.Total,
	}
	if !since.IsZero() {
		window["since"] = since.UTC().Format(time.RFC3339)
	}
	resp := map[string]interface{}{
		"window":     window,
		"settings":   settings,
		"counts":     map[string]interface{}{"by_mode": byMode, "by_arm": byArm, "by_outcome": byOutcome},
		"arms":       armRows,
		"mixed":      mixed,
		"none":       none,
		"arms_scope": "all closed beads with an associated closed PR (not narrowed by since)",
		"cost": map[string]interface{}{
			"latency_samples":        sum.LatencySamples,
			"mean_latency_ms":        sum.MeanLatencyMS,
			"token_samples":          sum.TokenSamples,
			"mean_prompt_tokens":     sum.MeanPromptTokens,
			"mean_completion_tokens": sum.MeanCompletionTokens,
		},
		"small_n":       smallN,
		"small_n_beads": advisorSmallNBeads,
		"caveat":        "",
	}
	if smallN {
		resp["caveat"] = advisorSmallNCaveat
	}
	jsonResponse(w, resp)
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
