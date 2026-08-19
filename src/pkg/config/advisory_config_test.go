package config

import "testing"

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
}

// TestAdvisoryConfigDefaultsPreserveExplicitValues confirms defaulting never
// overwrites an operator's choice, including an explicit pr_autoclose: false
// (the case a plain bool would silently lose).
func TestAdvisoryConfigDefaultsPreserveExplicitValues(t *testing.T) {
	off := false
	cfg := &Config{}
	cfg.Governor.Advisory = AdvisoryConfig{
		MaxFindings:   25,
		ShowAll:       true,
		StalenessDays: 3,
		PRAutoClose:   &off,
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
	if a.PRAutoClose == nil || *a.PRAutoClose {
		t.Errorf("PRAutoClose = %v, want the configured false", a.PRAutoClose)
	}
	if a.PRAutoCloseEnabled() {
		t.Error("PRAutoCloseEnabled() = true, want false when explicitly disabled")
	}
}
