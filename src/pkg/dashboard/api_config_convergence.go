package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// ── #4263: owner-gated runtime control for the convergence rollout mode ────────
//
// One top-level `convergence` settings block, following the auto_merge/review
// top-level precedents: OWNER-ONLY read and update (requireOwnerRole, which
// only trusts the server-verified live-role marker — a client-supplied role
// header is never enough), validate BEFORE mutating anything, mutate the live
// config, persist through saveConfig (which skips the next watcher reload so
// an older file snapshot cannot immediately overwrite the update), and audit.
//
// The change is live: the eval-cycle seam re-captures the effective mode at
// the start of every pass (CaptureConvergenceMode), so no rebuild or restart
// is needed, and a pass already in flight finishes under its captured pair.
// External YAML edits flow through the existing config watcher and the same
// capture point; HIVE_CONVERGENCE_MODE remains the process-level override and
// is surfaced (not hidden) by the GET below.

// handleConvergenceConfigGet returns the convergence rollout block plus the
// resolved effective mode and its captured generation.
func (s *Server) handleConvergenceConfigGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.convergenceSectionResponse(s.deps.Config))
}

// handleConvergenceConfigPut updates convergence.mode. An invalid mode is
// rejected with 400 BEFORE any live mutation or persistence — the previous
// effective mode/generation remains active, and an unknown value can never
// silently select enforcement.
func (s *Server) handleConvergenceConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	mode, ok := config.NormalizeConvergenceMode(body.Mode)
	if !ok {
		jsonError(w, fmt.Sprintf("invalid convergence mode %q: must be one of %s",
			body.Mode, strings.Join(config.ConvergenceModes(), ", ")), http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	previous := cfg.ConvergenceMode()
	cfg.Convergence.Mode = mode
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after convergence mode update", "error", err)
	}
	s.auditFromRequest(r, "config_convergence",
		auditDetail("section", "convergence", "mode", mode, "previous_effective", previous), "")
	if s.logger != nil && previous != cfg.ConvergenceMode() {
		s.logger.Info("convergence rollout mode updated via runtime settings",
			"mode", cfg.ConvergenceMode(), "previous", previous)
	}
	jsonResponse(w, s.convergenceSectionResponse(cfg))
}

// convergenceSectionResponse renders the convergence block for the dashboard:
// the CONFIGURED mode (what the file says), the EFFECTIVE mode (after the
// HIVE_CONVERGENCE_MODE override), whether that override is in force, and the
// captured generation the eval loop is judging under.
func (s *Server) convergenceSectionResponse(cfg *config.Config) map[string]interface{} {
	configured := config.ConvergenceModeOff
	if m, ok := config.NormalizeConvergenceMode(cfg.Convergence.Mode); ok {
		configured = m
	}
	effective := cfg.ConvergenceMode()
	_, envOverride := config.NormalizeConvergenceMode(os.Getenv(config.ConvergenceModeEnvVar))
	_, generation := s.ConvergenceModeGeneration()
	return map[string]interface{}{
		"mode":           configured,
		"effective_mode": effective,
		"env_override":   envOverride,
		"generation":     generation,
		"modes":          config.ConvergenceModes(),
	}
}
