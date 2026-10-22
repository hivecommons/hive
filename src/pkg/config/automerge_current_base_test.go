package config

import "testing"

func TestRequireCurrentBaseFor(t *testing.T) {
	cfg := AutoMergeConfig{RequireCurrentBase: []string{" HiveCommons/Hive ", ""}}
	tests := []struct {
		name string
		cfg  AutoMergeConfig
		repo string
		want bool
	}{
		{"unset", AutoMergeConfig{}, "hivecommons/hive", false},
		{"not listed", cfg, "acme/widget", false},
		{"listed case-insensitive", cfg, "hivecommons/hive", true},
		{"listed with spaces", cfg, " HIVECOMMONS/HIVE ", true},
		{"empty repo", cfg, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.RequireCurrentBaseFor(tc.repo); got != tc.want {
				t.Fatalf("RequireCurrentBaseFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}
