package config

import (
	"fmt"
	"strings"
	"time"
)

// Hub defaults the runtime resolves when the matching hub.* key is 0 or
// empty; runtime editors substitute the same values so a cleared field
// restores the default.
const (
	DefaultContributeAlreadyDoneLabel    = "hive/already-done"
	DefaultContributeAlreadyDoneHoldDays = 30
	DefaultContributeWallRetentionDays   = 90
	DefaultSnapshotIntervalMin           = 15
	DefaultUpgradeDrainTimeoutS          = 45 * 60

	// MaxContributeWallRetentionDays is the clamp applyDefaults uses.
	MaxContributeWallRetentionDays = 3650
	// MinSnapshotIntervalMin is the shortest snapshot rebuild interval; a
	// smaller positive value is treated as the default.
	MinSnapshotIntervalMin = 5
	// MaxSnapshotIntervalMin caps the editor at one day.
	MaxSnapshotIntervalMin = 24 * 60
	// MaxUpgradeDrainTimeoutS caps the editor at one day.
	MaxUpgradeDrainTimeoutS = 24 * 60 * 60
	// MaxContributeAlreadyDoneLabelLen is GitHub's label name limit.
	MaxContributeAlreadyDoneLabelLen = 50
)

// ValidateHubSettings checks the scalar hub keys the dashboard edits:
// contribute_wall_retention_days in [0, 3650], contribute_already_done_hold_days
// in [0, 365], a contribute_already_done_label no longer than a GitHub label,
// snapshot_interval_min of 0 or in [5, 1440], upgrade_drain_timeout_s at most
// one day (negative disables draining), and an announcement expires_at that is
// RFC3339. Unset (zero) fields pass, so callers can validate just the keys
// they edit.
func ValidateHubSettings(h HubConfig) error {
	if d := h.ContributeWallRetentionDays; d < 0 || d > MaxContributeWallRetentionDays {
		return fmt.Errorf("hub.contribute_wall_retention_days must be between 0 and %d", MaxContributeWallRetentionDays)
	}
	if d := h.ContributeAlreadyDoneHoldDays; d < 0 || d > contributeAlreadyDoneMaxDays {
		return fmt.Errorf("hub.contribute_already_done_hold_days must be between 0 and %d", contributeAlreadyDoneMaxDays)
	}
	if l := strings.TrimSpace(h.ContributeAlreadyDoneLabel); len([]rune(l)) > MaxContributeAlreadyDoneLabelLen {
		return fmt.Errorf("hub.contribute_already_done_label must be at most %d characters", MaxContributeAlreadyDoneLabelLen)
	}
	if m := h.SnapshotIntervalMin; m != 0 && (m < MinSnapshotIntervalMin || m > MaxSnapshotIntervalMin) {
		return fmt.Errorf("hub.snapshot_interval_min must be 0 (default) or between %d and %d", MinSnapshotIntervalMin, MaxSnapshotIntervalMin)
	}
	if h.UpgradeDrainTimeoutS > MaxUpgradeDrainTimeoutS {
		return fmt.Errorf("hub.upgrade_drain_timeout_s must be at most %d (negative disables draining)", MaxUpgradeDrainTimeoutS)
	}
	if e := strings.TrimSpace(h.ContributeAnnouncement.ExpiresAt); e != "" {
		if _, err := time.Parse(time.RFC3339, e); err != nil {
			return fmt.Errorf("hub.contribute_announcement.expires_at must be RFC3339")
		}
	}
	return nil
}
