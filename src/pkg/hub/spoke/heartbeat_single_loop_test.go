package spoke

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the #2453 single-loop guard through the PRODUCTION
// StartHeartbeat path: a real send loop against an httptest hub, with the
// readiness wait collapsed so the loop is beating within milliseconds.

// singleLoopTestInterval is short enough that several ticks fit in a
// sub-second observation window, long enough that the scheduler reliably
// delivers every tick even on a loaded CI runner.
const singleLoopTestInterval = 50 * time.Millisecond

// collapseReadinessWait shrinks waitForReady's knobs for the duration of a
// test so StartHeartbeat reaches its send loop immediately instead of polling
// localhost:3001 for up to 3 minutes.
func collapseReadinessWait(t *testing.T) {
	t.Helper()
	prevPoll, prevMax := waitForReadyPollInterval, waitForReadyMaxWait
	waitForReadyPollInterval = time.Millisecond
	waitForReadyMaxWait = 0
	t.Cleanup(func() {
		waitForReadyPollInterval = prevPoll
		waitForReadyMaxWait = prevMax
	})
}

// resetSingleLoopGuard clears the process-global heartbeat state so one
// test's loop cannot bleed into the next.
func resetSingleLoopGuard(t *testing.T) {
	t.Helper()
	heartbeatLoopActive.Store(false)
	ResetHeartbeatStateForTest()
	t.Cleanup(func() {
		heartbeatLoopActive.Store(false)
		ResetHeartbeatStateForTest()
	})
}

// countingHub is an httptest hub that counts every heartbeat POST it accepts.
func countingHub(t *testing.T, beats *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/heartbeat" {
			beats.Add(1)
		}
		json.NewEncoder(w).Encode(HeartbeatResponse{OK: true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitForBeats blocks until the hub has seen at least n beats, or fails the
// test after a generous deadline.
func waitForBeats(t *testing.T, beats *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for beats.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("hub saw %d beats, wanted at least %d", beats.Load(), n)
		case <-tick.C:
		}
	}
}

// TestStartHeartbeatSecondConcurrentCallRefused is the #2453 regression test:
// two concurrent StartHeartbeat invocations must yield exactly ONE active
// loop. The refused call is observable in two independent ways — it returns
// promptly while the first loop is still running (an un-refused call blocks
// until its context is cancelled), and the beat count over a multi-interval
// window matches a single cadence, not two.
func TestStartHeartbeatSecondConcurrentCallRefused(t *testing.T) {
	resetSingleLoopGuard(t)
	collapseReadinessWait(t)

	var beats atomic.Int32
	srv := countingHub(t, &beats)
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h-2453"} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstDone := make(chan struct{})
	go func() {
		StartHeartbeat(ctx, srv.URL, collect, singleLoopTestInterval, slog.Default())
		close(firstDone)
	}()

	// The first loop is beating.
	waitForBeats(t, &beats, 1)

	// Second concurrent invocation: with the guard it is refused and returns
	// immediately; without it this call runs a second loop and blocks here
	// until ctx is cancelled — which is exactly the production double-sender.
	secondDone := make(chan struct{})
	go func() {
		StartHeartbeat(ctx, srv.URL, collect, singleLoopTestInterval, slog.Default())
		close(secondDone)
	}()
	select {
	case <-secondDone:
		// refused, as required
	case <-time.After(3 * time.Second):
		t.Fatal("second concurrent StartHeartbeat did not return: the duplicate loop was NOT refused (two senders are running)")
	}

	// Cadence check over several intervals: one loop produces ~window/interval
	// beats; a second loop would double that. The ceiling sits between the two
	// so a duplicate sender fails even with generous scheduler jitter.
	beats.Store(0)
	const window = 12 * singleLoopTestInterval
	<-time.After(window)
	got := beats.Load()
	if got == 0 {
		t.Fatal("no beats observed after the refusal — the FIRST loop must keep running")
	}
	if max := int32(window/singleLoopTestInterval) + 3; got > max {
		t.Fatalf("observed %d beats in %v (interval %v) — more than one heartbeat cadence is on the wire", got, window, singleLoopTestInterval)
	}

	// A refused duplicate must not have broken the /api/livez contract.
	if !HeartbeatEnabled() {
		t.Error("HeartbeatEnabled() must remain true while the surviving loop runs")
	}

	cancel()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first loop did not stop on context cancellation")
	}
}

// TestStartHeartbeatGuardReleasedAfterLoopExit proves the guard is a "running
// now" flag, not a "ran once" latch: after the first loop exits on context
// cancellation, a fresh StartHeartbeat must run (not be refused) and beat.
func TestStartHeartbeatGuardReleasedAfterLoopExit(t *testing.T) {
	resetSingleLoopGuard(t)
	collapseReadinessWait(t)

	var beats atomic.Int32
	srv := countingHub(t, &beats)
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h-restart"} }

	ctx1, cancel1 := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		StartHeartbeat(ctx1, srv.URL, collect, singleLoopTestInterval, slog.Default())
		close(firstDone)
	}()
	waitForBeats(t, &beats, 1)
	cancel1()
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first loop did not stop on context cancellation")
	}

	// Restart: must be admitted, and must beat again.
	beats.Store(0)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	secondDone := make(chan struct{})
	go func() {
		StartHeartbeat(ctx2, srv.URL, collect, singleLoopTestInterval, slog.Default())
		close(secondDone)
	}()
	waitForBeats(t, &beats, 1)

	cancel2()
	select {
	case <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("restarted loop did not stop on context cancellation")
	}
}
