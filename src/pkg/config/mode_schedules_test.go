package config

import (
	"reflect"
	"testing"
)

// The governor's ModeUnscheduledAgents (#7474) and the spoke dashboard's
// per-agent unscheduledInMode card flag must name the SAME agents. Both call
// ModeSchedulesIn; this pins the contract that makes sharing it safe: it
// follows the governor's resolveCadence chain (the mode's own entry, else the
// idle entry every mode inherits), an explicit pause/off entry COUNTS as
// scheduled, and a replica reads its base's entries.

func modeSchedulesFixture() *Config {
	return &Config{
		Agents: map[string]AgentConfig{
			"scanner":    {},
			"reviewer":   {},
			"reviewer-2": {ReplicaOf: "reviewer"},
			"parked":     {},
			"janitor":    {},
			"lonely":     {},
		},
		Governor: GovernorConfig{Modes: map[string]ModeConfig{
			"idle":  {Threshold: 0, Cadences: map[string]Cadence{"scanner": "1h", "janitor": "2h"}},
			"quiet": {Threshold: 28, Cadences: map[string]Cadence{"scanner": "30m"}},
			"busy":  {Threshold: 150, Cadences: map[string]Cadence{"scanner": "15m", "parked": "pause"}},
			"surge": {Threshold: 550, Cadences: map[string]Cadence{"scanner": "5m", "reviewer": "30m", "parked": "30m"}},
		}},
	}
}

func TestModeSchedules(t *testing.T) {
	c := modeSchedulesFixture()
	tests := []struct {
		agent, mode string
		want        bool
		why         string
	}{
		{"reviewer", "surge", true, "its own surge entry"},
		{"reviewer", "busy", false, "no busy entry and no idle entry to inherit — the #7474 shape"},
		{"reviewer", "quiet", false, "same gap one mode lower"},
		{"reviewer", "idle", false, "idle is the fallback, and it has nothing either"},
		{"reviewer", "BUSY", false, "governor Mode strings are upper-case; matched case-insensitively"},
		{"reviewer-2", "surge", true, "a replica reads its base's entry"},
		{"reviewer-2", "busy", false, "and inherits its base's gap"},
		{"janitor", "surge", true, "an idle entry is inherited by every mode"},
		{"janitor", "busy", true, "an idle entry is inherited by every mode"},
		{"parked", "busy", true, "an explicit pause entry is scheduled-and-paused, not unscheduled"},
		{"parked", "quiet", false, "entries in busy and surge do not reach quiet"},
		{"scanner", "quiet", true, "named in every mode"},
		{"lonely", "surge", false, "no mode names it at all"},
	}
	for _, tc := range tests {
		if got := c.ModeSchedules(tc.agent, tc.mode); got != tc.want {
			t.Errorf("ModeSchedules(%q, %q) = %v, want %v (%s)", tc.agent, tc.mode, got, tc.want, tc.why)
		}
	}
	var nilCfg *Config
	if !nilCfg.ModeSchedules("reviewer", "busy") {
		t.Error("a nil config must report scheduled: \"cannot tell\" must not read as \"unscheduled\"")
	}
}

// CadenceModes is what the banner and the card print after "only in", so its
// order has to be the ladder's order (by threshold), not map order.
func TestCadenceModesInThresholdOrder(t *testing.T) {
	c := modeSchedulesFixture()
	tests := []struct {
		agent string
		want  []string
	}{
		{"scanner", []string{"idle", "quiet", "busy", "surge"}},
		{"parked", []string{"busy", "surge"}},
		{"reviewer", []string{"surge"}},
		{"reviewer-2", []string{"surge"}},
		{"janitor", []string{"idle"}},
		{"lonely", []string{}},
	}
	for _, tc := range tests {
		if got := c.CadenceModes(tc.agent); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CadenceModes(%q) = %v, want %v", tc.agent, got, tc.want)
		}
	}
	var nilCfg *Config
	if got := nilCfg.CadenceModes("scanner"); got == nil || len(got) != 0 {
		t.Errorf("nil config CadenceModes = %#v, want an empty non-nil slice", got)
	}
}
