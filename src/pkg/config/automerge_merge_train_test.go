package config

import "testing"

func TestMergeTrainAutoMergeConfigDefaultsOn(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name string
		cfg  MergeTrainAutoMergeConfig
		want bool
	}{
		{"unset", MergeTrainAutoMergeConfig{}, true},
		{"explicit on", MergeTrainAutoMergeConfig{Enabled: &on}, true},
		{"explicit off", MergeTrainAutoMergeConfig{Enabled: &off}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.IsEnabled(); got != tc.want {
			t.Errorf("%s: IsEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
	var a AutoMergeConfig
	if !a.MergeTrain.IsEnabled() {
		t.Fatal("zero AutoMergeConfig disables the merge train; want default on")
	}
}
