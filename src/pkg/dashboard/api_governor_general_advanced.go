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
// (governor eval interval, the hive-wide default explain mode, and the
// attribution-trailer toggle) so the Governor dialog's General tab can prefill
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
	}
}
