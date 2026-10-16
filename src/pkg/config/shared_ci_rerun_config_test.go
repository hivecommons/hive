package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The shared-CI rerun sweep is on unless `shared_ci_rerun.enabled: false`
// (hivecommons/hive#12253).
func TestSharedCIRerunEnabledDefaultsOn(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"project:\n  org: acme\n", true},
		{"shared_ci_rerun:\n  enabled: true\n", true},
		{"shared_ci_rerun:\n  enabled: false\n", false},
	}
	for _, tc := range cases {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tc.src), &cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.src, err)
		}
		if got := cfg.SharedCIRerun.IsEnabled(); got != tc.want {
			t.Errorf("%q: IsEnabled = %v, want %v", tc.src, got, tc.want)
		}
	}
}
