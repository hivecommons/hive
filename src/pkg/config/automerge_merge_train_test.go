package config

import "testing"

func TestMergeTrainAutoMergeConfigDefaults(t *testing.T) {
	off, on := false, true
	tests := []struct {
		name        string
		cfg         MergeTrainAutoMergeConfig
		wantEnabled bool
		wantMax     int
	}{
		{"unset is on with the default guard", MergeTrainAutoMergeConfig{}, true, DefaultMergeTrainMaxRebaseAttempts},
		{"explicit on", MergeTrainAutoMergeConfig{Enabled: &on, MaxRebaseAttempts: 3}, true, 3},
		{"explicit off", MergeTrainAutoMergeConfig{Enabled: &off}, false, DefaultMergeTrainMaxRebaseAttempts},
		{"negative guard falls back", MergeTrainAutoMergeConfig{MaxRebaseAttempts: -1}, true, DefaultMergeTrainMaxRebaseAttempts},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsEnabled(); got != tc.wantEnabled {
				t.Fatalf("IsEnabled = %v, want %v", got, tc.wantEnabled)
			}
			if got := tc.cfg.EffectiveMaxRebaseAttempts(); got != tc.wantMax {
				t.Fatalf("EffectiveMaxRebaseAttempts = %d, want %d", got, tc.wantMax)
			}
		})
	}
}
