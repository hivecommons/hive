package agent

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestRestartTelemetryPrunesRollingWindowAndReset(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	m.SeedRestartTelemetry("scanner", 7, []RestartEvent{
		{At: now.Add(-25 * time.Hour), Reason: "old"},
		{At: now.Add(-2 * time.Hour), Reason: "crash"},
	}, "crash")

	total, last24h, lastAt, reason, ok := m.RestartTelemetry("scanner")
	if !ok {
		t.Fatal("telemetry missing")
	}
	if total != 7 || last24h != 1 || reason != "crash" || lastAt.IsZero() {
		t.Fatalf("telemetry = total %d last24h %d lastAt %v reason %q", total, last24h, lastAt, reason)
	}
	if err := m.ResetRestartCount("scanner"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	total, last24h, _, _, _ = m.RestartTelemetry("scanner")
	if total != 0 || last24h != 0 {
		t.Fatalf("after reset total=%d last24h=%d, want zero", total, last24h)
	}
}
