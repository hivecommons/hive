package config

import "testing"

// The governor's NoCadenceAgents (#5577) and the spoke dashboard's per-agent
// noCadence card flag (#5594) must name the SAME agents. Both call
// HasAnyCadenceIn; this pins the contract that makes sharing it safe: an
// explicit "off"/"pause" entry COUNTS as configured, and only total absence
// from every mode reads as unscheduled.

func TestHasAnyCadence(t *testing.T) {
	c := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{
				"scanner":   {},
				"quality":   {},
				"scanner-2": {ReplicaOf: "scanner"},
				"lonely":    {},
			},
		},
		Governor: GovernorConfig{Modes: map[string]ModeConfig{
			"idle":  {Cadences: map[string]Cadence{"scanner": "15m"}},
			"surge": {Cadences: map[string]Cadence{"quality": "pause"}},
		}},
	}
	tests := []struct {
		agent string
		want  bool
		why   string
	}{
		{"scanner", true, "named by the idle mode"},
		{"quality", true, "an explicit pause entry is operator choice, not omission"},
		{"scanner-2", true, "inherits its replica base's cadence"},
		{"lonely", false, "no mode names it at all"},
	}
	for _, tc := range tests {
		if got := c.HasAnyCadence(tc.agent); got != tc.want {
			t.Errorf("HasAnyCadence(%q) = %v, want %v (%s)", tc.agent, got, tc.want, tc.why)
		}
	}
}

func TestHasAnyCadenceInEmptyModes(t *testing.T) {
	if HasAnyCadenceIn(nil, "scanner", "scanner") {
		t.Error("HasAnyCadenceIn(nil, ...) = true, want false")
	}
	if HasAnyCadenceIn(map[string]ModeConfig{"idle": {}}, "scanner", "") {
		t.Error("a mode with no cadence map must not count as configured")
	}
}
