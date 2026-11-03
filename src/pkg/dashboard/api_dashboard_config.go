package dashboard

import (
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type dashboardSettingsPayload struct {
	Port                int                          `json:"port"`
	AgentPollIntervalS  int                          `json:"agent_poll_interval_s"`
	ExperimentLab       bool                         `json:"experiment_lab"`
	StrategyLab         bool                         `json:"strategy_lab"`
	AuthTokenConfigured bool                         `json:"auth_token_configured"`
	AuthorizedUsers     []string                     `json:"authorized_users"`
	AuthorizedUserNames map[string]string            `json:"authorized_user_names"`
	HubProxied          bool                         `json:"hub_proxied"`
	IssueBands          dashboardIssueBandsPayload   `json:"issue_bands"`
	StageTracker        dashboardStageTrackerPayload `json:"stage_tracker"`
}

type dashboardIssueBandsPayload struct {
	WaitingLabels []string `json:"waiting_labels"`
	DoneLabels    []string `json:"done_labels"`
	StaleDays     int      `json:"stale_days"`
}

func dashboardSettingsResponse(cfg *config.Config) dashboardSettingsPayload {
	if cfg == nil {
		return dashboardSettingsPayload{AuthorizedUserNames: map[string]string{}}
	}
	d := cfg.Dashboard
	names := map[string]string{}
	for k, v := range d.AuthorizedUserNames {
		names[k] = v
	}
	toastInfo := int(cfg.StageTrackerToastInfo() / time.Second)
	toastError := int(cfg.StageTrackerToastError() / time.Second)
	return dashboardSettingsPayload{
		Port:                d.Port,
		AgentPollIntervalS:  d.AgentPollIntervalS,
		ExperimentLab:       d.ExperimentLab,
		StrategyLab:         d.StrategyLab,
		AuthTokenConfigured: strings.TrimSpace(d.AuthToken) != "",
		AuthorizedUsers:     append([]string{}, d.AuthorizedUsers...),
		AuthorizedUserNames: names,
		HubProxied:          d.HubProxied,
		IssueBands: dashboardIssueBandsPayload{
			WaitingLabels: append([]string{}, d.IssueBands.WaitingLabels...),
			DoneLabels:    append([]string{}, d.IssueBands.DoneLabels...),
			StaleDays:     d.IssueBands.StaleDays,
		},
		StageTracker: dashboardStageTrackerPayload{ToastInfoS: &toastInfo, ToastErrorS: &toastError},
	}
}

func (s *Server) handleDashboardSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	jsonResponse(w, dashboardSettingsResponse(s.deps.Config))
}

func (s *Server) handleDashboardSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Port                *int                          `json:"port"`
		AgentPollIntervalS  *int                          `json:"agent_poll_interval_s"`
		ExperimentLab       *bool                         `json:"experiment_lab"`
		StrategyLab         *bool                         `json:"strategy_lab"`
		AuthToken           *string                       `json:"auth_token"`
		AuthorizedUsers     []string                      `json:"authorized_users"`
		AuthorizedUserNames map[string]string             `json:"authorized_user_names"`
		HubProxied          *bool                         `json:"hub_proxied"`
		IssueBands          *dashboardIssueBandsPayload   `json:"issue_bands"`
		StageTracker        *dashboardStageTrackerPayload `json:"stage_tracker"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if cfg == nil {
		jsonError(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	next := cfg.Dashboard
	if body.Port != nil {
		if *body.Port < 1 || *body.Port > 65535 {
			jsonError(w, "dashboard.port must be between 1 and 65535", http.StatusBadRequest)
			return
		}
		next.Port = *body.Port
	}
	if body.AgentPollIntervalS != nil {
		if *body.AgentPollIntervalS < 1 {
			jsonError(w, "dashboard.agent_poll_interval_s must be positive", http.StatusBadRequest)
			return
		}
		next.AgentPollIntervalS = *body.AgentPollIntervalS
	}
	if body.ExperimentLab != nil {
		next.ExperimentLab = *body.ExperimentLab
	}
	if body.StrategyLab != nil {
		next.StrategyLab = *body.StrategyLab
	}
	if body.AuthToken != nil {
		next.AuthToken = strings.TrimSpace(*body.AuthToken)
	}
	if body.AuthorizedUsers != nil {
		next.AuthorizedUsers = trimNonEmpty(body.AuthorizedUsers)
	}
	if body.AuthorizedUserNames != nil {
		names := map[string]string{}
		for k, v := range body.AuthorizedUserNames {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k != "" && v != "" {
				names[k] = v
			}
		}
		next.AuthorizedUserNames = names
	}
	if body.HubProxied != nil {
		next.HubProxied = *body.HubProxied
	}
	if body.IssueBands != nil {
		next.IssueBands.WaitingLabels = trimNonEmpty(body.IssueBands.WaitingLabels)
		next.IssueBands.DoneLabels = trimNonEmpty(body.IssueBands.DoneLabels)
		if body.IssueBands.StaleDays < 0 {
			jsonError(w, "dashboard.issue_bands.stale_days must be zero or greater", http.StatusBadRequest)
			return
		}
		next.IssueBands.StaleDays = body.IssueBands.StaleDays
	}
	if err := applyStageTrackerSettings(&next, body.StageTracker); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg.Dashboard = next
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after dashboard settings update", "error", err)
		jsonError(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_dashboard_settings", auditDetail("section", "dashboard"), "")
	if body.StageTracker != nil {
		// The tracker payload carries the toast knobs; drop the memoised
		// body so the next poll sees the new durations.
		s.invalidateStageTrackerCache()
	}
	jsonResponse(w, dashboardSettingsResponse(cfg))
}
