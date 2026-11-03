package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Toast duration knobs (hivecommons/hive#15813): defaults when unset or nil,
// clamped to [MinStageTrackerToastS, MaxStageTrackerToastS] otherwise.
func TestStageTrackerToastAccessors(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.StageTrackerToastInfo(); got != DefaultStageTrackerToastInfoS*time.Second {
		t.Fatalf("nil info = %v", got)
	}
	if got := nilCfg.StageTrackerToastError(); got != DefaultStageTrackerToastErrorS*time.Second {
		t.Fatalf("nil error = %v", got)
	}

	cases := []struct {
		name            string
		info, errS      int
		wantInfo, wantE time.Duration
	}{
		{"unset", 0, 0, 15 * time.Second, 30 * time.Second},
		{"configured", 20, 45, 20 * time.Second, 45 * time.Second},
		{"clamped low", 1, -7, 3 * time.Second, 3 * time.Second},
		{"clamped high", 500, 121, 120 * time.Second, 120 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			c.Dashboard.StageTracker.ToastInfoS = tc.info
			c.Dashboard.StageTracker.ToastErrorS = tc.errS
			if got := c.StageTrackerToastInfo(); got != tc.wantInfo {
				t.Errorf("info = %v, want %v", got, tc.wantInfo)
			}
			if got := c.StageTrackerToastError(); got != tc.wantE {
				t.Errorf("error = %v, want %v", got, tc.wantE)
			}
		})
	}
}

func TestStageTrackerToastYAMLKeys(t *testing.T) {
	var c Config
	src := "dashboard:\n  stage_tracker:\n    toast_info_s: 9\n    toast_error_s: 60\n"
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	if c.StageTrackerToastInfo() != 9*time.Second || c.StageTrackerToastError() != 60*time.Second {
		t.Fatalf("yaml keys not wired: info=%v error=%v", c.StageTrackerToastInfo(), c.StageTrackerToastError())
	}
}
