package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Bounds for the governor eval interval the dashboard accepts, matching the
// existing sensing writer (handleGovernorSensing). 0 is the UNSET sentinel
// that config defaulting resolves back to the built-in default, so accepting
// it would silently discard the operator's edit.
const (
	minGeneralEvalIntervalS = 10    // 10 seconds minimum
	maxGeneralEvalIntervalS = 86400 // 24 hours
)

// handleGovernorGeneralAdvancedGet returns the general-tab advanced settings
// (governor eval interval and loop pacing, the hive-wide default explain
// mode, and the attribution-trailer toggle) so the Governor dialog's General tab can prefill
// its controls. OWNER-ONLY, matching the rest of the governor-config surface.
func (s *Server) handleGovernorGeneralAdvancedGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, generalAdvancedSectionResponse(s.deps.Config))
}

// handleGovernorGeneralAdvancedPut updates the general-tab advanced settings.
// Every field is a pointer, so an absent key leaves that setting untouched —
// the same "only what you send is changed" contract the other governor-config
// writers use.
func (s *Server) handleGovernorGeneralAdvancedPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		EvalIntervalS      *int    `json:"eval_interval_s"`
		AttributionTrailer *bool   `json:"attribution_trailer"`
		ExplainMode        *string `json:"explain_mode"`
		governorLoopSettings
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.EvalIntervalS != nil && (*body.EvalIntervalS < minGeneralEvalIntervalS || *body.EvalIntervalS > maxGeneralEvalIntervalS) {
		jsonError(w, fmt.Sprintf("eval_interval_s must be between %d and %d", minGeneralEvalIntervalS, maxGeneralEvalIntervalS), http.StatusBadRequest)
		return
	}
	if err := body.governorLoopSettings.validate(s.deps.Config.Governor, body.EvalIntervalS); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Reject an unknown mode instead of storing it: ResolveExplainModeDefault
	// normalizes anything it does not recognize to "off", so a typo saved here
	// would read back as a set default that quietly does nothing.
	var explainMode string
	if body.ExplainMode != nil {
		explainMode = strings.TrimSpace(*body.ExplainMode)
		if !config.ValidateExplainMode(explainMode) {
			jsonError(w, fmt.Sprintf("explain_mode must be %s, %s or %s (or empty to inherit %s)",
				config.ExplainModeOff, config.ExplainModeBrief, config.ExplainModeFull, config.ExplainModeEnvVar), http.StatusBadRequest)
			return
		}
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.EvalIntervalS != nil {
		cfg.Governor.EvalIntervalS = *body.EvalIntervalS
	}
	body.governorLoopSettings.apply(&cfg.Governor)
	if body.AttributionTrailer != nil {
		v := *body.AttributionTrailer
		cfg.Governor.AttributionTrailer = &v
	}
	// Empty is a real value here — it CLEARS the hive default and hands the
	// question back to HIVE_EXPLAIN_MODE — so this writes whatever was sent
	// rather than skipping on "".
	if body.ExplainMode != nil {
		cfg.Governor.ExplainMode = explainMode
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after general-advanced update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_general_advanced", auditDetail("section", "general_advanced"), "")
	s.refreshAndPersist()
	jsonResponse(w, generalAdvancedSectionResponse(cfg))
}

// generalAdvancedSectionResponse renders the general-tab advanced settings for
// the dashboard, resolving attribution_trailer's tri-state to the boolean the
// hive actually acts on so the UI never has to know the default.
func generalAdvancedSectionResponse(cfg *config.Config) map[string]interface{} {
	return map[string]interface{}{
		"eval_interval_s":     cfg.Governor.EvalIntervalS,
		"attribution_trailer": cfg.Governor.AttributionTrailerEnabled(),
		// The CONFIGURED value ("" = no hive default set), so the form can show
		// "not set" rather than pretending the operator chose the fallback...
		"explain_mode": cfg.Governor.ExplainMode,
		// ...and the RESOLVED value plus where it came from, so an operator can
		// tell a default they set from one the deployment set for them through
		// HIVE_EXPLAIN_MODE — an env var a hosted spoke owner cannot read (#4712).
		"explain_mode_effective": cfg.Governor.ResolveExplainModeDefault(),
		"explain_mode_source":    cfg.Governor.ExplainModeDefaultSource(),

		"eval_interval_max_s":              cfg.Governor.EvalIntervalMaxS,
		"eval_interval_webhook_s":          cfg.Governor.EvalIntervalWebhookS,
		"conserve_interval_multiplier":     cfg.Governor.ConserveIntervalMultiplier,
		"optional_sweep_every_n_cycles":    cfg.Governor.OptionalSweepEveryNCycles,
		"provider_budget_probe_interval_s": cfg.Governor.ProviderBudget.ProbeIntervalS,
		// Display-only: who owns the explicit mode thresholds. "pack" means an
		// ACMM pack seeded them and they still scale with repo count; empty
		// means the operator set them (editing any threshold clears it).
		"thresholds_source": cfg.Governor.ThresholdsSource,
		"loop_defaults": map[string]int{
			"eval_interval_max_s":              config.DefaultEvalIntervalMaxS,
			"eval_interval_webhook_s":          config.DefaultEvalIntervalWebhookS,
			"conserve_interval_multiplier":     config.DefaultConserveIntervalMultiplier,
			"optional_sweep_every_n_cycles":    config.DefaultOptionalSweepEveryNCycles,
			"provider_budget_probe_interval_s": config.DefaultProviderBudgetProbeIntervalS,
		},
	}
}

// governorLoopSettings are the governor eval-loop pacing knobs the General
// tab edits. Absent fields leave the config untouched; 0 restores the
// default. The loop recomputes its interval and sweep cadence every cycle, so
// a saved change applies from the next eval without a restart.
type governorLoopSettings struct {
	EvalIntervalMaxS             *int `json:"eval_interval_max_s"`
	EvalIntervalWebhookS         *int `json:"eval_interval_webhook_s"`
	ConserveIntervalMultiplier   *int `json:"conserve_interval_multiplier"`
	OptionalSweepEveryNCycles    *int `json:"optional_sweep_every_n_cycles"`
	ProviderBudgetProbeIntervalS *int `json:"provider_budget_probe_interval_s"`
}

// validate checks the request with the bounds config.Validate enforces at
// boot, so the dialog cannot save a value the next start would reject.
// evalIntervalS is the eval_interval_s carried by the same request, if any.
func (b governorLoopSettings) validate(cur config.GovernorConfig, evalIntervalS *int) error {
	for _, f := range []struct {
		v       *int
		name    string
		seconds bool
	}{
		{b.EvalIntervalMaxS, "eval_interval_max_s", true},
		{b.EvalIntervalWebhookS, "eval_interval_webhook_s", true},
		{b.ConserveIntervalMultiplier, "conserve_interval_multiplier", false},
		{b.OptionalSweepEveryNCycles, "optional_sweep_every_n_cycles", false},
		{b.ProviderBudgetProbeIntervalS, "provider_budget_probe_interval_s", true},
	} {
		if f.v == nil {
			continue
		}
		if *f.v < 0 {
			return fmt.Errorf("%s must be 0 (default) or greater", f.name)
		}
		if f.seconds && *f.v > maxGeneralEvalIntervalS {
			return fmt.Errorf("%s must be at most %d", f.name, maxGeneralEvalIntervalS)
		}
	}
	if b.EvalIntervalMaxS == nil && evalIntervalS == nil {
		return nil
	}
	base, maxS := cur.EvalIntervalS, cur.EvalIntervalMaxS
	if evalIntervalS != nil {
		base = *evalIntervalS
	}
	if b.EvalIntervalMaxS != nil {
		maxS = *b.EvalIntervalMaxS
		if maxS == 0 {
			maxS = config.DefaultEvalIntervalMaxS
		}
	}
	if maxS > 0 && base > 0 && maxS < base {
		return fmt.Errorf("eval_interval_max_s must be greater than or equal to eval_interval_s")
	}
	return nil
}

func (b governorLoopSettings) apply(g *config.GovernorConfig) {
	// A cleared field restores the default, as applyDefaults does at Load.
	for _, f := range []struct {
		src *int
		dst *int
		def int
	}{
		{b.EvalIntervalMaxS, &g.EvalIntervalMaxS, config.DefaultEvalIntervalMaxS},
		{b.EvalIntervalWebhookS, &g.EvalIntervalWebhookS, config.DefaultEvalIntervalWebhookS},
		{b.ConserveIntervalMultiplier, &g.ConserveIntervalMultiplier, config.DefaultConserveIntervalMultiplier},
		{b.OptionalSweepEveryNCycles, &g.OptionalSweepEveryNCycles, config.DefaultOptionalSweepEveryNCycles},
	} {
		if f.src == nil {
			continue
		}
		*f.dst = *f.src
		if *f.dst == 0 {
			*f.dst = f.def
		}
	}
	// 0 is kept as-is: EffectiveProbeInterval already treats it as the default.
	if b.ProviderBudgetProbeIntervalS != nil {
		g.ProviderBudget.ProbeIntervalS = *b.ProviderBudgetProbeIntervalS
	}
}
