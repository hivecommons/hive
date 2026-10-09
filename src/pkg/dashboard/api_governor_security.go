package dashboard

import (
	"net/http"
	"os"
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
		// Sentinel (suspicious-activity alerts) is pointer-typed per field
		// so an absent key means "unchanged" — a save that only toggled a
		// behavior must not wipe the sensitive-path list.
		Sentinel *struct {
			Enabled             *bool     `json:"enabled"`
			Label               *string   `json:"label"`
			SensitivePaths      *[]string `json:"sensitivePaths"`
			DisabledBehaviors   *[]string `json:"disabledBehaviors"`
			ExemptLogins        *[]string `json:"exemptLogins"`
			TrustedAuthorsBlock *bool     `json:"trustedAuthorsBlock"`
			Repos               *[]string `json:"repos"`
			MaxActions          *int      `json:"maxActions"`
		} `json:"sentinel"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	var nextSentinel *config.SentinelConfig
	if body.Sentinel != nil {
		sc := s.deps.Config.Sentinel
		if body.Sentinel.Enabled != nil {
			v := *body.Sentinel.Enabled
			sc.Enabled = &v
		}
		if body.Sentinel.Label != nil {
			label := strings.TrimSpace(*body.Sentinel.Label)
			if err := validateGovernorLabels([]string{label}); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			sc.Label = sanitizeString(label)
		}
		if body.Sentinel.SensitivePaths != nil {
			// An empty list restores the shipped defaults (nil). Operators
			// who want no path rule switch off the sensitive_path behavior.
			sc.SensitivePaths = trimNonEmpty(*body.Sentinel.SensitivePaths)
			if len(sc.SensitivePaths) == 0 {
				sc.SensitivePaths = nil
			}
		}
		if body.Sentinel.DisabledBehaviors != nil {
			sc.DisabledBehaviors = trimNonEmpty(*body.Sentinel.DisabledBehaviors)
		}
		if body.Sentinel.ExemptLogins != nil {
			sc.ExemptLogins = trimNonEmpty(*body.Sentinel.ExemptLogins)
		}
		if body.Sentinel.TrustedAuthorsBlock != nil {
			sc.TrustedAuthorsBlock = *body.Sentinel.TrustedAuthorsBlock
		}
		if body.Sentinel.Repos != nil {
			sc.Repos = trimNonEmpty(*body.Sentinel.Repos)
		}
		if body.Sentinel.MaxActions != nil {
			sc.MaxActions = *body.Sentinel.MaxActions
		}
		if err := config.ValidateSentinel(sc); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		nextSentinel = &sc
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
		cfg.Ioscan.Canaries = body.IoscanCanaries
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
	if nextSentinel != nil {
		cfg.Sentinel = *nextSentinel
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
	if cfg.Ioscan.FailClosedAtLevel(cfg.ACMMLevelOrZero()) {
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

	// credentialWarnings (#9586): the non-fatal GitHub credential-posture
	// diagnosis the spoke logs at ERROR on boot - today an unrecognized
	// HIVE_PROXY_INJECT_GH_AUTH value, which leaves injection OFF and the
	// agents' real tokens in their caches. Boot deliberately does not refuse
	// it (auto-deployed upgrades would crash-loop), so this is where the
	// operator sees it. Read from the process env, the same source the proxy
	// and the token-divert path read.
	credentialWarnings := credentialPostureWarnings(os.Getenv)
	// credentialInjection (#9586): the proxy-injection state and why
	// (explicit on, explicit off, unrecognized, or unset = off because
	// injection is opt-in), matching the line logged at boot - so the operator
	// sees what an unset variable means without reading the pod log.
	credentialInjection := config.ResolveProxyInjectGHAuth(os.Getenv)

	return map[string]interface{}{
		"credentialWarnings":               credentialWarnings,
		"credentialInjection":              credentialInjection,
		"ioscanEnabled":                    cfg.Ioscan.IsEnabled(),
		"ioscanFailMode":                   failMode,
		"ioscanCanaries":                   cfg.Ioscan.CanariesEnabled(),
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
		"sentinel":                         sentinelSectionResponse(cfg.Sentinel),
	}
}

// sentinelSectionResponse renders the suspicious-activity block. It sends
// the EFFECTIVE sensitive-path list plus whether that list is the shipped
// default, so the UI can show the defaults pre-filled and offer a reset.
func sentinelSectionResponse(sc config.SentinelConfig) map[string]interface{} {
	rules := config.SentinelBehaviors()
	behaviors := make([]map[string]interface{}, 0, len(rules))
	disabled := map[string]bool{}
	for _, d := range sc.DisabledBehaviors {
		disabled[strings.ToLower(strings.TrimSpace(d))] = true
	}
	for _, r := range rules {
		behaviors = append(behaviors, map[string]interface{}{
			"name":        r.Name,
			"description": r.Description,
			"enabled":     !disabled[r.Name],
		})
	}
	return map[string]interface{}{
		"enabled":               sc.IsEnabled(),
		"label":                 sc.LabelOrDefault(),
		"sensitivePaths":        sc.EffectiveSensitivePaths(),
		"sensitivePathsDefault": sc.SensitivePaths == nil,
		"defaultSensitivePaths": config.DefaultSentinelSensitivePaths(),
		"behaviors":             behaviors,
		"exemptLogins":          nonNilStrings(sc.ExemptLogins),
		"trustedAuthorsBlock":   sc.TrustedAuthorsBlock,
		"repos":                 nonNilStrings(sc.Repos),
		"maxActions":            sc.MaxActionsOrDefault(),
	}
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// credentialPostureWarnings returns config.ProxyInjectGHAuthWarnings as an
// always-non-nil slice, so the JSON field is [] rather than null.
func credentialPostureWarnings(getenv func(string) string) []string {
	if w := config.ProxyInjectGHAuthWarnings(getenv); w != nil {
		return w
	}
	return []string{}
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
