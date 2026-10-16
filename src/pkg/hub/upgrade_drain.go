package hub

import "time"

// upgradeDrainActive reports whether a spoke-reported drain deadline is still
// in the future, i.e. the spoke is knowingly deferring its self-upgrade.
func upgradeDrainActive(until, now time.Time) bool {
	return !until.IsZero() && now.Before(until)
}

// parseUpgradeDrainUntil parses the heartbeat's drain deadline. Empty or
// malformed input (a spoke that is not draining, or is too old to report it)
// yields the zero time, and the deadline is capped at maxUpgradeDrainDeadline.
func parseUpgradeDrainUntil(raw string, now time.Time) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	if limit := now.Add(maxUpgradeDrainDeadline); t.After(limit) {
		return limit
	}
	return t
}
