package config

import "testing"

func TestSessionTaintConfigDefaults(t *testing.T) {
	var c SessionTaintConfig
	if c.Enabled {
		t.Error("default must be off")
	}
	if c.MaxSessionsOrDefault() != DefaultSessionTaintMaxSessions ||
		c.MaxLabelsPerSessionOrDefault() != DefaultSessionTaintMaxLabelsPerSession ||
		c.MaxDecisionsOrDefault() != DefaultSessionTaintMaxDecisions {
		t.Error("zero values must resolve to the defaults")
	}
	c = SessionTaintConfig{MaxSessions: -1, MaxLabelsPerSession: 5, MaxDecisions: 7}
	if c.MaxSessionsOrDefault() != DefaultSessionTaintMaxSessions || c.MaxLabelsPerSessionOrDefault() != 5 || c.MaxDecisionsOrDefault() != 7 {
		t.Error("explicit positive values must be kept, non-positive ones defaulted")
	}
}
