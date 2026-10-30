package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDedupeAdvisoryAutocloseDefaultsOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"unset", "hive_id: h\n", true},
		{"explicit on", "dedupe:\n  advisory_autoclose: true\n", true},
		{"explicit off", "dedupe:\n  advisory_autoclose: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.Dedupe.AdvisoryAutocloseEnabled(); got != tc.want {
				t.Fatalf("AdvisoryAutocloseEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
