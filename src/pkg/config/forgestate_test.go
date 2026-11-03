package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestForgeStateConfigDefaults(t *testing.T) {
	var c ForgeStateConfig
	if c.Enabled {
		t.Fatal("forgestate must default off")
	}
	if got := c.ReconcileInterval(); got != 15*time.Minute {
		t.Errorf("ReconcileInterval = %v, want 15m", got)
	}
	if got := c.FreshnessBound(); got != time.Minute {
		t.Errorf("FreshnessBound = %v, want 60s", got)
	}
	if got := c.DriftThresholdOrDefault(); got != 0.05 {
		t.Errorf("DriftThresholdOrDefault = %v, want 0.05", got)
	}
	if got := c.SearchBudgetPerMinute(); got != 20 {
		t.Errorf("SearchBudgetPerMinute = %d, want 20", got)
	}
}

func TestForgeStateConfigParsesOverrides(t *testing.T) {
	var cfg Config
	src := []byte("forgestate:\n  enabled: true\n  reconcile_interval_s: 300\n  freshness_bound_s: 30\n  drift_threshold: 0.2\n  search_budget: 10\n")
	if err := yaml.Unmarshal(src, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := cfg.ForgeState
	if !c.Enabled {
		t.Error("Enabled = false, want true")
	}
	if got := c.ReconcileInterval(); got != 5*time.Minute {
		t.Errorf("ReconcileInterval = %v, want 5m", got)
	}
	if got := c.FreshnessBound(); got != 30*time.Second {
		t.Errorf("FreshnessBound = %v, want 30s", got)
	}
	if got := c.DriftThresholdOrDefault(); got != 0.2 {
		t.Errorf("DriftThresholdOrDefault = %v, want 0.2", got)
	}
	if got := c.SearchBudgetPerMinute(); got != 10 {
		t.Errorf("SearchBudgetPerMinute = %d, want 10", got)
	}
}
