package config

import "testing"

func TestHumanMergeOwnerApprovalFor(t *testing.T) {
	cfg := AutoMergeConfig{HumanMergeOwnerApproval: []string{" Acme/Widget ", ""}}
	for _, tc := range []struct {
		name string
		cfg  AutoMergeConfig
		repo string
		want bool
	}{
		{"unset is off everywhere", AutoMergeConfig{}, "acme/widget", false},
		{"not listed", cfg, "hivecommons/hive", false},
		{"listed case-insensitive", cfg, "acme/widget", true},
		{"listed with spaces", cfg, " ACME/WIDGET ", true},
		{"empty repo", cfg, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.HumanMergeOwnerApprovalFor(tc.repo); got != tc.want {
				t.Fatalf("HumanMergeOwnerApprovalFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}
