package dashboard

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func (s *Server) handleGovernorSecurity(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IoscanEnabled        *bool              `json:"ioscanEnabled"`
		IoscanFailMode       *string            `json:"ioscanFailMode"`
		IoscanCanaries       *bool              `json:"ioscanCanaries"`
		IntentEnforce        *bool              `json:"intentEnforce"`
		IntentAlignmentModel *string            `json:"intentAlignmentModel"`
		ReviewRequire        *bool              `json:"reviewRequireApproval"`
		ReviewFanOut         *bool              `json:"reviewFanOut"`
		ReviewMaxParallel    *int               `json:"reviewMaxParallelReviews"`
		ReviewReviewerAgents *[]string          `json:"reviewReviewerAgents"`
		ReviewFixerAgent     *string            `json:"reviewFixerAgent"`
		RetroEnabled         *bool              `json:"retroEnabled"`
		RetroAnalysisModel   *string            `json:"retroAnalysisModel"`
		OTelEnabled          *bool              `json:"otelEnabled"`
		OTelEndpoint         *string            `json:"otelEndpoint"`
		OTelServiceName      *string            `json:"otelServiceName"`
		OTelInsecure         *bool              `json:"otelInsecure"`
		OTelSampleRatio      *float64           `json:"otelSampleRatio"`
		OTelHeaders          *map[string]string `json:"otelHeaders"`
		AgentSandboxEnabled  *bool              `json:"agentSandboxEnabled"`
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
	if body.OTelEndpoint != nil {
		ep := strings.TrimSpace(*body.OTelEndpoint)
		if ep != "" {
			u, err := url.Parse(ep)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				jsonError(w, "otel endpoint must be empty or an http(s) URL", http.StatusBadRequest)
				return
			}
		}
	}
	if body.OTelSampleRatio != nil && (*body.OTelSampleRatio < minTracingSampleRatio || *body.OTelSampleRatio > maxTracingSampleRatio) {
		jsonError(w, "otel sample_ratio must be between 0.0 and 1.0", http.StatusBadRequest)
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
	if body.RetroEnabled != nil {
		cfg.Retro.Enabled = *body.RetroEnabled
	}
	if body.RetroAnalysisModel != nil {
		cfg.Retro.AnalysisModel = strings.TrimSpace(*body.RetroAnalysisModel)
	}
	if body.OTelEnabled != nil || body.OTelEndpoint != nil || body.OTelServiceName != nil || body.OTelInsecure != nil || body.OTelSampleRatio != nil || body.OTelHeaders != nil {
		merged := cfg.EffectiveOTel()
		if body.OTelEnabled != nil {
			merged.Enabled = *body.OTelEnabled
		}
		if body.OTelEndpoint != nil {
			merged.Endpoint = strings.TrimSpace(*body.OTelEndpoint)
		}
		if body.OTelServiceName != nil {
			merged.ServiceName = strings.TrimSpace(*body.OTelServiceName)
		}
		if body.OTelInsecure != nil {
			merged.Insecure = *body.OTelInsecure
		}
		if body.OTelSampleRatio != nil {
			merged.SampleRatio = *body.OTelSampleRatio
		}
		if body.OTelHeaders != nil {
			merged.Headers = sanitizedHeaderMap(*body.OTelHeaders)
		}
		cfg.OTel = merged
		cfg.Tracing = merged
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
	otelCfg := cfg.EffectiveOTel()
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
		"retroEnabled":                     cfg.Retro.Enabled,
		"retroAnalysisModel":               cfg.Retro.AnalysisModel,
		"otelEnabled":                      otelCfg.Enabled,
		"otelEndpoint":                     otelCfg.Endpoint,
		"otelServiceName":                  otelCfg.ServiceName,
		"otelInsecure":                     otelCfg.Insecure,
		"otelSampleRatio":                  otelCfg.SampleRatio,
		"otelHasHeaders":                   len(otelCfg.Headers) > 0,
		"agentSandboxEnabled":              cfg.AgentSandbox.Enabled,
		"sandboxedAgents":                  sandboxed,
		"totalAgents":                      len(cfg.Agents),
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
