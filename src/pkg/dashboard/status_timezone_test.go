package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestBuildGovernorNextRunIncludesMachineReadableTimestamp(t *testing.T) {
	last := time.Date(2026, 10, 9, 4, 11, 0, 0, time.UTC)
	cfg := &config.Config{Governor: config.GovernorConfig{EvalIntervalS: 300}}

	got := buildGovernor(governor.State{Mode: governor.ModeIdle, LastEval: last}, cfg)
	if got.NextKickAt != "2026-10-09T04:16:00Z" {
		t.Fatalf("NextKickAt = %q, want RFC3339 UTC next run", got.NextKickAt)
	}
	if got.NextKick == "" {
		t.Fatal("NextKick compatibility string should remain populated")
	}
}
