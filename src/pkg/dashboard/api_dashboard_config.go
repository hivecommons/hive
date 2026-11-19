package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type dashboardSettingsPayload struct {
	Port                    int                          `json:"port"`
	AgentPollIntervalS      int                          `json:"agent_poll_interval_s"`
	ExperimentLab           bool                         `json:"experiment_lab"`
	StrategyLab             bool                         `json:"strategy_lab"`
	SubscriptionGaugeHeader bool                         `json:"subscription_gauge_header"`
	AuthTokenConfigured     bool                         `json:"auth_token_configured"`
	AuthorizedUsers         []string                     `json:"authorized_users"`
	AuthorizedUserNames     map[string]string            `json:"authorized_user_names"`
	HubProxied              bool                         `json:"hub_proxied"`
	IssueBands              dashboardIssueBandsPayload   `json:"issue_bands"`
	StageTracker            dashboardStageTrackerPayload `json:"stage_tracker"`
	Sessions                dashboardSessionsPayload     `json:"sessions"`
}

type dashboardIssueBandsPayload struct {
	WaitingLabels []string `json:"waiting_labels"`
	DoneLabels    []string `json:"done_labels"`
	StaleDays     int      `json:"stale_days"`
}

// dashboardSessionsPayload is dashboard.sessions as the Settings dialog reads
// and writes it. The limits are the CONFIGURED values (0 = default), not the
// effective ones, so a saved 0 keeps following the default; EnvOverride tells
// the dialog when HIVE_SESSIONS_ENABLED is deciding instead of the toggle.
type dashboardSessionsPayload struct {
	Enabled      *bool `json:"enabled"`
	MaxSessions  *int  `json:"max_sessions"`
	MaxPerUser   *int  `json:"max_per_user"`
	IdleTTLHours *int  `json:"idle_ttl_hours"`
	// Autostart is dashboard.sessions.autostart (the SAVED value; an unset key
	// reads as its default, on). HIVE_SESSIONS_AUTOSTART overrides it.
	Autostart *bool `json:"autostart"`
	// EnvOverride is response-only: true when HIVE_SESSIONS_ENABLED holds a
	// recognised boolean, so the saved toggle is not what decides.
	EnvOverride bool `json:"env_override,omitempty"`
}

func dashboardSessionsResponse(cfg *config.Config) dashboardSessionsPayload {
	s := cfg.Dashboard.Sessions
	enabled, maxSessions, maxPerUser, idle := s.EnabledOrDefault(), s.MaxSessions, s.MaxPerUser, s.IdleTTLHours
	autostart := s.AutostartOrDefault()
	return dashboardSessionsPayload{
		Autostart:    &autostart,
		Enabled:      &enabled,
		MaxSessions:  &maxSessions,
		MaxPerUser:   &maxPerUser,
		IdleTTLHours: &idle,
		// HIVE_SESSIONS_ENABLED is the only thing that can make the effective
		// value differ from the saved one (config.SessionsEnabled).
		EnvOverride: cfg.SessionsEnabled() != enabled,
	}
}

// applyDashboardSessionsSettings copies the fields present in body onto next.
// A limit of 0 resets it to the built-in default; negatives are refused, as
// config validation would.
func applyDashboardSessionsSettings(next *config.DashboardConfig, body *dashboardSessionsPayload) error {
	if body == nil {
		return nil
	}
	for name, v := range map[string]*int{
		"max_sessions":   body.MaxSessions,
		"max_per_user":   body.MaxPerUser,
		"idle_ttl_hours": body.IdleTTLHours,
	} {
		if v != nil && *v < 0 {
			return fmt.Errorf("dashboard.sessions.%s must be zero or greater (0 uses the default)", name)
		}
	}
	if body.Enabled != nil {
		enabled := *body.Enabled
		next.Sessions.Enabled = &enabled
	}
	if body.Autostart != nil {
		autostart := *body.Autostart
		next.Sessions.Autostart = &autostart
	}
	if body.MaxSessions != nil {
		next.Sessions.MaxSessions = *body.MaxSessions
	}
	if body.MaxPerUser != nil {
		next.Sessions.MaxPerUser = *body.MaxPerUser
	}
	if body.IdleTTLHours != nil {
		next.Sessions.IdleTTLHours = *body.IdleTTLHours
	}
	return nil
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
		Port:                    d.Port,
		AgentPollIntervalS:      d.AgentPollIntervalS,
		ExperimentLab:           d.ExperimentLab,
		StrategyLab:             d.StrategyLab,
		SubscriptionGaugeHeader: d.SubscriptionGaugeHeader,
		AuthTokenConfigured:     strings.TrimSpace(d.AuthToken) != "",
		AuthorizedUsers:         append([]string{}, d.AuthorizedUsers...),
		AuthorizedUserNames:     names,
		HubProxied:              d.HubProxied,
		IssueBands: dashboardIssueBandsPayload{
			WaitingLabels: append([]string{}, d.IssueBands.WaitingLabels...),
			DoneLabels:    append([]string{}, d.IssueBands.DoneLabels...),
			StaleDays:     d.IssueBands.StaleDays,
		},
		StageTracker: dashboardStageTrackerPayload{ToastInfoS: &toastInfo, ToastErrorS: &toastError},
		Sessions:     dashboardSessionsResponse(cfg),
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
		Port                    *int                          `json:"port"`
		AgentPollIntervalS      *int                          `json:"agent_poll_interval_s"`
		ExperimentLab           *bool                         `json:"experiment_lab"`
		StrategyLab             *bool                         `json:"strategy_lab"`
		SubscriptionGaugeHeader *bool                         `json:"subscription_gauge_header"`
		AuthToken               *string                       `json:"auth_token"`
		AuthorizedUsers         []string                      `json:"authorized_users"`
		AuthorizedUserNames     map[string]string             `json:"authorized_user_names"`
		HubProxied              *bool                         `json:"hub_proxied"`
		IssueBands              *dashboardIssueBandsPayload   `json:"issue_bands"`
		StageTracker            *dashboardStageTrackerPayload `json:"stage_tracker"`
		Sessions                *dashboardSessionsPayload     `json:"sessions"`
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
	if body.SubscriptionGaugeHeader != nil {
		next.SubscriptionGaugeHeader = *body.SubscriptionGaugeHeader
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
	if err := applyDashboardSessionsSettings(&next, body.Sessions); err != nil {
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
