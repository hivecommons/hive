package agent

import (
	"context"
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

func TestRestartStormBreakerBacksOffAutomaticLoops(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	m.SeedRestartTelemetry("scanner", 3, []RestartEvent{
		{At: now.Add(-3 * time.Minute), Reason: "crash"},
		{At: now.Add(-2 * time.Minute), Reason: "crash"},
		{At: now.Add(-time.Minute), Reason: "crash"},
	}, "crash")

	plan := m.planAutomaticRestart("scanner", "crashed agent recovery", "", now)
	if !plan.allowed || plan.backoff != 0 || plan.pauseReason != "" {
		t.Fatalf("first post-fast restart plan = %+v, want allowed", plan)
	}

	plan = m.planAutomaticRestart("scanner", "crashed agent recovery", "", now.Add(30*time.Second))
	if plan.allowed || plan.backoff <= 0 || plan.pauseReason != "" {
		t.Fatalf("inside backoff plan = %+v, want suppressed with backoff", plan)
	}
}

func TestRestartStormBreakerPausesAfterWindowCap(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	var events []RestartEvent
	for i := 0; i < restartStormPauseAfter; i++ {
		events = append(events, RestartEvent{At: now.Add(-time.Duration(i) * time.Minute), Reason: "copilot hang diagnostic timed out"})
	}
	m.SeedRestartTelemetry("scanner", restartStormPauseAfter, events, "copilot hang diagnostic timed out")

	plan := m.planAutomaticRestart("scanner", "copilot hang diagnostic timed out", "copilot hung with no output (diagnostic timed out)", now)
	if plan.allowed || plan.backoff != 0 || plan.pauseReason == "" {
		t.Fatalf("storm plan = %+v, want pause", plan)
	}
	if want := "restart-storm: copilot hung with no output (diagnostic timed out)"; plan.pauseReason != want {
		t.Fatalf("pause reason = %q, want %q", plan.pauseReason, want)
	}
}

func TestResetAndResumeClearRestartStormBreaker(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	m.mu.Lock()
	a := m.agents["scanner"]
	a.restartStormBackoffUntil = time.Now().Add(time.Hour)
	a.tokenRestartAttempts = 3
	a.tokenRestartGaveUp = true
	m.mu.Unlock()

	if err := m.ResetRestartCount("scanner"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	m.mu.RLock()
	if !m.agents["scanner"].restartStormBackoffUntil.IsZero() || m.agents["scanner"].tokenRestartAttempts != 0 || m.agents["scanner"].tokenRestartGaveUp {
		t.Fatalf("reset did not clear breaker fields: %+v", m.agents["scanner"])
	}
	m.mu.RUnlock()

	m.mu.Lock()
	m.agents["scanner"].Paused = true
	m.agents["scanner"].Config.Paused = true
	m.agents["scanner"].PausedTrigger = RestartStormTrigger
	m.agents["scanner"].PausedReason = "restart-storm: test"
	m.agents["scanner"].restartStormBackoffUntil = time.Now().Add(time.Hour)
	m.agents["scanner"].tokenRestartAttempts = 3
	m.agents["scanner"].tokenRestartGaveUp = true
	m.mu.Unlock()
	if err := m.Resume(context.Background(), "scanner", "dashboard-api", "operator resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	m.mu.RLock()
	if !m.agents["scanner"].restartStormBackoffUntil.IsZero() || m.agents["scanner"].tokenRestartAttempts != 0 || m.agents["scanner"].tokenRestartGaveUp {
		t.Fatalf("resume did not clear breaker fields: %+v", m.agents["scanner"])
	}
	m.mu.RUnlock()
}
