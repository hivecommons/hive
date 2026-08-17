package config

import "testing"

func TestApplyDefaultsBackfillsMergerTierLimit(t *testing.T) {
	cfg := &Config{}
	cfg.Hub.TierLimits = map[string]TierRate{
		"trusted": {MaxPerHour: 7, MaxPerDay: 8, MaxConcurrent: 9},
	}
	cfg.applyDefaults()

	if got := cfg.Hub.TierLimits["trusted"]; got.MaxPerHour != 7 || got.MaxPerDay != 8 || got.MaxConcurrent != 9 {
		t.Fatalf("applyDefaults overwrote existing trusted limits: %+v", got)
	}
	got, ok := cfg.Hub.TierLimits["merger"]
	if !ok {
		t.Fatalf("applyDefaults did not backfill merger tier limit: %+v", cfg.Hub.TierLimits)
	}
	if got.MaxPerHour != 30 || got.MaxPerDay != 200 || got.MaxConcurrent != 5 {
		t.Fatalf("merger defaults = %+v, want 30/hr 200/day 5 concurrent", got)
	}
}
