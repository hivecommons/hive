package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// hubSettingsBody carries the scalar hub.* keys PUT /api/config/governor/hub
// accepts alongside its hub connection and contributor filter fields. Every
// field is a pointer so an absent key leaves the config untouched; 0 or ""
// restores the default. The two secrets are write-only: a masked echo is
// ignored and "" clears them. hub.cluster_id and hub.hive_type are identity
// keys written by hub provisioning and are display-only.
type hubSettingsBody struct {
	ContributeWallRetentionDays     *int    `json:"contribute_wall_retention_days"`
	ContributeAlreadyDoneLabel      *string `json:"contribute_already_done_label"`
	ContributeAlreadyDoneHoldDays   *int    `json:"contribute_already_done_hold_days"`
	ContributeRequireExplicitAccept *bool   `json:"contribute_require_explicit_accept"`
	NPSEnabled                      *bool   `json:"nps_enabled"`
	SnapshotIntervalMin             *int    `json:"snapshot_interval_min"`
	TaskStatusPush                  *bool   `json:"task_status_push"`
	UpgradeDrainTimeoutS            *int    `json:"upgrade_drain_timeout_s"`
	DiscordRequestsWebhookURL       *string `json:"notifications_discord_requests_webhook_url"`
	NPSRelayPullSecret              *string `json:"nps_relay_pull_secret"`
}

// prepare validates the carried keys with config.ValidateHubSettings (and
// the webhook URL with the notification URL rules) and returns the change to
// apply. ann is the announcement the same request carries, validated here so
// a bad expires_at is refused before any field is applied. Only the carried
// keys are validated, so a value already in hive.yaml that this editor would
// reject does not block saving an unrelated field.
func (b hubSettingsBody) prepare(ann *config.ContributeAnnouncement) (func(*config.HubConfig), error) {
	var probe config.HubConfig
	var steps []func(*config.HubConfig)
	if b.ContributeWallRetentionDays != nil {
		v := *b.ContributeWallRetentionDays
		probe.ContributeWallRetentionDays = v
		steps = append(steps, func(h *config.HubConfig) { h.ContributeWallRetentionDays = v })
	}
	if b.ContributeAlreadyDoneLabel != nil {
		v := strings.TrimSpace(*b.ContributeAlreadyDoneLabel)
		probe.ContributeAlreadyDoneLabel = v
		steps = append(steps, func(h *config.HubConfig) { h.ContributeAlreadyDoneLabel = v })
	}
	if b.ContributeAlreadyDoneHoldDays != nil {
		v := *b.ContributeAlreadyDoneHoldDays
		probe.ContributeAlreadyDoneHoldDays = v
		steps = append(steps, func(h *config.HubConfig) { h.ContributeAlreadyDoneHoldDays = v })
	}
	if b.ContributeRequireExplicitAccept != nil {
		v := *b.ContributeRequireExplicitAccept
		steps = append(steps, func(h *config.HubConfig) { h.ContributeRequireExplicitAccept = &v })
	}
	if b.NPSEnabled != nil {
		v := *b.NPSEnabled
		steps = append(steps, func(h *config.HubConfig) { h.NPSEnabled = &v })
	}
	if b.SnapshotIntervalMin != nil {
		v := *b.SnapshotIntervalMin
		probe.SnapshotIntervalMin = v
		steps = append(steps, func(h *config.HubConfig) { h.SnapshotIntervalMin = v })
	}
	if b.TaskStatusPush != nil {
		v := *b.TaskStatusPush
		steps = append(steps, func(h *config.HubConfig) { h.TaskStatusPush = &v })
	}
	if b.UpgradeDrainTimeoutS != nil {
		v := *b.UpgradeDrainTimeoutS
		probe.UpgradeDrainTimeoutS = v
		steps = append(steps, func(h *config.HubConfig) { h.UpgradeDrainTimeoutS = v })
	}
	if b.DiscordRequestsWebhookURL != nil {
		if err := validateNotificationURL(strings.TrimSpace(*b.DiscordRequestsWebhookURL), "notifications_discord_requests_webhook_url"); err != nil {
			return nil, err
		}
		v := b.DiscordRequestsWebhookURL
		steps = append(steps, func(h *config.HubConfig) { applyWriteOnly(&h.Notifications.Discord.RequestsWebhookURL, v) })
	}
	if b.NPSRelayPullSecret != nil {
		v := b.NPSRelayPullSecret
		steps = append(steps, func(h *config.HubConfig) { applyWriteOnly(&h.NPSRelayPullSecret, v) })
	}
	// An announcement with no text clears it, whatever else it carries.
	if ann != nil && sanitizeContributeAnnouncementText(ann.Text) != "" {
		probe.ContributeAnnouncement.ExpiresAt = ann.ExpiresAt
	}
	if err := config.ValidateHubSettings(probe); err != nil {
		return nil, err
	}
	return func(h *config.HubConfig) {
		for _, step := range steps {
			step(h)
		}
	}, nil
}

// hubSettingsResponse is the part of the GET /api/config/governor hub
// section that backs the scalar hub settings. Secrets are reported as set or
// not set, never echoed.
func (s *Server) hubSettingsResponse(cfg *config.Config) map[string]interface{} {
	h := cfg.Hub
	return map[string]interface{}{
		// Display-only identity keys, written by hub provisioning.
		"cluster_id":  h.ClusterID,
		"hive_type":   h.HiveType,
		"hub_managed": s.FleetReportHubManaged(),

		"contribute_already_done_label":     h.ContributeAlreadyDoneLabel,
		"contribute_already_done_hold_days": h.ContributeAlreadyDoneHoldDays,
		// Display-only deprecated aliases: the unverified hold days fall back
		// into contribute_already_done_hold_days, and close_already_done is no
		// longer read by the close path.
		"contribute_already_done_unverified_hold_days": h.ContributeAlreadyDoneUnverifiedHoldDays,
		"contribute_close_already_done":                h.IsContributeCloseAlreadyDone(),
		"contribute_require_explicit_accept":           h.IsContributeRequireExplicitAccept(),
		// The configured announcement (contribute_announcement is the active
		// one only, nil once it expires) so the editor can show it.
		"contribute_announcement_config": h.ContributeAnnouncement,
		"nps_enabled":                    h.NPSFeedbackEnabled(),
		"task_status_push":               h.TaskStatusPushEnabled(),
		"upgrade_drain_timeout_s":        h.UpgradeDrainTimeoutS,

		"notifications_discord_requests_webhook_set": strings.TrimSpace(h.Notifications.Discord.RequestsWebhookURL) != "",
		"nps_relay_pull_secret_set":                  strings.TrimSpace(h.NPSRelayPullSecret) != "",

		"defaults": map[string]interface{}{
			"contribute_already_done_label":     config.DefaultContributeAlreadyDoneLabel,
			"contribute_already_done_hold_days": config.DefaultContributeAlreadyDoneHoldDays,
			"contribute_wall_retention_days":    config.DefaultContributeWallRetentionDays,
			"snapshot_interval_min":             config.DefaultSnapshotIntervalMin,
			"upgrade_drain_timeout_s":           config.DefaultUpgradeDrainTimeoutS,
		},
	}
}
