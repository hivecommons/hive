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
	disabled := false
	cfg.CloseOnMerge = &disabled
	if cfg.CloseOnMergeEnabled() {
		t.Fatal("close-on-merge explicit false ignored")
	}
	cfg.CloseOnMergeBackfillInterval = 2 * time.Hour
	if got := cfg.EffectiveCloseOnMergeBackfillInterval(); got != 2*time.Hour {
		t.Fatalf("configured interval = %v, want 2h", got)
	}
}
