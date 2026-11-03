package dashboard

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// stageTrackerSettings is the live timing block the tracker payload carries
// for the settings modal (hivecommons/hive#15813): the config defaults when
// no config is wired.
func (s *Server) stageTrackerSettings() StageTrackerSettings {
	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	soak := config.DefaultContributorLaneSoakMinutes
	if cfg != nil {
		soak = cfg.AutoMerge.ContributorLane.EffectiveSoakMinutes()
	}
	return StageTrackerSettings{
		ToastInfoS:  int(cfg.StageTrackerToastInfo() / time.Second),
		ToastErrorS: int(cfg.StageTrackerToastError() / time.Second),
		SoakMinutes: soak,
	}
}

// dashboardStageTrackerPayload is the dashboard.stage_tracker block
// GET/PUT /api/config/dashboard exchange. Only the toast knobs are editable
// here; the cache and webhook knobs stay YAML-only.
type dashboardStageTrackerPayload struct {
	ToastInfoS  *int `json:"toast_info_s,omitempty"`
	ToastErrorS *int `json:"toast_error_s,omitempty"`
}

// applyStageTrackerSettings validates and copies the toast knobs from a PUT
// body onto next. A value of 0 resets the knob to its default; anything else
// must sit within the shared 3..120 s bounds.
func applyStageTrackerSettings(next *config.DashboardConfig, body *dashboardStageTrackerPayload) error {
	if body == nil {
		return nil
	}
	check := func(name string, v *int) error {
		if v == nil || *v == 0 {
			return nil
		}
		if *v < config.MinStageTrackerToastS || *v > config.MaxStageTrackerToastS {
			return fmt.Errorf("dashboard.stage_tracker.%s must be between %d and %d seconds (0 resets to the default)",
				name, config.MinStageTrackerToastS, config.MaxStageTrackerToastS)
		}
		return nil
	}
	if err := check("toast_info_s", body.ToastInfoS); err != nil {
		return err
	}
	if err := check("toast_error_s", body.ToastErrorS); err != nil {
		return err
	}
	if body.ToastInfoS != nil {
		next.StageTracker.ToastInfoS = *body.ToastInfoS
	}
	if body.ToastErrorS != nil {
		next.StageTracker.ToastErrorS = *body.ToastErrorS
	}
	return nil
}
