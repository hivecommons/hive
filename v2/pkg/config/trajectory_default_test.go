package config

import "testing"

func TestTrajectoryDefaultOn(t *testing.T) {
	// nil Enabled (key omitted) → enabled
	var tc TrajectoryConfig
	if !tc.IsEnabled() {
		t.Error("omitted trajectory.enabled must default to on")
	}
	// explicit false → disabled
	f := false
	tc.Enabled = &f
	if tc.IsEnabled() {
		t.Error("explicit trajectory.enabled=false must disable")
	}
	// explicit true → enabled
	tr := true
	tc.Enabled = &tr
	if !tc.IsEnabled() {
		t.Error("explicit trajectory.enabled=true must enable")
	}
}

func TestTrajectoryDefaultsApplied(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Governor.Trajectory.OnDivergence != "pause" {
		t.Errorf("on_divergence default = %q, want pause", c.Governor.Trajectory.OnDivergence)
	}
}
