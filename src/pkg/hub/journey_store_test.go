package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// parseSnoozeUntil
// ---------------------------------------------------------------------------

func TestParseSnoozeUntil_EmptyOrZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{"", "0", "  ", "  0  "} {
		got, err := parseSnoozeUntil(raw, now)
		if err != nil {
			t.Errorf("parseSnoozeUntil(%q): unexpected error: %v", raw, err)
		}
		if !got.IsZero() {
			t.Errorf("parseSnoozeUntil(%q): expected zero time, got %v", raw, got)
		}
	}
}

func TestParseSnoozeUntil_ValidDuration(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	got, err := parseSnoozeUntil("720h", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := now.Add(720 * time.Hour)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseSnoozeUntil_NegativeDuration(t *testing.T) {
	now := time.Now()
	got, err := parseSnoozeUntil("-1h", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("negative duration should return zero time, got %v", got)
	}
}

func TestParseSnoozeUntil_ExceedsMaximum(t *testing.T) {
	now := time.Now()
	// maxJourneySnoozeDuration is 90 days = 2160h
	_, err := parseSnoozeUntil("3000h", now)
	if err == nil {
		t.Fatal("expected error for duration exceeding maximum")
	}
}

func TestParseSnoozeUntil_InvalidFormat(t *testing.T) {
	now := time.Now()
	_, err := parseSnoozeUntil("not-a-duration", now)
	if err == nil {
		t.Fatal("expected error for invalid duration format")
	}
}

// ---------------------------------------------------------------------------
// journeyStore: newJourneyStore, get, recordSent, setSnooze
// ---------------------------------------------------------------------------

func TestJourneyStore_NewIsEmpty(t *testing.T) {
	js := newJourneyStore()
	if got := js.get("hive-1"); got != nil {
		t.Fatalf("expected nil for unknown hive, got %+v", got)
	}
}

func TestJourneyStore_NilReceiverSafe(t *testing.T) {
	var js *journeyStore
	if got := js.get("any"); got != nil {
		t.Fatal("nil journeyStore.get should return nil")
	}
	// These must not panic.
	js.recordSent("any", nudge{Stage: StageACMMLevel}, 3, time.Now())
	js.setSnooze("any", time.Now().Add(time.Hour), "admin", "test")
	if err := js.save(); err != nil {
		t.Fatalf("nil journeyStore.save should not error: %v", err)
	}
	if err := js.load(time.Now()); err != nil {
		t.Fatalf("nil journeyStore.load should not error: %v", err)
	}
}

func TestJourneyStore_RecordSentAndGet(t *testing.T) {
	js := newJourneyStore()
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	n := nudge{Stage: StageACMMLevel, Severity: SeverityGentle}
	js.recordSent("hive-a", n, 3, now)

	st := js.get("hive-a")
	if st == nil {
		t.Fatal("expected non-nil state after recordSent")
	}
	if st.LastStage != StageACMMLevel.String() {
		t.Errorf("LastStage = %q, want %q", st.LastStage, StageACMMLevel.String())
	}
	if st.LastACMMLevel != 3 {
		t.Errorf("LastACMMLevel = %d, want 3", st.LastACMMLevel)
	}
	if st.LastSentAt != now.UTC().Format(time.RFC3339) {
		t.Errorf("LastSentAt = %q, want %q", st.LastSentAt, now.UTC().Format(time.RFC3339))
	}
}

func TestJourneyStore_GetReturnsCopy(t *testing.T) {
	js := newJourneyStore()
	js.recordSent("hive-b", nudge{Stage: StageACMMLevel}, 2, time.Now())
	st1 := js.get("hive-b")
	st1.LastACMMLevel = 999
	st2 := js.get("hive-b")
	if st2.LastACMMLevel == 999 {
		t.Fatal("get() did not return a copy; mutation leaked into store")
	}
}

func TestJourneyStore_SetSnoozeAndClear(t *testing.T) {
	js := newJourneyStore()
	until := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	js.setSnooze("hive-c", until, "admin-x", "exempt test")

	st := js.get("hive-c")
	if st == nil {
		t.Fatal("expected non-nil state after setSnooze")
	}
	if st.SnoozedBy != "admin-x" {
		t.Errorf("SnoozedBy = %q, want admin-x", st.SnoozedBy)
	}
	if st.SnoozeReason != "exempt test" {
		t.Errorf("SnoozeReason = %q, want 'exempt test'", st.SnoozeReason)
	}

	// Clear snooze with zero time.
	js.setSnooze("hive-c", time.Time{}, "", "")
	st = js.get("hive-c")
	if st.SnoozedUntil != "" {
		t.Errorf("SnoozedUntil should be empty after clear, got %q", st.SnoozedUntil)
	}
}

// ---------------------------------------------------------------------------
// journeyStore: save / load round-trip
// ---------------------------------------------------------------------------

func TestJourneyStore_SaveLoadRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	origPath := journeyStatePath
	journeyStatePath = filepath.Join(tmpDir, "journey-state.json")
	t.Cleanup(func() { journeyStatePath = origPath })

	js := newJourneyStore()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	js.recordSent("hive-rt", nudge{Stage: StageACMMLevel, Severity: SeverityGentle}, 4, now)
	js.setSnooze("hive-snoozed", now.Add(48*time.Hour), "admin", "reason")

	if err := js.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	js2 := newJourneyStore()
	if err := js2.load(now); err != nil {
		t.Fatalf("load: %v", err)
	}

	st := js2.get("hive-rt")
	if st == nil || st.LastACMMLevel != 4 {
		t.Fatalf("loaded state mismatch: %+v", st)
	}
	snoozed := js2.get("hive-snoozed")
	if snoozed == nil || snoozed.SnoozedBy != "admin" {
		t.Fatalf("loaded snooze mismatch: %+v", snoozed)
	}
}

func TestJourneyStore_SaveNoopWhenClean(t *testing.T) {
	tmpDir := t.TempDir()
	origPath := journeyStatePath
	journeyStatePath = filepath.Join(tmpDir, "journey-state.json")
	t.Cleanup(func() { journeyStatePath = origPath })

	js := newJourneyStore()
	// No mutations — save should be a no-op.
	if err := js.save(); err != nil {
		t.Fatalf("clean save should not error: %v", err)
	}
	if _, err := os.Stat(journeyStatePath); !os.IsNotExist(err) {
		t.Fatal("file should not exist when nothing was dirty")
	}
}

func TestJourneyStore_LoadMissingFileIsOK(t *testing.T) {
	origPath := journeyStatePath
	journeyStatePath = filepath.Join(t.TempDir(), "nonexistent.json")
	t.Cleanup(func() { journeyStatePath = origPath })

	js := newJourneyStore()
	if err := js.load(time.Now()); err != nil {
		t.Fatalf("load of missing file should not error: %v", err)
	}
}

func TestJourneyStore_LoadCorruptFileReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	origPath := journeyStatePath
	journeyStatePath = filepath.Join(tmpDir, "journey-state.json")
	t.Cleanup(func() { journeyStatePath = origPath })

	os.WriteFile(journeyStatePath, []byte("{not json"), 0o644)
	js := newJourneyStore()
	if err := js.load(time.Now()); err == nil {
		t.Fatal("load of corrupt file should return error")
	}
}

func TestJourneyStore_LoadDropsExpiredSnooze(t *testing.T) {
	tmpDir := t.TempDir()
	origPath := journeyStatePath
	journeyStatePath = filepath.Join(tmpDir, "journey-state.json")
	t.Cleanup(func() { journeyStatePath = origPath })

	// Write a state file with an already-expired snooze.
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := map[string]*JourneyState{
		"hive-expired": {
			SnoozedUntil: past.Format(time.RFC3339),
			SnoozedBy:    "old-admin",
			SnoozeReason: "old reason",
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(journeyStatePath, data, 0o644)

	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) // well past snooze expiry
	js := newJourneyStore()
	if err := js.load(now); err != nil {
		t.Fatalf("load: %v", err)
	}

	st := js.get("hive-expired")
	if st == nil {
		t.Fatal("expected state entry to exist")
	}
	if st.SnoozedUntil != "" || st.SnoozedBy != "" {
		t.Errorf("expired snooze should have been cleared on load, got %+v", st)
	}
}

// ---------------------------------------------------------------------------
// snoozeActive
// ---------------------------------------------------------------------------

func TestJourneyState_SnoozeActive(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		state  *JourneyState
		active bool
	}{
		{"nil state", nil, false},
		{"empty snooze", &JourneyState{}, false},
		{"future snooze", &JourneyState{SnoozedUntil: now.Add(time.Hour).Format(time.RFC3339)}, true},
		{"past snooze", &JourneyState{SnoozedUntil: now.Add(-time.Hour).Format(time.RFC3339)}, false},
		{"corrupt timestamp", &JourneyState{SnoozedUntil: "not-a-timestamp"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.snoozeActive(now); got != tt.active {
				t.Errorf("snoozeActive = %v, want %v", got, tt.active)
			}
		})
	}
}
