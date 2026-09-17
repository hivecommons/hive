package config

import (
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestHealthConfigCarriesNoDeadKnobs is the guard #7251 asks for.
//
// governor.health.healthcheck_interval and governor.health.restart_cooldown
// were fully plumbed — parsed, defaulted, validated, persisted, and rendered
// in Settings — while NO runtime loop read either one. An operator lowering
// "Healthcheck Interval" to 60s to get faster detection got no change
// whatsoever, and the cadence that actually governs liveness sweeps lived
// further down the same tab under a different name.
//
// The allowlist is deliberately explicit rather than a count: a new field here
// must be justified in this test, which forces whoever adds it to say where it
// is consumed. That is the whole point — the failure mode was not "too many
// fields", it was "a field nothing reads".
func TestHealthConfigCarriesNoDeadKnobs(t *testing.T) {
	allowed := map[string]string{
		"ModelLock": "read by the governor's budget-pressure model downgrade path",
		"DeprecatedHealthcheckInterval": "migration-only: carries an explicitly set legacy value into " +
			"governor.watchdog.probe_interval_s, then clears itself",
	}

	var got []string
	ty := reflect.TypeOf(HealthConfig{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}
	sort.Strings(got)

	for _, name := range got {
		if _, ok := allowed[name]; !ok {
			t.Errorf("HealthConfig has an unexpected field %q.\n"+
				"Every field here must be READ by something at runtime — #7251 removed two that were not, "+
				"after operators tuned them for no effect. If %q is genuinely consumed, add it to this "+
				"allowlist with a note saying where.", name, name)
		}
	}

	for name := range allowed {
		found := false
		for _, g := range got {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allowlist mentions %q but HealthConfig no longer has it; prune this test", name)
		}
	}
}

// TestDeprecatedHealthcheckIntervalMigratesToWatchdog pins the back-compat
// path. An operator who deliberately tuned the old field had an intent — a
// probe cadence — and the watchdog probe interval is the knob that field
// claimed to be, so the intent is carried forward rather than silently lost.
func TestDeprecatedHealthcheckIntervalMigratesToWatchdog(t *testing.T) {
	c := &Config{}
	c.Governor.Health.DeprecatedHealthcheckInterval = 60
	c.migrateDeprecatedHealthSettings()

	if got := c.Governor.Watchdog.ProbeIntervalS; got != 60 {
		t.Errorf("watchdog ProbeIntervalS = %d, want 60 migrated from the legacy field", got)
	}
	if got := c.Governor.Health.DeprecatedHealthcheckInterval; got != 0 {
		t.Errorf("legacy field = %d, want it cleared so it cannot survive a config re-save", got)
	}
}

// TestExplicitWatchdogProbeIntervalWinsOverLegacy — the current, supported knob
// must never be clobbered by a stale one the operator may not even remember
// setting.
func TestExplicitWatchdogProbeIntervalWinsOverLegacy(t *testing.T) {
	c := &Config{}
	c.Governor.Health.DeprecatedHealthcheckInterval = 60
	c.Governor.Watchdog.ProbeIntervalS = 900
	c.migrateDeprecatedHealthSettings()

	if got := c.Governor.Watchdog.ProbeIntervalS; got != 900 {
		t.Errorf("ProbeIntervalS = %d, want the explicit 900 to survive migration", got)
	}
	if got := c.Governor.Health.DeprecatedHealthcheckInterval; got != 0 {
		t.Errorf("legacy field = %d, want it cleared even when not adopted", got)
	}
}

// TestMigrationIsIdempotent — applyDefaults runs again on every config reload,
// so a second pass must not resurrect or re-apply anything.
func TestMigrationIsIdempotent(t *testing.T) {
	c := &Config{}
	c.Governor.Health.DeprecatedHealthcheckInterval = 60
	c.migrateDeprecatedHealthSettings()
	c.Governor.Watchdog.ProbeIntervalS = 120 // operator changes it afterwards
	c.migrateDeprecatedHealthSettings()

	if got := c.Governor.Watchdog.ProbeIntervalS; got != 120 {
		t.Errorf("ProbeIntervalS = %d after a second migration pass, want 120 — "+
			"the migration must not re-apply on config reload", got)
	}
}

// TestMigrationIgnoresUnsetAndNegativeLegacyValues — an absent key must not
// write anything into the watchdog. This matters because
// pkg/config/watchdog.go deliberately materializes NO defaults: a zero there
// means "use the RFC default, resolved at consumption time", and writing a
// concrete value would freeze it across upgrades (#4041).
func TestMigrationIgnoresUnsetAndNegativeLegacyValues(t *testing.T) {
	for _, legacy := range []int{0, -1} {
		c := &Config{}
		c.Governor.Health.DeprecatedHealthcheckInterval = legacy
		c.migrateDeprecatedHealthSettings()
		if got := c.Governor.Watchdog.ProbeIntervalS; got != 0 {
			t.Errorf("legacy %d wrote ProbeIntervalS = %d, want 0 left untouched so the "+
				"watchdog keeps tracking its own default", legacy, got)
		}
	}
}

// TestLegacyHealthKeysStillParseWithoutError is the safety property that makes
// this removal non-breaking: config is decoded non-strictly, so a hive.yaml
// still carrying the old keys loads fine. restart_cooldown in particular is
// dropped outright with no alias, and that must not become a load failure.
func TestLegacyHealthKeysStillParseWithoutError(t *testing.T) {
	raw := []byte(`
governor:
  health:
    healthcheck_interval: 120
    restart_cooldown: 45
    model_lock: true
`)
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("a config carrying the removed keys failed to parse: %v", err)
	}
	cfg.applyDefaults()
	if !cfg.Governor.Health.ModelLock {
		t.Error("model_lock did not survive alongside the removed keys")
	}
	if got := cfg.Governor.Watchdog.ProbeIntervalS; got != 120 {
		t.Errorf("ProbeIntervalS = %d, want 120 migrated from healthcheck_interval", got)
	}
}
