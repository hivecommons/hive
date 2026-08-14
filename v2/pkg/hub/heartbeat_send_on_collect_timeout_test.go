package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// These tests are the positive control for the OFFLINE regression fix. #3743
// bounded collect() so a wedged collector can't freeze the heartbeat loop, but
// on timeout sendHeartbeat returned WITHOUT POSTing anything. On a spoke whose
// /data (NFS) is persistently wedged, collect() times out every cycle, so
// EVERY beat was skipped, the hub received no heartbeats, and it marked an
// alive, 1/1-Ready hive OFFLINE after the stale threshold. The fix: on a
// timed-out collect, send the LAST-GOOD cached payload (marked StatsStale)
// instead of skipping — so the hub still gets a live beat and keeps the hive
// online. Only the first beats, before any collect has ever succeeded, skip.

// blockUntilClosed returns a collect() that blocks until ch is closed, then
// returns the given payload. It models an NFS-wedged AllStatuses() that never
// returns within the heartbeat timeout.
func blockUntilClosed(ch <-chan struct{}, p *HeartbeatPayload) StatusCollector {
	return func() *HeartbeatPayload {
		<-ch
		return p
	}
}

// TestSendHeartbeat_TimedOutCollectStillPostsCachedPayload is the core positive
// control: after one successful collect populates the cache, a subsequent beat
// whose collect() times out STILL POSTs to the hub (carrying the cached hive
// identity) rather than skipping. Without the fix the second POST never
// happens and the hub goes stale.
func TestSendHeartbeat_TimedOutCollectStillPostsCachedPayload(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	var posts atomic.Int32
	lastHiveID := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var p HeartbeatPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		lastHiveID <- p.HiveID
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()

	// Beat 1: a fast collect populates the last-good cache and is delivered.
	sendHeartbeat(ctx, server.URL, func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "wedged-hive", Org: "org", PrimaryRepo: "repo"}
	}, nil2Logger())

	if got := posts.Load(); got != 1 {
		t.Fatalf("expected 1 POST after the fresh beat, got %d", got)
	}
	if id := <-lastHiveID; id != "wedged-hive" {
		t.Fatalf("fresh beat carried hive_id %q, want wedged-hive", id)
	}

	// Beat 2: collect() is wedged (times out). The beat MUST still POST, using
	// the cached identity, so the hub keeps the hive online.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	sendHeartbeat(ctx, server.URL, blockUntilClosed(release, &HeartbeatPayload{HiveID: "late"}), nil2Logger())

	if got := posts.Load(); got != 2 {
		t.Fatalf("timed-out collect did NOT POST a heartbeat (posts=%d) — the hub would receive nothing and mark the hive OFFLINE", got)
	}
	if id := <-lastHiveID; id != "wedged-hive" {
		t.Fatalf("timed-out beat carried hive_id %q, want the cached wedged-hive identity", id)
	}
}

// TestSendHeartbeat_TimedOutCollectMarksStatsStale proves the beat sent on a
// collect timeout is flagged StatsStale, so the hub keeps the hive online but
// does not treat the cached agent/fleet numbers as freshly observed.
func TestSendHeartbeat_TimedOutCollectMarksStatsStale(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	stale := make(chan bool, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p HeartbeatPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		stale <- p.StatsStale
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()

	// Fresh beat: cache populated, must NOT be marked stale.
	sendHeartbeat(ctx, server.URL, func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "hive", Org: "org", PrimaryRepo: "repo"}
	}, nil2Logger())
	if got := <-stale; got {
		t.Fatalf("fresh beat was marked StatsStale=true, want false")
	}

	// Timed-out beat: cached stats, must be marked stale.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sendHeartbeat(ctx, server.URL, blockUntilClosed(release, &HeartbeatPayload{HiveID: "late"}), nil2Logger())
	if got := <-stale; !got {
		t.Fatalf("beat sent on a timed-out collect was NOT marked StatsStale — the hub would treat cached numbers as fresh")
	}
}

// TestSendHeartbeat_TimedOutCollectStillAdvancesAttempt proves the #3743
// liveness win is preserved: even on the cached-payload path, the attempt clock
// advances and (because the POST now happens) success advances too — so
// /api/livez stays green AND the hub's lastHeartbeat advances.
func TestSendHeartbeat_TimedOutCollectStillAdvancesAttempt(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()

	// Prime the cache with one good beat.
	sendHeartbeat(ctx, server.URL, func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "hive", Org: "org", PrimaryRepo: "repo"}
	}, nil2Logger())

	before := time.Now().Truncate(time.Second)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sendHeartbeat(ctx, server.URL, blockUntilClosed(release, &HeartbeatPayload{HiveID: "late"}), nil2Logger())

	att, ok := LastHeartbeatAttempt()
	if !ok || att.Before(before) {
		t.Errorf("LastHeartbeatAttempt() = (%v, %v), want a fresh attempt at/after %v even though collect() timed out", att, ok, before)
	}
	suc, ok := LastHeartbeatSuccess()
	if !ok || suc.Before(before) {
		t.Errorf("LastHeartbeatSuccess() = (%v, %v), want a fresh success — the cached beat WAS accepted by the hub, keeping the hive online", suc, ok)
	}
}

// TestSendHeartbeat_FirstBeatTimeoutWithNoCacheSkips proves the one case that
// still skips: a collect() that times out before ANY collect has ever
// succeeded has no cached identity to send, so it skips the beat (but the loop
// still ticks — attempt advanced, no success).
func TestSendHeartbeat_FirstBeatTimeoutWithNoCacheSkips(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	before := time.Now().Truncate(time.Second)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	done := make(chan struct{})
	go func() {
		sendHeartbeat(context.Background(), server.URL, blockUntilClosed(release, &HeartbeatPayload{HiveID: "late"}), nil2Logger())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(heartbeatTimeout + 5*time.Second):
		t.Fatal("sendHeartbeat did not return on a first-beat wedged collect — the loop would freeze")
	}

	if got := posts.Load(); got != 0 {
		t.Fatalf("first-beat timeout with no cache should POST nothing, got %d POSTs", got)
	}
	att, ok := LastHeartbeatAttempt()
	if !ok || att.Before(before) {
		t.Errorf("LastHeartbeatAttempt() = (%v, %v), want a fresh attempt even on a skipped first beat", att, ok)
	}
	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("LastHeartbeatSuccess() ok = true, want false: the first beat was skipped, nothing was delivered")
	}
}

// TestClonePayload_DeepCopyIndependence proves the cache stores an independent
// deep copy: mutating slices/maps on the clone must not touch the original (and
// vice-versa), which is what makes the cache safe to read from the sender while
// the collect goroutine writes a fresh one.
func TestClonePayload_DeepCopyIndependence(t *testing.T) {
	if clonePayload(nil) != nil {
		t.Fatal("clonePayload(nil) must be nil")
	}
	orig := &HeartbeatPayload{
		HiveID:             "hive",
		Agents:             []AgentSummary{{Name: "scanner", State: "running"}},
		ActiveSessionUsers: []string{"alice"},
		UserLastActions:    map[string]string{"alice": "2026-01-01T00:00:00Z"},
	}
	c := clonePayload(orig)
	if c == nil {
		t.Fatal("clonePayload returned nil for a valid payload")
	}
	c.Agents[0].Name = "mutated"
	c.ActiveSessionUsers[0] = "mutated"
	c.UserLastActions["alice"] = "mutated"
	if orig.Agents[0].Name != "scanner" {
		t.Errorf("mutating the clone's Agents changed the original: %q", orig.Agents[0].Name)
	}
	if orig.ActiveSessionUsers[0] != "alice" {
		t.Errorf("mutating the clone's ActiveSessionUsers changed the original: %q", orig.ActiveSessionUsers[0])
	}
	if orig.UserLastActions["alice"] != "2026-01-01T00:00:00Z" {
		t.Errorf("mutating the clone's UserLastActions changed the original: %q", orig.UserLastActions["alice"])
	}
}
