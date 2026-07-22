package dashboard

import (
	"net/http"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// handleGovernorTrajectory updates the trajectory-review lane config. Fields
// are optional; only those present in the body are changed. Takes effect on
// the next hive restart, like other governor config changes.
func (s *Server) handleGovernorTrajectory(w http.ResponseWriter, r *http.Request) {
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
	// Reconcile the "enabled but no reviewer endpoint" alert against the new
	// state: it must clear when the lane is turned off or an endpoint is set,
	// not linger from a stale startup evaluation.
	s.ReconcileTrajectoryAlert(&s.deps.Config.Governor)
	s.auditFromRequest(r, "config_governor_trajectory", auditDetail("section", "trajectory"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// TrajectoryNotConfiguredAlertID is the dashboard system-alert id for
// "trajectory review is enabled but has no reviewer endpoint."
const TrajectoryNotConfiguredAlertID = "trajectory-not-configured"

// ReconcileTrajectoryAlert raises the "enabled but no reviewer endpoint" alert
// only when the lane is BOTH enabled AND not runnable, and clears it in every
// other case (disabled, or an endpoint now resolves). Safe to call from
// startup and from the config PUT handler.
func (s *Server) ReconcileTrajectoryAlert(g *config.GovernorConfig) {
	if g.Trajectory.IsEnabled() && !g.ReviewerReady() {
		s.AddSystemAlert(TrajectoryNotConfiguredAlertID, "warning",
			"Trajectory review is ON but has no reviewer endpoint configured — no agents are being reviewed. Set a reviewer endpoint (LiteLLM, vLLM, or llm-d) in Governor Config.")
		return
	}
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
