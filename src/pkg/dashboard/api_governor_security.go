package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// handleGovernorSecurity serves PUT /api/config/governor/security. It is
// OWNER-ONLY: the body carries agentSandboxEnabled, ioscanFailMode and
// intentEnforce, so an un-gated version let any read-write member turn the
// agent sandbox off outright. Every sibling governor-config handler
// (health/logging/hub/trajectory/thresholds/...) already calls requireOwnerRole;
// this one was the outlier. Audit F16 (2026-08-13).
func (s *Server) handleGovernorSecurity(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		IoscanEnabled        *bool     `json:"ioscanEnabled"`
		IoscanFailMode       *string   `json:"ioscanFailMode"`
		IoscanCanaries       *bool     `json:"ioscanCanaries"`
		IntentEnforce        *bool     `json:"intentEnforce"`
		IntentAlignmentModel *string   `json:"intentAlignmentModel"`
		ReviewRequire        *bool     `json:"reviewRequireApproval"`
		ReviewFanOut         *bool     `json:"reviewFanOut"`
		ReviewMaxParallel    *int      `json:"reviewMaxParallelReviews"`
		ReviewReviewerAgents *[]string `json:"reviewReviewerAgents"`
		ReviewFixerAgent     *string   `json:"reviewFixerAgent"`
		AgentSandboxEnabled  *bool     `json:"agentSandboxEnabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.IoscanFailMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*body.IoscanFailMode))
		if mode != "" && mode != "open" && mode != "closed" {
			jsonError(w, "ioscan fail_mode must be open or closed", http.StatusBadRequest)
			return
		}
	}

	if body.ReviewMaxParallel != nil && (*body.ReviewMaxParallel < 0 || *body.ReviewMaxParallel > 64) {
		jsonError(w, "review max_parallel_reviews must be between 0 and 64", http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	if body.IoscanEnabled != nil {
		v := *body.IoscanEnabled
		cfg.Ioscan.Enabled = &v
	}
	if body.IoscanFailMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*body.IoscanFailMode))
		if mode == "open" {
			mode = ""
		}
		cfg.Ioscan.FailMode = mode
	}
	if body.IoscanCanaries != nil {
		cfg.Ioscan.Canaries = *body.IoscanCanaries
	}
	if body.IntentEnforce != nil {
		cfg.Intent.Enforce = *body.IntentEnforce
	}
	if body.IntentAlignmentModel != nil {
		cfg.Intent.AlignmentModel = strings.TrimSpace(*body.IntentAlignmentModel)
	}
	if body.ReviewRequire != nil {
		cfg.Review.RequireApproval = *body.ReviewRequire
	}
	if body.ReviewFanOut != nil {
		cfg.Review.FanOut = *body.ReviewFanOut
	}
	if body.ReviewMaxParallel != nil {
		cfg.Review.MaxParallelReviews = *body.ReviewMaxParallel
	}
	if body.ReviewReviewerAgents != nil {
		cfg.Review.ReviewerAgents = sanitizeStringSlice(*body.ReviewReviewerAgents)
	}
	if body.ReviewFixerAgent != nil {
		cfg.Review.FixerAgent = sanitizeString(strings.TrimSpace(*body.ReviewFixerAgent))
	}
	if body.AgentSandboxEnabled != nil {
		cfg.AgentSandbox.Enabled = *body.AgentSandboxEnabled
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after security update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_security", auditDetail("section", "security"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func securitySectionResponse(cfg *config.Config) map[string]interface{} {
	failMode := "open"
	if cfg.Ioscan.FailClosed() {
		failMode = "closed"
	}
	sandboxed := 0
	reviewCapable := 0
	for name, a := range cfg.Agents {
		if a.SandboxEnabled(cfg.AgentSandbox) {
			sandboxed++
		}
		if dashboardAgentReviewCapable(name, a, cfg.Review.ReviewerAgents) {
			reviewCapable++
		}
	}
	// sandboxWarnings surfaces the exact diagnostic that boot/reload already
	// log at WARN (config.AgentSandboxGateWarnings) into the same response the
	// Security tab renders from. Before this, the two-gate misconfiguration
	// #4918 describes — global agent_sandbox.enabled on, no (or partial)
	// per-agent opt-in — was reported only to the server log, which the
	// operator flipping the Security tab toggle has no reason to be watching.
	// Empty means the diagnostic has nothing to say (off globally, or fully
	// opted in); see AgentSandboxGateWarnings for the exact conditions.
	sandboxWarnings := config.AgentSandboxGateWarnings(cfg)
	if sandboxWarnings == nil {
		// Always an array in the JSON response, never null — the frontend
		// (and any other API consumer) should not need a nil-check on top of
		// the falsy-array check it already does.
		sandboxWarnings = []string{}
	}

	return map[string]interface{}{
		"ioscanEnabled":                    cfg.Ioscan.IsEnabled(),
		"ioscanFailMode":                   failMode,
		"ioscanCanaries":                   cfg.Ioscan.Canaries,
		"intentEnforce":                    cfg.Intent.Enforce,
		"intentAlignmentModel":             cfg.Intent.AlignmentModel,
		"reviewRequireApproval":            cfg.Review.RequireApproval,
		"reviewFanOut":                     cfg.Review.FanOut,
		"reviewMaxParallelReviews":         cfg.Review.EffectiveMaxParallelReviews(),
		"reviewReviewerAgents":             cfg.Review.ReviewerAgents,
		"reviewFixerAgent":                 cfg.Review.FixerAgent,
		"reviewCapableAgents":              reviewCapable,
		"reviewSeverityThresholdAvailable": false,
		"agentSandboxEnabled":              cfg.AgentSandbox.Enabled,
		"sandboxedAgents":                  sandboxed,
		"totalAgents":                      len(cfg.Agents),
		"sandboxWarnings":                  sandboxWarnings,
	}
}

func sanitizeStringSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		if s := strings.TrimSpace(sanitizeString(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sanitizedHeaderMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(sanitizeString(k))
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(v)
	}
	return out
}
