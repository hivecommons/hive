package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Closing human-authored PRs is destructive, so a config with no
// `supersession_sweep:` block must leave it off (hivecommons/hive#11418).
func TestSupersessionSweepCloseContributorPRsDefaultsOff(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("project:\n  org: acme\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.SupersessionSweep.CloseContributorPRs {
		t.Error("supersession_sweep.close_contributor_prs defaults on; it must be opt-in")
	}
	if cfg.SupersessionSweep.GracePeriod != 0 {
		t.Errorf("grace_period = %v, want 0 (sweep default)", cfg.SupersessionSweep.GracePeriod)
	}
}

func TestSupersessionSweepParsesKnobs(t *testing.T) {
	var cfg Config
	src := "supersession_sweep:\n  close_contributor_prs: true\n  grace_period: 48h\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.SupersessionSweep.CloseContributorPRs {
		t.Error("close_contributor_prs did not parse")
	}
	if cfg.SupersessionSweep.GracePeriod != 48*time.Hour {
		t.Errorf("grace_period = %v, want 48h", cfg.SupersessionSweep.GracePeriod)
	}
}
