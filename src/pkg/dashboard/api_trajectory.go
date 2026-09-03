package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// handleGovernorTrajectory updates the trajectory-review lane config. Fields
// are optional; only those present in the body are changed. Takes effect on
// the next hive restart, like other governor config changes.
func (s *Server) handleGovernorTrajectory(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled         *bool   `json:"enabled"`
		IntervalS       *int    `json:"intervalS"`
		Model           *string `json:"model"`
		Endpoint        *string `json:"endpoint"`
		TranscriptLines *int    `json:"transcriptLines"`
		OnDivergence    *string `json:"onDivergence"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	t := &s.deps.Config.Governor.Trajectory
	if body.Enabled != nil {
		v := *body.Enabled
		t.Enabled = &v
	}
	if body.IntervalS != nil && *body.IntervalS >= 0 {
		t.IntervalS = *body.IntervalS
	}
	if body.Model != nil {
		t.Model = strings.TrimSpace(*body.Model)
	}
	if body.Endpoint != nil {
		t.Endpoint = strings.TrimSpace(*body.Endpoint)
	}
	if body.TranscriptLines != nil && *body.TranscriptLines >= 0 {
		t.TranscriptLines = *body.TranscriptLines
	}
	if body.OnDivergence != nil {
		v := strings.TrimSpace(*body.OnDivergence)
		if v == "pause" || v == "alert" {
			t.OnDivergence = v
		}
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after trajectory update", "error", err)
	}
	// Clear any legacy "not configured" banner alert. The half-configured
	// state is surfaced inline in Settings → General instead of the
	// top banner (see ReconcileTrajectoryAlert).
	s.ReconcileTrajectoryAlert(&s.deps.Config.Governor)
	s.auditFromRequest(r, "config_governor_trajectory", auditDetail("section", "trajectory"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// TrajectoryNotConfiguredAlertID is the dashboard system-alert id formerly
// used for "trajectory review is enabled but has no reviewer endpoint."
// The alert is no longer raised; the id is retained so ReconcileTrajectoryAlert
// can clear one persisted by an older build.
const TrajectoryNotConfiguredAlertID = "trajectory-not-configured"

// ReconcileTrajectoryAlert clears the legacy "enabled but no reviewer
// endpoint" system alert. It no longer raises it.
//
// Why the banner went away: the lane defaults to ON (TrajectoryConfig.IsEnabled
// returns true when Enabled is nil), so on a hive that has never configured a
// LiteLLM endpoint the alert fired on first boot for everyone — it flagged the
// untouched default, not an operator who opted in and then misconfigured it.
// That made it a "you have not set up an optional feature" nag occupying the
// top banner, which is reserved for conditions demanding action. The lane also
// fails open (an unreachable reviewer returns "not divergent"), so the inert
// state never pauses or breaks a working agent.
//
// The half-configured state is still surfaced, just not as a top-of-page
// warning: Settings → General shows an amber "On — no reviewer endpoint
// (not running)" status chip plus a "Resolved: none — reviewer will not run"
// hint next to the endpoint field, and Getting Started → More to explore
// mentions the feature as an advanced capability.
//
// This is still called from startup and the config PUT handler so that an
// alert persisted by an older build is cleared rather than left stuck in the
// banner forever.
// The config argument is retained (unused) so both call sites and any future
// re-introduction of a narrower, opt-in-only alert keep a stable signature.
func (s *Server) ReconcileTrajectoryAlert(_ *config.GovernorConfig) {
	s.ClearSystemAlert(TrajectoryNotConfiguredAlertID)
}

// trajectorySectionResponse builds the trajectory config payload for the
// governor config GET. It exposes reviewerReady (endpoint AND model resolve)
// and the effective/source of the endpoint so the UI can distinguish
// "on and running" from "on but no reviewer". The API key value is never
// returned — only whether one resolved.
func trajectorySectionResponse(g *config.GovernorConfig) map[string]interface{} {
	endpoint, key, model := g.ResolveReviewer()
	// Where did the endpoint come from — the trajectory block or the inherited
	// LiteLLM block? Helps the UI point the operator at the right field.
	endpointSource := "none"
	if strings.TrimSpace(g.Trajectory.Endpoint) != "" {
		endpointSource = "trajectory"
	} else if endpoint != "" {
		endpointSource = "litellm"
	}
	return map[string]interface{}{
		"enabled":           g.Trajectory.IsEnabled(),
		"intervalS":         g.Trajectory.IntervalS,
		"model":             g.Trajectory.Model,
		"effectiveModel":    model,
		"transcriptLines":   g.Trajectory.TranscriptLines,
		"onDivergence":      g.Trajectory.OnDivergence,
		"exemptAgents":      g.Trajectory.ExemptAgents,
		"endpoint":          g.Trajectory.Endpoint,
		"effectiveEndpoint": endpoint,
		"endpointSource":    endpointSource,
		"hasKey":            key != "",
		"reviewerReady":     endpoint != "" && model != "",
	}
}
