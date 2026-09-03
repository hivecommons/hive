package snapshot

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/watchdog"
)

func TestWatchdogStateRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	until := time.Date(2026, 8, 23, 12, 5, 0, 0, time.UTC)
	transition := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	state := &PersistedState{
		Agents: map[string]AgentState{"a1": {Paused: false}},
		Watchdog: map[string]watchdog.PersistedAgent{
			"a1": {
				Failures:     3,
				BackoffUntil: &until,
				CrashLooping: true,
				Conditions: []watchdog.Condition{{
					Type:               watchdog.ConditionReady,
					Status:             watchdog.ConditionFalse,
					Reason:             "shell-prompt",
					LastTransitionTime: transition,
				}},
			},
		},
	}
	if err := SaveState(path, state, logger); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.Watchdog["a1"]
	if !ok {
		t.Fatal("watchdog state lost on roundtrip")
	}
	if got.Failures != 3 || !got.CrashLooping || got.BackoffUntil == nil || !got.BackoffUntil.Equal(until) {
		t.Fatalf("watchdog state mangled: %+v", got)
	}
	if len(got.Conditions) != 1 || got.Conditions[0].Reason != "shell-prompt" ||
		!got.Conditions[0].LastTransitionTime.Equal(transition) {
		t.Fatalf("conditions mangled: %+v", got.Conditions)
	}
}

func TestWatchdogStateAbsentIsOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := SaveState(path, &PersistedState{}, logger); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Fatal("state file empty")
	}
	loaded, err := LoadState(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Watchdog != nil {
		t.Fatal("absent watchdog state must stay absent")
	}
}
