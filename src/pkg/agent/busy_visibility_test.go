package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func busyVisibilityTestManager() *Manager {
	return NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), ProjectContext{})
}

func TestBusyVisibilityCounterResetsOnDeliveryAndRestart(t *testing.T) {
	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	now := time.Date(2026, 9, 29, 4, 16, 0, 0, time.UTC)

	m.markKickUndeliverableLocked(agent, now)
	m.markKickUndeliverableLocked(agent, now.Add(time.Minute))
	if agent.KicksUndeliverable != 2 {
		t.Fatalf("kicks undeliverable = %d, want 2", agent.KicksUndeliverable)
	}
	if !agent.BusySince.Equal(now) {
		t.Fatalf("busy since = %v, want %v", agent.BusySince, now)
	}

	m.resetBusyVisibilityLocked(agent)
	if agent.KicksUndeliverable != 0 || !agent.BusySince.IsZero() || agent.BusyCondition != "" {
		t.Fatalf("reset after delivery left busy state: %+v", agent)
	}

	m.markKickUndeliverableLocked(agent, now)
	m.invalidateKicksOnRestartLocked(agent, "test restart", false, false)
	if agent.KicksUndeliverable != 0 || !agent.BusySince.IsZero() || agent.BusyCondition != "" {
		t.Fatalf("restart reset left busy state: %+v", agent)
	}
}

func TestNewestCopilotTranscriptActivityScansMultipleSessions(t *testing.T) {
	home := t.TempDir()
	older := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	newer := older.Add(2 * time.Hour)
	writeEventsFile(t, home, "parent", older)
	writeEventsFile(t, home, "sub-agent", newer)
	if err := os.MkdirAll(filepath.Join(home, ".copilot", "session-state", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := newestCopilotTranscriptActivity(home)
	if !ok {
		t.Fatal("newestCopilotTranscriptActivity ok = false, want true")
	}
	if !got.Equal(newer) {
		t.Fatalf("newest transcript activity = %v, want %v", got, newer)
	}
}

func TestBusyConditionThresholdsAndDefaults(t *testing.T) {
	now := time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)
	busySince := now.Add(-45 * time.Minute)
	lastActivity := now.Add(-31 * time.Minute)
	agent := &AgentProcess{
		Config:                 config.AgentConfig{},
		KicksUndeliverable:     busyUndeliverableThreshold,
		BusySince:              busySince,
		LastTranscriptActivity: lastActivity,
	}
	if got := agent.Config.EffectiveBusyNoActivityThreshold(); got != config.DefaultBusyNoActivityThreshold {
		t.Fatalf("default threshold = %v, want %v", got, config.DefaultBusyNoActivityThreshold)
	}
	condition, message := busyCondition(agent, now)
	if condition != busyNoActivityCondition {
		t.Fatalf("condition = %q (%s), want %q", condition, message, busyNoActivityCondition)
	}

	agent.KicksUndeliverable = busyUndeliverableThreshold - 1
	condition, _ = busyCondition(agent, now)
	if condition != "" {
		t.Fatalf("below kick threshold condition = %q, want empty", condition)
	}

	agent.KicksUndeliverable = busyUndeliverableThreshold
	agent.Config.BusyNoActivityThreshold = time.Hour
	condition, _ = busyCondition(agent, now)
	if condition != "" {
		t.Fatalf("below silence threshold condition = %q, want empty", condition)
	}

	agent.Config.MaxTurnDuration = 40 * time.Minute
	condition, message = busyCondition(agent, now)
	if condition != busyOverCeilingCondition {
		t.Fatalf("ceiling condition = %q (%s), want %q", condition, message, busyOverCeilingCondition)
	}

	oldKick := now.Add(-2 * time.Hour)
	idle := &AgentProcess{
		Config:   config.AgentConfig{MaxTurnDuration: 40 * time.Minute},
		LastKick: &oldKick,
	}
	condition, _ = busyCondition(idle, now)
	if condition != "" {
		t.Fatalf("idle old kick condition = %q, want empty", condition)
	}
}

func writeEventsFile(t *testing.T, home, id string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(home, ".copilot", "session-state", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant.message"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}
