package spoke

import (
	"testing"
	"time"
)

// TestHeartbeatLivenessAccessors pins the distinction the liveness check relies
// on: LastHeartbeatAttempt advances even while the hub is unreachable, so a
// wedged goroutine (no attempts) is distinguishable from a network partition
// (attempts, no successes).
func TestHeartbeatLivenessAccessors(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)

	// Truncated to a second because the atomics store unix seconds; a
	// sub-second component could not survive the round trip.
	attempt := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	success := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	t.Run("never attempted reads as not-ok", func(t *testing.T) {
		ResetHeartbeatStateForTest()

		if got, ok := LastHeartbeatAttempt(); ok {
			t.Errorf("LastHeartbeatAttempt() = (%v, true), want ok=false before any attempt", got)
		}
		if got, ok := LastHeartbeatSuccess(); ok {
			t.Errorf("LastHeartbeatSuccess() = (%v, true), want ok=false before any success", got)
		}
		if HeartbeatEnabled() {
			t.Error("HeartbeatEnabled() = true, want false when no loop was started")
		}
	})

	t.Run("attempts without successes: partitioned network", func(t *testing.T) {
		SetHeartbeatStateForTest(true, attempt, time.Time{})

		got, ok := LastHeartbeatAttempt()
		if !ok {
			t.Fatal("LastHeartbeatAttempt() ok = false after recording an attempt")
		}
		if !got.Equal(attempt) {
			t.Errorf("LastHeartbeatAttempt() = %v, want %v", got, attempt)
		}
		if _, ok := LastHeartbeatSuccess(); ok {
			t.Error("LastHeartbeatSuccess() ok = true, want false: no beat has succeeded")
		}
		if !HeartbeatEnabled() {
			t.Error("HeartbeatEnabled() = false, want true when the loop is running")
		}
	})

	t.Run("both recorded: healthy loop", func(t *testing.T) {
		SetHeartbeatStateForTest(true, attempt, success)

		gotAttempt, ok := LastHeartbeatAttempt()
		if !ok || !gotAttempt.Equal(attempt) {
			t.Errorf("LastHeartbeatAttempt() = (%v, %v), want (%v, true)", gotAttempt, ok, attempt)
		}
		gotSuccess, ok := LastHeartbeatSuccess()
		if !ok || !gotSuccess.Equal(success) {
			t.Errorf("LastHeartbeatSuccess() = (%v, %v), want (%v, true)", gotSuccess, ok, success)
		}
	})
}

// TestResetHeartbeatStateForTest checks the reset hook really clears all three
// pieces of global state — tests that rely on it to avoid leaking into their
// neighbours depend on this.
func TestResetHeartbeatStateForTest(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)

	SetHeartbeatStateForTest(true, time.Now(), time.Now())
	if _, ok := LastHeartbeatAttempt(); !ok {
		t.Fatal("precondition: state was not set")
	}

	ResetHeartbeatStateForTest()

	if _, ok := LastHeartbeatAttempt(); ok {
		t.Error("LastHeartbeatAttempt() still ok after reset")
	}
	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("LastHeartbeatSuccess() still ok after reset")
	}
	if HeartbeatEnabled() {
		t.Error("HeartbeatEnabled() still true after reset")
	}
}

// TestRecordHeartbeatAttemptAdvances checks the recorder the loop calls on each
// send actually moves the timestamp forward from "never".
func TestRecordHeartbeatAttemptAdvances(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	before := time.Now().Truncate(time.Second)
	recordHeartbeatAttempt()

	got, ok := LastHeartbeatAttempt()
	if !ok {
		t.Fatal("LastHeartbeatAttempt() ok = false after recordHeartbeatAttempt()")
	}
	if got.Before(before) {
		t.Errorf("LastHeartbeatAttempt() = %v, want at or after %v", got, before)
	}
	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("recordHeartbeatAttempt() also marked a success, want attempts and successes independent")
	}
}

// TestRecordHeartbeatSuccessAdvances is the mirror of the above for the success
// timestamp.
func TestRecordHeartbeatSuccessAdvances(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	before := time.Now().Truncate(time.Second)
	recordHeartbeatSuccess()

	got, ok := LastHeartbeatSuccess()
	if !ok {
		t.Fatal("LastHeartbeatSuccess() ok = false after recordHeartbeatSuccess()")
	}
	if got.Before(before) {
		t.Errorf("LastHeartbeatSuccess() = %v, want at or after %v", got, before)
	}
	if _, ok := LastHeartbeatAttempt(); ok {
		t.Error("recordHeartbeatSuccess() also marked an attempt, want attempts and successes independent")
	}
}

// TestStoreUnixOrZero pins the zero-time convention the accessors depend on:
// the zero time stores as 0 so it reads back as "never happened", rather than
// as a 1970 timestamp that would look like a very stale but real beat.
func TestStoreUnixOrZero(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)

	real := time.Now().Add(-time.Hour).Truncate(time.Second)

	storeUnixOrZero(&lastHeartbeatAttemptUnix, real)
	if got := lastHeartbeatAttemptUnix.Load(); got != real.Unix() {
		t.Errorf("stored %d, want %d", got, real.Unix())
	}

	storeUnixOrZero(&lastHeartbeatAttemptUnix, time.Time{})
	if got := lastHeartbeatAttemptUnix.Load(); got != 0 {
		t.Errorf("zero time stored as %d, want 0", got)
	}
	if _, ok := LastHeartbeatAttempt(); ok {
		t.Error("LastHeartbeatAttempt() ok = true after storing the zero time, want never-happened")
	}
}
