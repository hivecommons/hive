package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestAdvisoryConfigDefaults pins the shipped advisory behavior for a hive that
// says nothing: a ten-finding digest, a week-long staleness window, and
// PR-linked auto-close on.
func TestAdvisoryConfigDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()

	a := cfg.Governor.Advisory
	if a.MaxFindings != defaultAdvisoryMaxFindings {
		t.Errorf("MaxFindings = %d, want %d", a.MaxFindings, defaultAdvisoryMaxFindings)
	}
	if a.StalenessDays != defaultAdvisoryStalenessDays {
		t.Errorf("StalenessDays = %d, want %d", a.StalenessDays, defaultAdvisoryStalenessDays)
	}
	if a.PRAutoClose == nil || !*a.PRAutoClose {
		t.Errorf("PRAutoClose = %v, want an explicit true", a.PRAutoClose)
	}
	if !a.PRAutoCloseEnabled() {
		t.Error("PRAutoCloseEnabled() = false, want true by default")
	}
	if a.ShowAll {
		t.Error("ShowAll = true, want false — the cap is the default, opting out of it is not")
	}
	if a.UpdateIntervalS != 0 {
		t.Errorf("UpdateIntervalS = %d, want 0 to preserve the unset sentinel", a.UpdateIntervalS)
	}
	if got := a.AdvisoryUpdateInterval(); got != 60*time.Second {
		t.Errorf("AdvisoryUpdateInterval() = %v, want 60s", got)
	}
}

// TestAdvisoryConfigDefaultsPreserveExplicitValues confirms defaulting never
// overwrites an operator's choice, including an explicit pr_autoclose: false
// (the case a plain bool would silently lose).
func TestAdvisoryConfigDefaultsPreserveExplicitValues(t *testing.T) {
	off := false
	cfg := &Config{}
	cfg.Governor.Advisory = AdvisoryConfig{
		MaxFindings:     25,
		ShowAll:         true,
		StalenessDays:   3,
		UpdateIntervalS: 300,
		PRAutoClose:     &off,
	}
	cfg.applyDefaults()

	a := cfg.Governor.Advisory
	if a.MaxFindings != 25 {
		t.Errorf("MaxFindings = %d, want the configured 25", a.MaxFindings)
	}
	if !a.ShowAll {
		t.Error("ShowAll was reset to false")
	}
	if a.StalenessDays != 3 {
		t.Errorf("StalenessDays = %d, want the configured 3", a.StalenessDays)
	}
	if got := a.AdvisoryUpdateInterval(); got != 300*time.Second {
		t.Errorf("AdvisoryUpdateInterval() = %v, want 300s", got)
	}
	if a.PRAutoClose == nil || *a.PRAutoClose {
		t.Errorf("PRAutoClose = %v, want the configured false", a.PRAutoClose)
	}
	if a.PRAutoCloseEnabled() {
		t.Error("PRAutoCloseEnabled() = true, want false when explicitly disabled")
	}
}

func TestAdvisoryUpdateIntervalClamp(t *testing.T) {
	cfg := &Config{Governor: GovernorConfig{Advisory: AdvisoryConfig{UpdateIntervalS: 1}}}
	cfg.applyDefaults()
	if cfg.Governor.Advisory.UpdateIntervalS != AdvisoryUpdateIntervalMinS {
		t.Fatalf("UpdateIntervalS = %d, want clamp to %d", cfg.Governor.Advisory.UpdateIntervalS, AdvisoryUpdateIntervalMinS)
	}
	if got := cfg.Governor.Advisory.AdvisoryUpdateInterval(); got != 30*time.Second {
		t.Fatalf("AdvisoryUpdateInterval() = %v, want 30s", got)
	}
}

func TestAdvisoryUpdateIntervalYAMLRoundTrip(t *testing.T) {
	in := Config{Governor: GovernorConfig{Advisory: AdvisoryConfig{UpdateIntervalS: 900}}}
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var out Config
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if got := out.Governor.Advisory.UpdateIntervalS; got != 900 {
		t.Fatalf("round-trip update_interval_s = %d, want 900", got)
	}
}
