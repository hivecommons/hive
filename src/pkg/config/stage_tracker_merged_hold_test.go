package config

import (
	"testing"
	"time"
)

// TestStageTrackerMergedHold: dashboard.stage_tracker.merged_hold_s is the
// default (8 s) when unset or on a nil config, floors at 3 s, and is taken
// as given otherwise (hivecommons/hive#15808).
func TestStageTrackerMergedHold(t *testing.T) {
	if got := (*Config)(nil).StageTrackerMergedHold(); got != 8*time.Second {
		t.Fatalf("nil config: got %s, want 8s", got)
	}
	for in, want := range map[int]time.Duration{0: 8 * time.Second, 1: 3 * time.Second, 3: 3 * time.Second, 45: 45 * time.Second} {
		c := &Config{}
		c.Dashboard.StageTracker.MergedHoldS = in
		if got := c.StageTrackerMergedHold(); got != want {
			t.Errorf("merged_hold_s=%d: got %s, want %s", in, got, want)
		}
	}
}
