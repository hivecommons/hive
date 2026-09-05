package spoke

import (
	"context"
	"testing"
	"time"
)

// These tests pin the #2439 invariant: the heartbeat *attempt* clock advances
// on every tick BEFORE any work that can hang or fail (collecting status,
// posting to the hub). This is what keeps /api/livez healthy on a GHE-hosted
// hive whose GitHub App is not installed — the loop is demonstrably alive
// (attempts advancing) even though it cannot reach the hub or mint a token, so
// the pod is not restarted and the rollout can finish.

// TestSendHeartbeat_StampsAttemptBeforeCollect proves the attempt timestamp is
// recorded before collect() runs, so a collect that does slow/failing work (or
// a hub that is unreachable) never freezes the attempt clock.
func TestSendHeartbeat_StampsAttemptBeforeCollect(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	var attemptAtCollect time.Time
	var hadAttemptAtCollect bool

	collect := func() *HeartbeatPayload {
		// Observe the attempt clock from INSIDE collect: it must already be set,
		// proving recordHeartbeatAttempt ran first. Simulate the collect doing
		// slow external work (the pattern that used to freeze the clock).
		attemptAtCollect, hadAttemptAtCollect = LastHeartbeatAttempt()
		// Return nil so sendHeartbeat bails before any network I/O — we only
		// care that the attempt was already stamped.
		return nil
	}

	before := time.Now().Truncate(time.Second)
	// Unreachable hub URL is irrelevant here: nil payload short-circuits before
	// the POST. The point is the attempt stamp precedes collect entirely.
	sendHeartbeat(context.Background(), "http://127.0.0.1:1", collect, nil2Logger())

	if !hadAttemptAtCollect {
		t.Fatal("attempt clock was NOT set when collect() ran: recordHeartbeatAttempt must precede collect so a slow/failing collect cannot freeze liveness")
	}
	if attemptAtCollect.Before(before) {
		t.Errorf("attempt observed inside collect = %v, want at or after %v", attemptAtCollect, before)
	}

	got, ok := LastHeartbeatAttempt()
	if !ok || got.Before(before) {
		t.Errorf("LastHeartbeatAttempt() = (%v, %v), want a fresh attempt at/after %v", got, ok, before)
	}
	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("LastHeartbeatSuccess() ok = true, want false: no beat reached the hub")
	}
}

// TestSendHeartbeat_UnreachableHubStillAdvancesAttempt is the network-partition
// (and no-app GHE) case at the send level: the hub POST fails, so no success is
// recorded, but the attempt clock still advanced — exactly what livez needs to
// keep the pod alive through an outage.
func TestSendHeartbeat_UnreachableHubStillAdvancesAttempt(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	collect := func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "test-hive"}
	}

	before := time.Now().Truncate(time.Second)
	// Port 1 is unreachable; the POST fails, success is never recorded.
	resp := sendHeartbeat(context.Background(), "http://127.0.0.1:1", collect, nil2Logger())
	if resp != nil {
		t.Fatalf("expected nil response from an unreachable hub, got %+v", resp)
	}

	got, ok := LastHeartbeatAttempt()
	if !ok || got.Before(before) {
		t.Errorf("LastHeartbeatAttempt() = (%v, %v), want a fresh attempt at/after %v even though the hub was unreachable", got, ok, before)
	}
	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("LastHeartbeatSuccess() ok = true, want false when the hub POST failed")
	}
}
