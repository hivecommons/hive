package config

import "strings"

// Values for github.notifications.mode.
const (
	// NotificationsModeQuiet (the default) folds the hive's own PR
	// bookkeeping — stage stalled/regressed notes, the trusted-human 071
	// note, the duplicate-PR redirect — into one status comment per PR that
	// is edited in place, posts no "no findings" review, and @-mentions a
	// person once per cause rather than once per head.
	NotificationsModeQuiet = "quiet"
	// NotificationsModeNormal posts every note as its own comment and every
	// perspective's verdict as its own review, as the hive did before the
	// knob existed.
	NotificationsModeNormal = "normal"
)

// GitHubNotificationsConfig is github.notifications: how much of the hive's
// PR bookkeeping becomes an email. Every new issue comment and every review
// submission notifies the author and every watcher of the repository;
// editing an existing comment or review notifies nobody. The Activity
// Monitor, the evidence bundles and the stage labels carry the same facts,
// so the quiet default loses no information.
type GitHubNotificationsConfig struct {
	// Mode is "quiet" (default when empty) or "normal".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// Quiet reports whether the hive should fold its PR notes into one edited
// status comment and suppress no-findings reviews. Empty reads as quiet.
func (c GitHubNotificationsConfig) Quiet() bool {
	return !strings.EqualFold(strings.TrimSpace(c.Mode), NotificationsModeNormal)
}

// Valid reports whether Mode names a known mode (or is unset).
func (c GitHubNotificationsConfig) Valid() bool {
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case "", NotificationsModeQuiet, NotificationsModeNormal:
		return true
	}
	return false
}
