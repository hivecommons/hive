package dashboard

import (
	"net/http"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func (s *Server) handleGovernorSecurity(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IoscanEnabled       *bool   `json:"ioscanEnabled"`
		IoscanFailMode      *string `json:"ioscanFailMode"`
		IoscanCanaries      *bool   `json:"ioscanCanaries"`
		IntentEnforce       *bool   `json:"intentEnforce"`
		ReviewRequire       *bool   `json:"reviewRequireApproval"`
		ReviewFanOut        *bool   `json:"reviewFanOut"`
		RetroEnabled        *bool   `json:"retroEnabled"`
		AgentSandboxEnabled *bool   `json:"agentSandboxEnabled"`
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
	if body.ReviewRequire != nil {
		cfg.Review.RequireApproval = *body.ReviewRequire
	}
	if body.ReviewFanOut != nil {
		cfg.Review.FanOut = *body.ReviewFanOut
	}
	if body.RetroEnabled != nil {
		cfg.Retro.Enabled = *body.RetroEnabled
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
	return map[string]interface{}{
		"ioscanEnabled":         cfg.Ioscan.IsEnabled(),
		"ioscanFailMode":        failMode,
		"ioscanCanaries":        cfg.Ioscan.Canaries,
		"intentEnforce":         cfg.Intent.Enforce,
		"reviewRequireApproval": cfg.Review.RequireApproval,
		"reviewFanOut":          cfg.Review.FanOut,
		"reviewCapableAgents":   reviewCapable,
		"retroEnabled":          cfg.Retro.Enabled,
		"agentSandboxEnabled":   cfg.AgentSandbox.Enabled,
		"sandboxedAgents":       sandboxed,
		"totalAgents":           len(cfg.Agents),
	}
}
