package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// #4820: governor.advisory.update_interval_s throttles the advisory digest's
// posting cadence. These tests pin the resolver contract — 0/unset means
// "every eval cycle" (today's behavior, NOT a materialized 60), the floor
// clamps up instead of erroring, and hive.yaml round-trips the field.

// TestAdvisoryUpdateInterval_UnsetMeansDefaultCadence pins invariant 1 of the
// issue: an untouched install resolves to a zero interval (gate always open),
// and applyDefaults does NOT materialize a number into the field — "every
// eval cycle" must keep tracking governor.eval_interval_s.
func TestAdvisoryUpdateInterval_UnsetMeansDefaultCadence(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()

	if cfg.Governor.Advisory.UpdateIntervalS != 0 {
		t.Errorf("applyDefaults materialized update_interval_s = %d, want 0 (unset)",
			cfg.Governor.Advisory.UpdateIntervalS)
	}
	d, clamped := cfg.Governor.Advisory.EffectiveUpdateInterval()
	if d != 0 || clamped {
		t.Errorf("unset: EffectiveUpdateInterval() = (%v, %v), want (0, false)", d, clamped)
	}
}

// TestAdvisoryUpdateInterval_ClampsToFloor pins the server-side clamp: a
// configured sub-floor value (hand-edited hive.yaml included) resolves to the
// 30s floor and reports that it was clamped, so the caller can log it once.
func TestAdvisoryUpdateInterval_ClampsToFloor(t *testing.T) {
	a := AdvisoryConfig{UpdateIntervalS: 10}
	d, clamped := a.EffectiveUpdateInterval()
	if d != MinAdvisoryUpdateIntervalS*time.Second {
		t.Errorf("interval 10: EffectiveUpdateInterval() = %v, want %v", d, MinAdvisoryUpdateIntervalS*time.Second)
	}
	if !clamped {
		t.Error("interval 10: clamped = false, want true")
	}
}

// TestAdvisoryUpdateInterval_HonorsConfiguredValue pins the plain case: a
// value at or above the floor is used as-is, unclamped.
func TestAdvisoryUpdateInterval_HonorsConfiguredValue(t *testing.T) {
	a := AdvisoryConfig{UpdateIntervalS: 300}
	d, clamped := a.EffectiveUpdateInterval()
	if d != 300*time.Second || clamped {
		t.Errorf("interval 300: EffectiveUpdateInterval() = (%v, %v), want (5m0s, false)", d, clamped)
	}
}

// TestAdvisoryUpdateInterval_YAMLRoundTrip pins the hive.yaml contract: the
// field serializes under governor.advisory.update_interval_s, survives a
// marshal/unmarshal cycle, and is OMITTED when unset so existing configs do
// not grow a spurious key on the next save.
func TestAdvisoryUpdateInterval_YAMLRoundTrip(t *testing.T) {
	in := AdvisoryConfig{MaxFindings: 10, StalenessDays: 7, UpdateIntervalS: 300}
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out AdvisoryConfig
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.UpdateIntervalS != 300 {
		t.Errorf("round-tripped update_interval_s = %d, want 300\nyaml:\n%s", out.UpdateIntervalS, raw)
	}

	unsetRaw, err := yaml.Marshal(AdvisoryConfig{MaxFindings: 10, StalenessDays: 7})
	if err != nil {
		t.Fatalf("marshal unset: %v", err)
	}
	if string(unsetRaw) != "" && yamlContainsKey(unsetRaw, "update_interval_s") {
		t.Errorf("unset field must be omitted from yaml, got:\n%s", unsetRaw)
	}
}

func yamlContainsKey(raw []byte, key string) bool {
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
