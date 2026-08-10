package hub

import (
	"testing"
	"time"
)

// ============================================================
// alertAckKey / alertAckKeyHive — key roundtrip and extraction
// ============================================================

func TestAlertAckKey_Roundtrip(t *testing.T) {
	tests := []struct {
		hiveID    string
		alertType string
	}{
		{"torch-spyre", "heartbeat-stale"},
		{"my-hive-123", "crash-loop"},
		{"", "empty-hive"},
	}
	for _, tc := range tests {
		key := alertAckKey(tc.hiveID, tc.alertType)
		got := alertAckKeyHive(key)
		if got != tc.hiveID {
			t.Errorf("alertAckKeyHive(alertAckKey(%q, %q)) = %q, want %q",
				tc.hiveID, tc.alertType, got, tc.hiveID)
		}
	}
}

func TestAlertAckKeyHive_NoSeparator(t *testing.T) {
	// A key with no separator returns the whole string as the hive ID.
	got := alertAckKeyHive("plain-string")
	if got != "plain-string" {
		t.Errorf("expected entire string, got %q", got)
	}
}

// ============================================================
// observeStart — restart counting
// ============================================================

func TestObserveStart_CountsDistinctRestarts(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	// First observation
	count := a.observeStart("hive-1", now.Add(-30*time.Minute).Format(time.RFC3339), now)
	if count != 1 {
		t.Errorf("first observation: got %d, want 1", count)
	}

	// Same start time (not a restart)
	count = a.observeStart("hive-1", now.Add(-30*time.Minute).Format(time.RFC3339), now)
	if count != 1 {
		t.Errorf("duplicate start time: got %d, want 1", count)
	}

	// New start time (restart detected)
	count = a.observeStart("hive-1", now.Add(-5*time.Minute).Format(time.RFC3339), now)
	if count != 2 {
		t.Errorf("second distinct start: got %d, want 2", count)
	}
}

func TestObserveStart_ExpireOldObservations(t *testing.T) {
	a := newAlertState()
	now := time.Now()
	old := now.Add(-3 * time.Hour) // beyond alertCrashLoopWindow (2h)

	// Record an observation that happened 3 hours ago
	a.observeStart("hive-1", old.Add(-10*time.Minute).Format(time.RFC3339), old)

	// New observation NOW — old one should be expired
	count := a.observeStart("hive-1", now.Format(time.RFC3339), now)
	if count != 1 {
		t.Errorf("after expiry: got %d, want 1 (old should be evicted)", count)
	}
}

func TestObserveStart_NilReceiver(t *testing.T) {
	var a *alertState
	got := a.observeStart("hive-1", time.Now().Format(time.RFC3339), time.Now())
	if got != 0 {
		t.Errorf("nil receiver should return 0, got %d", got)
	}
}

func TestObserveStart_EmptyInputs(t *testing.T) {
	a := newAlertState()
	now := time.Now()
	tests := []struct {
		name      string
		hiveID    string
		startedAt string
	}{
		{"empty_hive", "", now.Format(time.RFC3339)},
		{"empty_started", "hive-1", ""},
		{"whitespace_started", "hive-1", "   "},
		{"invalid_time", "hive-1", "not-a-time"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.observeStart(tc.hiveID, tc.startedAt, now)
			if got != 0 {
				t.Errorf("expected 0, got %d", got)
			}
		})
	}
}

func TestObserveStart_BoundsMemory(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	// Add more than alertStartTimesRetained (16) distinct restarts
	for i := 0; i < 20; i++ {
		startTime := now.Add(-time.Duration(20-i) * time.Minute)
		a.observeStart("hive-1", startTime.Format(time.RFC3339), now)
	}

	// Should be capped
	a.mu.Lock()
	count := len(a.startTimes["hive-1"])
	a.mu.Unlock()
	if count > alertStartTimesRetained {
		t.Errorf("startTimes not bounded: got %d, want <= %d", count, alertStartTimesRetained)
	}
}

// ============================================================
// markCondition — first-seen tracking
// ============================================================

func TestMarkCondition_RecordsFirstSeen(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	first := a.markCondition("hive-1", "heartbeat-stale", now)
	if !first.Equal(now) {
		t.Errorf("first markCondition should return now, got %v", first)
	}

	// Second call returns the ORIGINAL timestamp
	later := now.Add(5 * time.Minute)
	second := a.markCondition("hive-1", "heartbeat-stale", later)
	if !second.Equal(now) {
		t.Errorf("repeated markCondition should return original time %v, got %v", now, second)
	}
}

func TestMarkCondition_DifferentTypesAreIndependent(t *testing.T) {
	a := newAlertState()
	now := time.Now()
	later := now.Add(time.Minute)

	a.markCondition("hive-1", "heartbeat-stale", now)
	fs := a.markCondition("hive-1", "crash-loop", later)
	if !fs.Equal(later) {
		t.Errorf("different alert type should get its own firstSeen, got %v", fs)
	}
}

// ============================================================
// retainConditions — eviction of resolved conditions
// ============================================================

func TestRetainConditions_RemovesResolvedConditions(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	// Set up two conditions
	a.markCondition("hive-1", "heartbeat-stale", now)
	a.markCondition("hive-2", "crash-loop", now)

	// hive-1 was evaluated but its condition is no longer active
	evaluated := map[string]bool{"hive-1": true, "hive-2": true}
	active := map[string]bool{alertAckKey("hive-2", "crash-loop"): true}

	a.retainConditions(evaluated, active)

	// hive-1's condition should be evicted
	a.mu.Lock()
	_, h1exists := a.firstSeen[alertAckKey("hive-1", "heartbeat-stale")]
	_, h2exists := a.firstSeen[alertAckKey("hive-2", "crash-loop")]
	a.mu.Unlock()

	if h1exists {
		t.Error("hive-1 condition should have been evicted")
	}
	if !h2exists {
		t.Error("hive-2 condition should still exist (active)")
	}
}

func TestRetainConditions_SkipsUnevaluatedHives(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	a.markCondition("hive-hidden", "heartbeat-stale", now)

	// hive-hidden was NOT in the evaluated set (non-admin scope)
	evaluated := map[string]bool{"hive-other": true}
	active := map[string]bool{}

	a.retainConditions(evaluated, active)

	// Should NOT be evicted — wasn't evaluated
	a.mu.Lock()
	_, exists := a.firstSeen[alertAckKey("hive-hidden", "heartbeat-stale")]
	a.mu.Unlock()

	if !exists {
		t.Error("unevaluated hive's condition should be preserved")
	}
}

// ============================================================
// ackFor — acknowledgement validity
// ============================================================

func TestAckFor_ValidAck(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	// Set up a condition and acknowledge it
	a.markCondition("hive-1", "heartbeat-stale", now)
	a.setAck("hive-1", "heartbeat-stale", "admin@example.com", now)

	// Retrieve the ack
	ack, ok := a.ackFor("hive-1", "heartbeat-stale", now, now.Add(time.Minute))
	if !ok {
		t.Fatal("expected ack to be valid")
	}
	if ack.By != "admin@example.com" {
		t.Errorf("ack.By = %q, want admin@example.com", ack.By)
	}
}

func TestAckFor_ExpiredAck(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	a.markCondition("hive-1", "heartbeat-stale", now)
	a.setAck("hive-1", "heartbeat-stale", "admin", now)

	// Check after alertAckMaxAge (7 days) has passed
	_, ok := a.ackFor("hive-1", "heartbeat-stale", now, now.Add(8*24*time.Hour))
	if ok {
		t.Error("ack should be expired after 8 days")
	}
}

func TestAckFor_MismatchedFirstSeen(t *testing.T) {
	a := newAlertState()
	now := time.Now()

	// Ack was made against an older firstSeen
	a.markCondition("hive-1", "heartbeat-stale", now)
	a.setAck("hive-1", "heartbeat-stale", "admin", now)

	// Condition cleared and returned with a new firstSeen
	newFirstSeen := now.Add(time.Hour)
	_, ok := a.ackFor("hive-1", "heartbeat-stale", newFirstSeen, now.Add(time.Hour))
	if ok {
		t.Error("ack should not match a different firstSeen (new occurrence)")
	}
}

func TestAckFor_NoAckExists(t *testing.T) {
	a := newAlertState()
	_, ok := a.ackFor("hive-1", "heartbeat-stale", time.Now(), time.Now())
	if ok {
		t.Error("should return false when no ack exists")
	}
}

// ============================================================
// setAck — requires active condition
// ============================================================

func TestSetAck_RequiresActiveCondition(t *testing.T) {
	a := newAlertState()
	// Try to ack without an active condition
	ok := a.setAck("hive-1", "heartbeat-stale", "admin", time.Now())
	if ok {
		t.Error("setAck should fail when no condition is active")
	}
}

func TestSetAck_SucceedsWithActiveCondition(t *testing.T) {
	a := newAlertState()
	now := time.Now()
	a.markCondition("hive-1", "heartbeat-stale", now)

	ok := a.setAck("hive-1", "heartbeat-stale", "admin", now)
	if !ok {
		t.Error("setAck should succeed with active condition")
	}
}
