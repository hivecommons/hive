package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestMergeTrainStallWarningYAMLRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, yaml   string
		want         time.Duration
		explicitZero bool
	}{
		{"absent", "governor: {}", time.Hour, false},
		{"null", "governor: {merge_train_stall_warn_s: null}", time.Hour, false},
		{"disabled", "governor: {merge_train_stall_warn_s: 0}", 0, true},
		{"custom", "governor: {merge_train_stall_warn_s: 120}", 2 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.MergeTrainStallWarn(); got != tc.want {
				t.Fatalf("threshold=%s want %s", got, tc.want)
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.explicitZero && !strings.Contains(string(data), "merge_train_stall_warn_s: 0") {
				t.Fatal("explicit zero was dropped on save")
			}
			var restored Config
			if err := yaml.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if got := restored.MergeTrainStallWarn(); got != tc.want {
				t.Fatalf("restored threshold=%s want %s", got, tc.want)
			}
		})
	}
	cfg := &Config{}
	cfg.SetMergeTrainStallWarnS(0)
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err := yaml.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.MergeTrainStallWarn() != 0 {
		t.Fatal("dashboard disable did not survive persistence")
	}
}
