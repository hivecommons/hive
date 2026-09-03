package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// #4041: pause provenance — WHO paused WHAT WHEN — must live on the agent
// state itself, not only in the audit log, or a deliberate owner quiesce is
// indistinguishable from a malfunction days later.

func provenanceTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
}

// Positive control: a pause performed on behalf of a person records the actor.
func TestPauseBy_RecordsActor(t *testing.T) {
	m := provenanceTestManager(t)
	if err := m.PauseBy("scanner", "dashboard-api", "manual pause", "bketelsen"); err != nil {
		t.Fatalf("PauseBy: %v", err)
	}
	status, err := m.GetStatus("scanner")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !status.Paused {
		t.Fatal("agent not paused")
	}
	if status.PausedBy != "bketelsen" {
		t.Errorf("PausedBy = %q, want %q", status.PausedBy, "bketelsen")
	}
	if status.PausedTrigger != "dashboard-api" {
		t.Errorf("PausedTrigger = %q, want %q", status.PausedTrigger, "dashboard-api")
	}
	if status.PausedReason != "manual pause" {
		t.Errorf("PausedReason = %q, want %q", status.PausedReason, "manual pause")
	}
	if status.PausedAt.IsZero() {
		t.Error("PausedAt not set")
	}
}

// A system-initiated pause must never fabricate a human actor.
func TestPause_LeavesActorEmpty(t *testing.T) {
	m := provenanceTestManager(t)
	if err := m.Pause("scanner", "login-detector", "login required detected"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	status, _ := m.GetStatus("scanner")
	if status.PausedBy != "" {
		t.Errorf("PausedBy = %q, want empty for a system-initiated pause", status.PausedBy)
	}
	if status.PausedTrigger != "login-detector" {
		t.Errorf("PausedTrigger = %q, want %q", status.PausedTrigger, "login-detector")
	}
}

// Resume clears the whole provenance set — stale WHO/WHY/WHEN on a running
// agent would be worse than none.
func TestResume_ClearsProvenance(t *testing.T) {
	m := provenanceTestManager(t)
	if err := m.PauseBy("scanner", "dashboard-api", "manual pause", "bketelsen"); err != nil {
		t.Fatalf("PauseBy: %v", err)
	}
	// The relaunch half may fail without a live tmux; provenance is cleared
	// before it either way, and that clearing is the contract under test.
	_ = m.Resume(context.Background(), "scanner", "dashboard-api", "manual resume")
	status, _ := m.GetStatus("scanner")
	if status.Paused {
		t.Fatal("agent still paused after Resume")
	}
	if status.PausedBy != "" || status.PausedTrigger != "" || status.PausedReason != "" {
		t.Errorf("provenance not cleared: by=%q trigger=%q reason=%q",
			status.PausedBy, status.PausedTrigger, status.PausedReason)
	}
	if !status.PausedAt.IsZero() {
		t.Errorf("PausedAt not cleared: %v", status.PausedAt)
	}
}

// SeedPauseState (the boot-restore path) replays the persisted actor, so
// provenance survives restarts instead of downgrading to anonymous.
func TestSeedPauseState_ReplaysActor(t *testing.T) {
	m := provenanceTestManager(t)
	pausedAt := time.Date(2026, 8, 14, 16, 10, 10, 0, time.UTC)
	m.SeedPauseState("scanner", pausedAt, "dashboard-api", "manual pause", "bketelsen")
	status, _ := m.GetStatus("scanner")
	if status.PausedBy != "bketelsen" {
		t.Errorf("PausedBy = %q, want %q", status.PausedBy, "bketelsen")
	}
	if !status.PausedAt.Equal(pausedAt) {
		t.Errorf("PausedAt = %v, want %v", status.PausedAt, pausedAt)
	}
}

func TestPauseByCauseEmitsPostPersistenceEvent(t *testing.T) {
	m := provenanceTestManager(t)
	persisted := make(chan bool, 1)
	observed := make(chan PauseTransitionEvent, 1)
	m.SetPersistPauseCallback(func(name string, paused bool) {
		if name == "scanner" {
			persisted <- paused
		}
	})
	m.SetPauseTransitionObserver(func(event PauseTransitionEvent) {
		observed <- event
	})

	cause := PauseCausation{Depth: 1, HookName: "pause-on-red", OriginTransition: "escalation_red"}
	if err := m.PauseByCause("scanner", "hook:pause-on-red", "red CI", "hook:pause-on-red", cause); err != nil {
		t.Fatalf("PauseByCause: %v", err)
	}
	select {
	case paused := <-persisted:
		if !paused {
			t.Fatal("persist callback saw resume, want pause")
		}
	default:
		t.Fatal("pause observer fired before or without persistence")
	}

	select {
	case event := <-observed:
		if !event.Paused || event.Agent != "scanner" || event.Trigger != "hook:pause-on-red" {
			t.Fatalf("unexpected event: %+v", event)
		}
		if event.Causation != cause {
			t.Fatalf("causation not preserved: got %+v want %+v", event.Causation, cause)
		}
	case <-time.After(time.Second):
		t.Fatal("pause transition observer did not fire")
	}
}
