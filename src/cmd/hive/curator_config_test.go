package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// curatorConfigFromHive is the only bridge between the operator-facing
// knowledge_curator config block and the knowledge package's CuratorConfig.
// Every field must survive the copy: a silently dropped field here means an
// operator setting (e.g. auto_promote_threshold) is parsed but never acted on.
func TestCuratorConfigFromHiveCopiesEveryField(t *testing.T) {
	enabled := true
	in := config.KnowledgeCurator{
		Enabled:              &enabled,
		Schedule:             "0 3 * * *",
		ExtractFrom:          []string{"retro", "beads"},
		AutoPromoteThreshold: 0.75,
		PromoteFrom:          "hive",
		PromoteTo:            "org",
	}

	got := curatorConfigFromHive(in)

	want := knowledge.CuratorConfig{
		Enabled:              &enabled,
		Schedule:             "0 3 * * *",
		ExtractFrom:          []string{"retro", "beads"},
		AutoPromoteThreshold: 0.75,
		PromoteFrom:          "hive",
		PromoteTo:            "org",
	}
	if got.Enabled == nil || *got.Enabled != *want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if got.Schedule != want.Schedule {
		t.Errorf("Schedule = %q, want %q", got.Schedule, want.Schedule)
	}
	if len(got.ExtractFrom) != 2 || got.ExtractFrom[0] != "retro" || got.ExtractFrom[1] != "beads" {
		t.Errorf("ExtractFrom = %v, want %v", got.ExtractFrom, want.ExtractFrom)
	}
	if got.AutoPromoteThreshold != want.AutoPromoteThreshold {
		t.Errorf("AutoPromoteThreshold = %v, want %v", got.AutoPromoteThreshold, want.AutoPromoteThreshold)
	}
	if got.PromoteFrom != want.PromoteFrom || got.PromoteTo != want.PromoteTo {
		t.Errorf("Promote from/to = %q/%q, want %q/%q", got.PromoteFrom, got.PromoteTo, want.PromoteFrom, want.PromoteTo)
	}
	if zero := curatorConfigFromHive(config.KnowledgeCurator{}); zero.Enabled != nil || zero.Schedule != "" || zero.ExtractFrom != nil {
		t.Errorf("zero-value input produced non-zero CuratorConfig: %+v", zero)
	}
}
