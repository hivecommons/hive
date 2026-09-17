package config

import "log"

// deprecatedHealthcheckWarned ensures the deprecation notice is logged once per
// process rather than on every applyDefaults call (config is re-applied on
// reload, and a warning per reload would be noise, not signal).
var deprecatedHealthcheckWarned bool

// migrateDeprecatedHealthSettings carries an operator's explicitly configured
// governor.health.healthcheck_interval forward into the watchdog probe
// interval, then clears it (#7251).
//
// governor.health.healthcheck_interval and governor.health.restart_cooldown
// were dead knobs: fully plumbed through config, validation, persistence and
// the Settings UI, but read by no runtime loop. The watchdog (RFC #4665) is
// the liveness system that actually runs, and its probe interval is the knob
// the old field CLAIMED to be. So an operator who deliberately tuned it gets
// that intent honoured instead of silently dropped.
//
// restart_cooldown is NOT migrated. It has no watchdog equivalent worth
// aliasing: the watchdog supersedes it with a backoff ladder
// (1m -> 2m -> 4m -> 8m -> 16m, reset after a healthy window), and mapping a
// single cooldown onto a ladder would invent a policy the operator never
// chose. The YAML key is simply ignored, which is safe because config is
// decoded non-strictly.
//
// Note on the watchdog's no-defaults rule: pkg/config/watchdog.go deliberately
// materializes NO defaults into WatchdogConfig, because #4041 showed that
// marshaling defaults back into a saved config freezes them forever. This
// migration does not violate that. It only ever copies a value the operator
// EXPLICITLY set, and only when probe_interval_s is unset — it never writes a
// default. A hive that simply inherited the old 300s default ends up writing
// 300, which is exactly the watchdog's own default, so the result is a no-op.
func (c *Config) migrateDeprecatedHealthSettings() {
	legacy := c.Governor.Health.DeprecatedHealthcheckInterval
	if legacy <= 0 {
		return
	}

	// Clear unconditionally: whether or not it is adopted below, the field is
	// gone from the surface and must not survive a re-save.
	c.Governor.Health.DeprecatedHealthcheckInterval = 0

	// An explicit watchdog setting always wins — it is the current, supported
	// knob, and the operator who set it meant it.
	if c.Governor.Watchdog.ProbeIntervalS > 0 {
		return
	}
	c.Governor.Watchdog.ProbeIntervalS = legacy

	if !deprecatedHealthcheckWarned {
		deprecatedHealthcheckWarned = true
		log.Printf("[config] governor.health.healthcheck_interval is deprecated and no longer read; "+
			"migrated its value (%ds) to governor.watchdog.probe_interval_s. "+
			"governor.health.restart_cooldown is also removed — the watchdog restart backoff ladder supersedes it. See #7251.", legacy)
	}
}
