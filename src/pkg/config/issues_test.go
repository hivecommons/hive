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
