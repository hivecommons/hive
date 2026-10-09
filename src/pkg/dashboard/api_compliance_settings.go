package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
)

// handleComplianceFrameworksPut serves PUT /api/config/governor/compliance
// (hivecommons/hive#11080): the Settings → Compliance tab's framework picker.
// It sets compliance.frameworks only — posture-check tuning stays YAML-only —
// and an absent `frameworks` key leaves the selection untouched, the same
// "only what you send is changed" contract the other section writers use.
// An empty list deselects every framework. Owner-only, like every governor
// config writer; validated with config.ComplianceConfig.Validate before
// anything is mutated, so an unknown id is a 400 and never half-applied.
// Responds with the re-evaluated compliance.Report.
func (s *Server) handleComplianceFrameworksPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Frameworks *[]string `json:"frameworks"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if body.Frameworks == nil {
		jsonResponse(w, compliance.BuildReport(cfg, complianceGetenv))
		return
	}

	// --- validate before mutating anything ---
	requested := config.ComplianceConfig{Frameworks: *body.Frameworks}
	if err := requested.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// --- apply ---
	previous := strings.Join(cfg.Compliance.SelectedFrameworks(), ",")
	next := cfg.Compliance
	next.Frameworks = requested.SelectedFrameworks()
	cfg.Compliance = next
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after compliance frameworks update", "error", err)
	}
	s.auditFromRequest(r, "config_compliance",
		auditDetail("section", "compliance", "frameworks", strings.Join(next.Frameworks, ","), "previous", previous), "")
	s.refreshAndPersist()
	jsonResponse(w, compliance.BuildReport(cfg, complianceGetenv))
}
