package hub

import (
	"sync"
	"time"

	// tzdata embeds the IANA timezone database into the binary. The hub image is
	// built FROM alpine:3.21, which does NOT install the tzdata package (see
	// Dockerfile.hub — only ca-certificates and curl are added), so
	// time.LoadLocation("America/New_York") would fail at runtime with
	// "unknown time zone" and the daily schedule would never fire. Embedding
	// costs ~450KB in the binary and removes the dependency on the base image.
	_ "time/tzdata"
)

// Auto-upgrade scheduling modes. AutoUpgrade (bool) remains the on/off master
// switch; the mode only decides WHEN an enabled auto-upgrade is allowed to fire.
//
// The product intent is disruption minimisation: a stable, working hive should
// not be restarted at an arbitrary moment just because a new commit landed.
// "daily" confines the restart to one predictable after-hours moment per day.
const (
	// AutoUpgradeModeInstant upgrades as soon as the hive is seen behind latest.
	// This is the historical behaviour and the meaning of an EMPTY mode, so the
	// ~42 existing PVC records that carry only `auto_upgrade: true` keep
	// behaving exactly as they do today.
	AutoUpgradeModeInstant = "instant"

	// AutoUpgradeModeDaily upgrades at most once per calendar day, during the
	// first poll cycle at or after autoUpgradeDailyHour local time in
	// autoUpgradeTimezone.
	AutoUpgradeModeDaily = "daily"
)

// autoUpgradeDailyHour is the hour (24h clock, in autoUpgradeTimezone) at or
// after which a "daily" hive becomes eligible to upgrade. 17 = 5pm ET, chosen
// as an after-hours moment for the maintainers this hub serves.
const autoUpgradeDailyHour = 17

// autoUpgradeTimezone is the IANA zone the daily window is expressed in. It is
// deliberately a ZONE NAME and not a fixed UTC offset: America/New_York is
// UTC-5 (EST) for part of the year and UTC-4 (EDT) for the rest, so a
// hardcoded offset would fire an hour early or late for roughly half of every
// year and would drift across the DST transitions in March and November.
const autoUpgradeTimezone = "America/New_York"

// autoUpgradeDateFormat is the calendar-day key stored per hive. Day identity
// is evaluated in autoUpgradeTimezone, not UTC, so "at most once per day" means
// once per ET day as the operator experiences it.
const autoUpgradeDateFormat = "2006-01-02"

// validAutoUpgradeModes is the allowlist the API validates against. Unknown
// values are rejected with 400 rather than silently falling back, so a typo in
// an API client never quietly changes a hive's upgrade cadence.
var validAutoUpgradeModes = map[string]bool{
	AutoUpgradeModeInstant: true,
	AutoUpgradeModeDaily:   true,
}

// isValidAutoUpgradeMode reports whether mode is acceptable on the API. The
// empty string is allowed and means "instant" (see normalizeAutoUpgradeMode) so
// that older clients which send only auto_upgrade keep working unchanged.
func isValidAutoUpgradeMode(mode string) bool {
	return mode == "" || validAutoUpgradeModes[mode]
}

// normalizeAutoUpgradeMode maps a stored/legacy value onto a concrete mode.
// Empty (a legacy record, or a client that never sent the field) is instant.
func normalizeAutoUpgradeMode(mode string) string {
	if mode == AutoUpgradeModeDaily {
		return AutoUpgradeModeDaily
	}
	return AutoUpgradeModeInstant
}

// autoUpgradeLocation resolves autoUpgradeTimezone once and caches the result.
// If the zone cannot be loaded the error is cached too, and callers fail SAFE
// by treating the hive as instant: a hive that upgrades sooner than the
// operator asked is a far better failure than one that silently never upgrades
// again. With the embedded tzdata import above this path should be
// unreachable, but it is kept because failing closed here would be invisible.
var (
	autoUpgradeLocOnce sync.Once
	autoUpgradeLoc     *time.Location
	autoUpgradeLocErr  error
)

func autoUpgradeLocation() (*time.Location, error) {
	autoUpgradeLocOnce.Do(func() {
		autoUpgradeLoc, autoUpgradeLocErr = time.LoadLocation(autoUpgradeTimezone)
	})
	return autoUpgradeLoc, autoUpgradeLocErr
}

// autoUpgradeDecision is the result of evaluating one hive's schedule for one
// poll cycle. FireDate is the calendar-day key to persist once the upgrade has
// actually been triggered; it is empty when Allowed is false.
type autoUpgradeDecision struct {
	Allowed bool
	// FireDate is the ET calendar day the upgrade is being attributed to.
	// Persisting it is what makes a 17:02 cycle and a 17:04 cycle collapse
	// into a single upgrade for the day.
	FireDate string
	// Reason is a short, log-friendly explanation. Always populated.
	Reason string
}

// shouldAutoUpgradeNow decides whether an auto-upgrade-enabled hive may fire on
// this poll cycle.
//
// mode is the hive's stored AutoUpgradeMode (empty = instant, i.e. legacy).
// lastFiredDate is the hive's stored AutoUpgradeLastFired, an ET calendar day
// in autoUpgradeDateFormat, or empty if it has never fired on a schedule.
// now is injected so the behaviour is testable without touching the wall clock.
//
// Missed windows deliberately do NOT accumulate. If the hub is down across
// 17:00 and comes back at 21:30, the hive upgrades on the next cycle it sees
// (it is still behind, and the day's window has passed) and then resumes the
// normal daily cadence. The alternative — waiting a further ~20 hours for the
// next 17:00 — would leave a hive knowingly behind for a full extra day, which
// defeats the point of having auto-upgrade enabled at all. Only ONE upgrade is
// ever owed, because the gate is "have we fired today", not a queue of windows.
func shouldAutoUpgradeNow(mode, lastFiredDate string, now time.Time) autoUpgradeDecision {
	if normalizeAutoUpgradeMode(mode) == AutoUpgradeModeInstant {
		// Instant hives are ungated and never record a fire date.
		return autoUpgradeDecision{Allowed: true, Reason: "instant mode"}
	}

	loc, err := autoUpgradeLocation()
	if err != nil {
		// Fail safe: behave as instant rather than never upgrading.
		return autoUpgradeDecision{Allowed: true, Reason: "timezone unavailable — falling back to instant"}
	}

	local := now.In(loc)
	today := local.Format(autoUpgradeDateFormat)

	if lastFiredDate == today {
		return autoUpgradeDecision{Reason: "already upgraded today"}
	}
	if local.Hour() < autoUpgradeDailyHour {
		return autoUpgradeDecision{Reason: "before the daily window"}
	}
	return autoUpgradeDecision{Allowed: true, FireDate: today, Reason: "daily window open"}
}
