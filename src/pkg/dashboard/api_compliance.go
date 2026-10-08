package dashboard

import (
	"net/http"
	"os"

	"github.com/hivecommons/hive/pkg/compliance"
)

// complianceGetenv reads env-backed compliance settings (credential-injection
// posture); tests swap it.
var complianceGetenv = os.Getenv

// handleComplianceStatus serves GET /api/compliance/status
// (hivecommons/hive#11078): every control of the frameworks selected in
// `compliance.frameworks`, evaluated against the live config, with the
// current vs recommended value per mapped setting. Read-only. Gated to
// merger or owner because the report describes the hive's access-control
// and merge posture in detail.
func (s *Server) handleComplianceStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, compliance.BuildReport(s.deps.Config, complianceGetenv))
}
