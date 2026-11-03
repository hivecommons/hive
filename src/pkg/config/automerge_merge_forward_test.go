package config

import "testing"

func TestMergeForwardTrustedHumanFor(t *testing.T) {
	cfg := AutoMergeConfig{MergeForwardTrustedHuman: []string{" HiveCommons/Hive ", ""}}
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
			if got := tc.cfg.MergeForwardTrustedHumanFor(tc.repo); got != tc.want {
				t.Fatalf("MergeForwardTrustedHumanFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}

func TestSquashOnlyFor(t *testing.T) {
	cfg := AutoMergeConfig{SquashOnly: []string{" Acme/Widget ", ""}}
	tests := []struct {
		name string
		cfg  AutoMergeConfig
		repo string
		want bool
	}{
		{"unset", AutoMergeConfig{}, "acme/widget", false},
		{"not listed", cfg, "hivecommons/hive", false},
		{"listed case-insensitive", cfg, "acme/widget", true},
		{"listed with spaces", cfg, " ACME/WIDGET ", true},
		{"empty repo", cfg, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.SquashOnlyFor(tc.repo); got != tc.want {
				t.Fatalf("SquashOnlyFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}
