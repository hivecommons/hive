package governor

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestReplicaCadenceFallsBackToBaseAndStaggers(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"scanner":   {Enabled: true, ReplicaIndex: 1, ReplicaCount: 2},
		"scanner-2": {Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2},
	}
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"scanner": "10m"}},
	}}, agents, slog.Default())
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	due := g.Evaluate(0, 0, 0, 0)
	if len(due) != 1 || due[0] != "scanner" {
		t.Fatalf("initial due = %v, want only scanner", due)
	}
	g.RecordKick("scanner")
	now = now.Add(5 * time.Minute)
	due = g.Evaluate(0, 0, 0, 0)
	if len(due) != 1 || due[0] != "scanner-2" {
		t.Fatalf("staggered due = %v, want only scanner-2", due)
	}
}
