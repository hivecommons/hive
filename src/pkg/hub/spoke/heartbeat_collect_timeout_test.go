package spoke

import (
	"context"
	"sync"
	"testing"
	"time"
)

// These tests are the positive control for the NFS -> m.mu -> collect() ->
// frozen-liveness stall fix. collect() reads the agent manager under m.mu.RLock
// (Manager.AllStatuses); if another goroutine holds m.mu.Lock() while blocked
// in an uninterruptible NFS write to /data, collect() blocks with no timeout of
// its own. Because sendHeartbeat runs synchronously in the heartbeat loop's
// for-select, a stuck collect() means sendHeartbeat never returns, the loop
// never ticks again, and recordHeartbeatAttempt() stops advancing — after
// livezHeartbeatStallMax /api/livez reports the loop as stalled and kubelet
// kills an otherwise-alive pod. collectWithTimeout bounds collect() so the loop
// keeps ticking and liveness stays green.

// TestCollectWithTimeout_BlockedCollectReturnsAndDoesNotBlock is the core
// positive control: a collect() that never returns must NOT hang
// collectWithTimeout — it returns nil (skip this beat) at the timeout, well
// before the test's own deadline.
func TestCollectWithTimeout_BlockedCollectReturnsAndDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // unblock the leaked goroutine at test end

	blocked := func() *HeartbeatPayload {
		<-release // simulate an NFS-wedged AllStatuses() that never returns in time
		return &HeartbeatPayload{HiveID: "late"}
	}

	const timeout = 50 * time.Millisecond
	done := make(chan *HeartbeatPayload, 1)
	go func() {
		done <- collectWithTimeout(context.Background(), blocked, timeout, nil2Logger())
	}()

	select {
	case payload := <-done:
		if payload != nil {
			t.Fatalf("expected nil (skipped beat) from a blocked collect, got %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collectWithTimeout did NOT return on a blocked collect — the heartbeat loop would freeze and liveness would 503")
	}
}

// TestCollectWithTimeout_FastCollectReturnsPayload proves the happy path is
// unchanged: a collect() that finishes within the timeout returns its payload.
func TestCollectWithTimeout_FastCollectReturnsPayload(t *testing.T) {
	want := &HeartbeatPayload{HiveID: "hive-1"}
	got := collectWithTimeout(context.Background(), func() *HeartbeatPayload {
		return want
	}, time.Second, nil2Logger())
	if got != want {
		t.Fatalf("collectWithTimeout returned %+v, want %+v", got, want)
	}
}

// TestCollectWithTimeout_PanicRecovered proves a panicking collect() does not
// crash the heartbeat goroutine — it is reported as a skipped (nil) beat.
func TestCollectWithTimeout_PanicRecovered(t *testing.T) {
	got := collectWithTimeout(context.Background(), func() *HeartbeatPayload {
		panic("boom")
	}, time.Second, nil2Logger())
	if got != nil {
		t.Fatalf("expected nil after a panicking collect, got %+v", got)
	}
}

// TestCollectWithTimeout_LateGoroutineDoesNotLeakPermanently proves the timed-
// out collect goroutine can still complete its send after collectWithTimeout
// has returned (the channel is buffered), so once the NFS blocker clears the
// goroutine exits rather than leaking forever.
func TestCollectWithTimeout_LateGoroutineDoesNotLeakPermanently(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	exited := make(chan struct{})

	blocked := func() *HeartbeatPayload {
		<-release
		once.Do(func() { close(exited) })
		return &HeartbeatPayload{HiveID: "late"}
	}

	got := collectWithTimeout(context.Background(), blocked, 20*time.Millisecond, nil2Logger())
	if got != nil {
		t.Fatalf("expected nil (timed out), got %+v", got)
	}

	// Now let the wedged collect finish, as a real NFS server eventually would.
	close(release)
	select {
	case <-exited:
		// Goroutine completed its buffered send and returned — no permanent leak.
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out collect goroutine never completed after its blocker cleared — permanent goroutine leak")
	}
}

// TestCollectWithTimeout_ContextCancelReturns proves a cancelled context short-
// circuits the wait (loop shutdown must not block on a wedged collect).
func TestCollectWithTimeout_ContextCancelReturns(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := collectWithTimeout(ctx, func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{}
	}, time.Hour, nil2Logger())
	if got != nil {
		t.Fatalf("expected nil on a cancelled context, got %+v", got)
	}
}

// TestSendHeartbeat_BlockedCollectStillAdvancesAttemptAndReturns is the end-to-
// end positive control at the sendHeartbeat level: even when collect() is
// wedged, recordHeartbeatAttempt has run (attempt clock advanced) AND
// sendHeartbeat returns (the loop keeps ticking) — the exact combination that
// keeps /api/livez green through an NFS stall.
func TestSendHeartbeat_BlockedCollectStillAdvancesAttemptAndReturns(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	blocked := func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "late"}
	}

	before := time.Now().Truncate(time.Second)

	done := make(chan *HeartbeatResponse, 1)
	go func() {
		done <- sendHeartbeat(context.Background(), "http://127.0.0.1:1", blocked, nil2Logger())
	}()

	select {
	case resp := <-done:
		if resp != nil {
			t.Fatalf("expected nil response when collect() is wedged, got %+v", resp)
		}
	case <-time.After(heartbeatTimeout + 5*time.Second):
		t.Fatal("sendHeartbeat did not return with a wedged collect — the heartbeat loop would freeze and liveness would 503")
	}

	got, ok := LastHeartbeatAttempt()
	if !ok || got.Before(before) {
		t.Errorf("LastHeartbeatAttempt() = (%v, %v), want a fresh attempt at/after %v even though collect() was wedged", got, ok, before)
	}
	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("LastHeartbeatSuccess() ok = true, want false: the beat was skipped, not delivered")
	}
}
