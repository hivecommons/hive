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

func TestCollectFreshStatsWithTimeout_FastCollectReturnsPayload(t *testing.T) {
	want := &HeartbeatPayload{HiveID: "fresh"}
	got := collectFreshStatsWithTimeout(context.Background(), func() *HeartbeatPayload {
		return want
	}, time.Second, nil2Logger())
	if got != want {
		t.Fatalf("collectFreshStatsWithTimeout returned %+v, want %+v", got, want)
	}
}

func TestCollectFreshStatsWithTimeout_BlockedCollectReturnsNil(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	got := collectFreshStatsWithTimeout(context.Background(), func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "late"}
	}, 20*time.Millisecond, nil2Logger())
	if got != nil {
		t.Fatalf("expected nil from timed-out fresh collect, got %+v", got)
	}
}

func TestCollectFreshStatsWithTimeout_PanicRecovered(t *testing.T) {
	got := collectFreshStatsWithTimeout(context.Background(), func() *HeartbeatPayload {
		panic("boom")
	}, time.Second, nil2Logger())
	if got != nil {
		t.Fatalf("expected nil after panicking fresh collect, got %+v", got)
	}
}

func TestCollectFreshStatsWithTimeout_ContextCancelReturnsNil(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := collectFreshStatsWithTimeout(ctx, func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "late"}
	}, time.Hour, nil2Logger())
	if got != nil {
		t.Fatalf("expected nil after context cancellation, got %+v", got)
	}
}

func TestOverlayFreshStatsAfterCollectTimeoutCopiesFreshFieldsAndClearsCached(t *testing.T) {
	payload := &HeartbeatPayload{
		HiveID:                 "hive",
		ACMMLevel:              6,
		DashboardURL:           "https://cached.example",
		Agents:                 []AgentSummary{{Name: "quality", State: "stopped"}},
		Health:                 map[string]any{"status": "ok"},
		Governor:               GovernorSummary{Mode: "old", Issues: 9, PRs: 8, WorkSource: "jira"},
		ProviderLimitReason:    "cached provider limit",
		ProviderLimitRebuffs:   3,
		ProviderLimitHiveWide:  true,
		ProviderLimitAgents:    []string{"quality"},
		LastWriteCapableKickAt: "2026-09-11T15:00:00Z",
		LastKickDisposition:    "cached",
		LastKickSkipReason:     "cached skip",
		NotWritableQueued:      7,
		NoCadenceAgents:        []string{"quality"},
		ConsentWedged:          []string{"quality"},
		AgentErrorStreaks:      map[string]int{"quality": 5},
	}

	overlayFreshStatsAfterCollectTimeout(context.Background(), payload, nil2Logger(), func() *HeartbeatPayload {
		return &HeartbeatPayload{
			ACMMLevel:              5,
			DashboardURL:           "https://fresh.example",
			Agents:                 []AgentSummary{{Name: "quality", State: "running"}},
			Health:                 map[string]any{"status": "unknown"},
			Governor:               GovernorSummary{Mode: "active", Issues: 1, PRs: 2, WorkSource: "linear"},
			ProviderLimitAgents:    []string{},
			LastWriteCapableKickAt: "",
			LastKickDisposition:    "",
			LastKickSkipReason:     "",
			NotWritableQueued:      0,
			NoCadenceAgents:        []string{},
			ConsentWedged:          []string{},
			AgentErrorStreaks:      map[string]int{},
			ProviderLimitReason:    "",
			ProviderLimitRebuffs:   0,
			ProviderLimitHiveWide:  false,
		}
	})

	if !payload.FreshAgentStats {
		t.Fatal("FreshAgentStats = false, want true after agents/health overlay")
	}
	if payload.ACMMLevel != 5 || payload.DashboardURL != "https://fresh.example" {
		t.Fatalf("identity-ish fresh fields = L%d %q, want L5 fresh URL", payload.ACMMLevel, payload.DashboardURL)
	}
	if len(payload.Agents) != 1 || payload.Agents[0].State != "running" {
		t.Fatalf("Agents = %+v, want fresh running agent", payload.Agents)
	}
	if payload.Health["status"] != "unknown" {
		t.Fatalf("Health = %+v, want fresh unknown health", payload.Health)
	}
	if payload.Governor.Mode != "active" || payload.Governor.Issues != 1 || payload.Governor.PRs != 2 || payload.Governor.WorkSource != "linear" {
		t.Fatalf("Governor = %+v, want fresh governor", payload.Governor)
	}
	if payload.ProviderLimitReason != "" || payload.ProviderLimitRebuffs != 0 || payload.ProviderLimitHiveWide || len(payload.ProviderLimitAgents) != 0 {
		t.Fatalf("provider limit fields not cleared: %q %d %v %v", payload.ProviderLimitReason, payload.ProviderLimitRebuffs, payload.ProviderLimitHiveWide, payload.ProviderLimitAgents)
	}
	if payload.LastWriteCapableKickAt != "" || payload.LastKickDisposition != "" || payload.LastKickSkipReason != "" || payload.NotWritableQueued != 0 {
		t.Fatalf("output freshness fields not cleared: %q %q %q %d", payload.LastWriteCapableKickAt, payload.LastKickDisposition, payload.LastKickSkipReason, payload.NotWritableQueued)
	}
	if payload.NoCadenceAgents == nil || len(payload.NoCadenceAgents) != 0 {
		t.Fatalf("NoCadenceAgents = %#v, want measured empty slice", payload.NoCadenceAgents)
	}
	if payload.ConsentWedged == nil || len(payload.ConsentWedged) != 0 {
		t.Fatalf("ConsentWedged = %#v, want measured empty slice", payload.ConsentWedged)
	}
	if payload.AgentErrorStreaks == nil || len(payload.AgentErrorStreaks) != 0 {
		t.Fatalf("AgentErrorStreaks = %#v, want measured empty map", payload.AgentErrorStreaks)
	}
}

func TestOverlayFreshStatsAfterCollectTimeoutUnavailableLeavesCachedPayload(t *testing.T) {
	base := HeartbeatPayload{
		HiveID:              "hive",
		ACMMLevel:           6,
		DashboardURL:        "https://cached.example",
		Agents:              []AgentSummary{{Name: "quality", State: "stopped"}},
		Health:              map[string]any{"status": "ok"},
		ProviderLimitReason: "cached provider limit",
	}
	for _, tc := range []struct {
		name      string
		payload   *HeartbeatPayload
		collector []FreshStatusCollector
	}{
		{name: "nil payload", payload: nil, collector: []FreshStatusCollector{func() *HeartbeatPayload { return &HeartbeatPayload{} }}},
		{name: "no collector", payload: clonePayload(&base), collector: nil},
		{name: "nil collector", payload: clonePayload(&base), collector: []FreshStatusCollector{nil}},
		{name: "collector returns nil", payload: clonePayload(&base), collector: []FreshStatusCollector{func() *HeartbeatPayload { return nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			overlayFreshStatsAfterCollectTimeout(context.Background(), tc.payload, nil2Logger(), tc.collector...)
			if tc.payload == nil {
				return
			}
			if tc.payload.FreshAgentStats {
				t.Fatal("FreshAgentStats = true, want false when overlay unavailable")
			}
			if tc.payload.ACMMLevel != base.ACMMLevel || tc.payload.DashboardURL != base.DashboardURL || tc.payload.ProviderLimitReason != base.ProviderLimitReason {
				t.Fatalf("payload changed without overlay: %+v", tc.payload)
			}
			if len(tc.payload.Agents) != 1 || tc.payload.Agents[0].State != "stopped" || tc.payload.Health["status"] != "ok" {
				t.Fatalf("agent/health changed without overlay: %+v %+v", tc.payload.Agents, tc.payload.Health)
			}
		})
	}
}

func TestOverlayFreshStatsAfterCollectTimeoutHealthOnlyMarksFresh(t *testing.T) {
	payload := &HeartbeatPayload{
		Agents: []AgentSummary{{Name: "quality", State: "stopped"}},
		Health: map[string]any{"status": "ok"},
	}
	overlayFreshStatsAfterCollectTimeout(context.Background(), payload, nil2Logger(), func() *HeartbeatPayload {
		return &HeartbeatPayload{Health: map[string]any{"status": "unknown"}}
	})
	if !payload.FreshAgentStats {
		t.Fatal("FreshAgentStats = false, want true for health-only overlay")
	}
	if len(payload.Agents) != 1 || payload.Agents[0].State != "stopped" {
		t.Fatalf("Agents = %+v, want cached agents left alone on health-only overlay", payload.Agents)
	}
	if payload.Health["status"] != "unknown" {
		t.Fatalf("Health = %+v, want fresh health", payload.Health)
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
