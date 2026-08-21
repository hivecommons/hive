package config

import "testing"

func cfgWithModes() *Config {
	c := &Config{}
	c.Governor.Modes = map[string]ModeConfig{
		"surge": {Cadences: map[string]Cadence{
			"scanner":    "5m",    // active in surge
			"brainstorm": "pause", // paused in surge
		}},
		"idle": {Cadences: map[string]Cadence{
			"scanner": "off", // off in idle
		}},
	}
	return c
}

func TestExpectedActive_PositiveCadence(t *testing.T) {
	c := cfgWithModes()
	if !c.ExpectedActive("scanner", "surge", false, nil) {
		t.Error("scanner has a 5m cadence in surge — expected active")
	}
}

func TestExpectedActive_PausedCadence(t *testing.T) {
	c := cfgWithModes()
	if c.ExpectedActive("brainstorm", "surge", false, nil) {
		t.Error("brainstorm is paused in surge — must not be expected active")
	}
	if c.ExpectedActive("scanner", "idle", false, nil) {
		t.Error("scanner is off in idle — must not be expected active")
	}
}

func TestExpectedActive_NoEntryForMode(t *testing.T) {
	c := cfgWithModes()
	// brainstorm has no idle cadence entry → not scheduled → not expected active.
	if c.ExpectedActive("brainstorm", "idle", false, nil) {
		t.Error("no cadence entry for mode must read as not expected active")
	}
	// Unknown mode entirely.
	if c.ExpectedActive("scanner", "busy", false, nil) {
		t.Error("unknown mode must read as not expected active")
	}
}

func TestExpectedActive_OnDemandNeverExpected(t *testing.T) {
	c := cfgWithModes()
	if c.ExpectedActive("scanner", "surge", true, nil) {
		t.Error("on-demand agent is never expected active on a schedule")
	}
	if c.ExpectedActive("scanner", "surge", false, map[string]bool{"scanner": true}) {
		t.Error("on-demand-by-pack agent is never expected active")
	}
}

func TestExpectedActive_ModeCaseInsensitive(t *testing.T) {
	c := cfgWithModes()
	// Governor mode strings arrive upper-cased (SURGE); the helper lowercases.
	if !c.ExpectedActive("scanner", "SURGE", false, nil) {
		t.Error("mode lookup must be case-insensitive")
	}
}
