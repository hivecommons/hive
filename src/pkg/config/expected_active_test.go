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

// OffByCadence is the shared predicate behind the dashboard's offByCadence
// card signal and the watchdog's not-producing gate (#11910).
func TestOffByCadence(t *testing.T) {
	c := cfgWithModes()
	for _, tc := range []struct {
		name     string
		agent    string
		mode     string
		onDemand bool
		want     bool
	}{
		{name: "paused entry", agent: "brainstorm", mode: "surge", want: true},
		{name: "off entry", agent: "scanner", mode: "idle", want: true},
		{name: "kicking entry", agent: "scanner", mode: "surge", want: false},
		{name: "absent entry is not off", agent: "brainstorm", mode: "idle", want: false},
		{name: "unknown mode", agent: "brainstorm", mode: "busy", want: false},
		{name: "on-demand is never off by cadence", agent: "brainstorm", mode: "surge", onDemand: true, want: false},
		{name: "upper-case mode", agent: "brainstorm", mode: "SURGE", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.OffByCadence(tc.agent, tc.mode, tc.onDemand); got != tc.want {
				t.Errorf("OffByCadence(%q, %q, %v) = %v, want %v", tc.agent, tc.mode, tc.onDemand, got, tc.want)
			}
		})
	}
	var nilCfg *Config
	if nilCfg.OffByCadence("brainstorm", "surge", false) {
		t.Error("a nil config cannot tell, so it must not report off by cadence")
	}
}
