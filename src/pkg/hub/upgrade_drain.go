package hub

import "time"

// upgradeDrainActive reports whether a spoke-reported drain deadline is still
// in the future, i.e. the spoke is knowingly deferring its self-upgrade.
func upgradeDrainActive(until, now time.Time) bool {
	return !until.IsZero() && now.Before(until)
}

// parseUpgradeDrainUntil parses the heartbeat's drain deadline. Empty or
// malformed input (a spoke that is not draining, or is too old to report it)
// yields the zero time.
//
// The deadline is capped at maxUpgradeDrainDeadline past the upgrade's START
// (startedAt), not past now: a cap re-based on every heartbeat could be renewed
// forever by a spoke that keeps reporting a far-future deadline, muting stuck
// and orphan detection without bound. Anchoring to the start clock bounds the
// total mute window per upgrade. A zero startedAt (no hub-armed clock yet)
// falls back to now.
func parseUpgradeDrainUntil(raw string, startedAt, now time.Time) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	anchor := now
	if !startedAt.IsZero() && startedAt.Before(now) {
		anchor = startedAt
	}
	if limit := anchor.Add(maxUpgradeDrainDeadline); t.After(limit) {
		return limit
	}
	return t
}
