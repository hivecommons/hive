package config

import (
	"testing"
	"time"
)

func TestIssuesCloseOnMergeDefaults(t *testing.T) {
	var cfg IssuesConfig
	if !cfg.CloseOnMergeEnabled() {
		t.Fatal("close-on-merge default = false, want true")
	}
	if got := cfg.EffectiveCloseOnMergeBackfillInterval(); got != time.Hour {
		t.Fatalf("backfill interval = %v, want 1h", got)
	}
	if cfg.ReporterConfirmationEnabled() {
		t.Fatal("reporter confirmation default = true, want false")
	}
	disabled := false
	cfg.CloseOnMerge = &disabled
	if cfg.CloseOnMergeEnabled() {
		t.Fatal("close-on-merge explicit false ignored")
	}
	enabled := true
	cfg.ReporterConfirmation = &enabled
	if !cfg.ReporterConfirmationEnabled() {
		t.Fatal("reporter_confirmation explicit true ignored")
	}
	cfg.CloseOnMergeBackfillInterval = 2 * time.Hour
	if got := cfg.EffectiveCloseOnMergeBackfillInterval(); got != 2*time.Hour {
		t.Fatalf("configured interval = %v, want 2h", got)
	}
}

func TestIssuesCloseOnMergeSweepIntervals(t *testing.T) {
	var cfg IssuesConfig
	if got := cfg.EffectiveCloseOnMergeInterval(); got != DefaultCloseOnMergeInterval {
		t.Fatalf("sweep interval default = %v, want %v", got, DefaultCloseOnMergeInterval)
	}
	if got := cfg.EffectiveCloseOnMergeRecheck(); got != DefaultCloseOnMergeRecheck {
		t.Fatalf("recheck default = %v, want %v", got, DefaultCloseOnMergeRecheck)
	}
	cfg.CloseOnMergeInterval = 5 * time.Minute
	cfg.CloseOnMergeRecheck = 6 * time.Hour
	if got := cfg.EffectiveCloseOnMergeInterval(); got != 5*time.Minute {
		t.Fatalf("configured sweep interval = %v, want 5m", got)
	}
	if got := cfg.EffectiveCloseOnMergeRecheck(); got != 6*time.Hour {
		t.Fatalf("configured recheck = %v, want 6h", got)
	}
	cfg.CloseOnMergeInterval, cfg.CloseOnMergeRecheck = -time.Second, -time.Second
	if cfg.EffectiveCloseOnMergeInterval() != DefaultCloseOnMergeInterval || cfg.EffectiveCloseOnMergeRecheck() != DefaultCloseOnMergeRecheck {
		t.Fatal("negative values did not fall back to defaults")
	}
}

func TestIssuesCloseOnMergeEnvOverrides(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	var cfg IssuesConfig
	cfg.applyEnv(getenv)
	if cfg.CloseOnMergeInterval != 0 || cfg.CloseOnMergeRecheck != 0 {
		t.Fatalf("empty env changed config: %+v", cfg)
	}

	env[CloseOnMergeIntervalEnvVar] = " 15m "
	env[CloseOnMergeRecheckEnvVar] = "12h"
	cfg.applyEnv(getenv)
	if cfg.CloseOnMergeInterval != 15*time.Minute || cfg.CloseOnMergeRecheck != 12*time.Hour {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}

	// Malformed or non-positive values leave the previous value in place.
	env[CloseOnMergeIntervalEnvVar] = "soon"
	env[CloseOnMergeRecheckEnvVar] = "-1h"
	cfg.applyEnv(getenv)
	if cfg.CloseOnMergeInterval != 15*time.Minute || cfg.CloseOnMergeRecheck != 12*time.Hour {
		t.Fatalf("bad env values overwrote config: %+v", cfg)
	}
}
