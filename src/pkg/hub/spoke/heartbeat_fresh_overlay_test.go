package spoke

// Unit coverage for the fresh-stats overlay helpers behind the collect-timeout
// fix. These pin the guard rails: a panicking or wedged fresh collector must
// degrade to "no overlay" (stale cached stats still beat), never to a crashed
// or frozen heartbeat loop, and the overlay must only replace sections the
// fresh payload actually carries.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newRejectingHub returns the URL of a hub that rejects every beat with 403.
func newRejectingHub(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestCollectFreshStatsWithTimeoutReturnsThePayload(t *testing.T) {
	want := &HeartbeatPayload{HiveID: "fresh"}
	got := collectFreshStatsWithTimeout(context.Background(), func() *HeartbeatPayload { return want }, time.Second, nil2Logger())
	if got != want {
		t.Fatalf("collectFreshStatsWithTimeout = %+v, want the collector's payload", got)
	}
}

func TestCollectFreshStatsWithTimeoutTimesOutToNil(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	got := collectFreshStatsWithTimeout(context.Background(), func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "late"}
	}, 10*time.Millisecond, nil2Logger())
	if got != nil {
		t.Fatalf("collectFreshStatsWithTimeout = %+v, want nil on timeout", got)
	}
}

func TestCollectFreshStatsWithTimeoutSurvivesAPanickingCollector(t *testing.T) {
	got := collectFreshStatsWithTimeout(context.Background(), func() *HeartbeatPayload {
		panic("wedged collector")
	}, time.Second, nil2Logger())
	if got != nil {
		t.Fatalf("collectFreshStatsWithTimeout = %+v, want nil after a collector panic", got)
	}
}

func TestCollectFreshStatsWithTimeoutHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	got := collectFreshStatsWithTimeout(ctx, func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "late"}
	}, time.Second, nil2Logger())
	if got != nil {
		t.Fatalf("collectFreshStatsWithTimeout = %+v, want nil on cancelled context", got)
	}
}

func TestOverlayFreshStatsIsInertWithoutACollector(t *testing.T) {
	// nil payload, no collectors, and a nil first collector must all no-op.
	overlayFreshStatsAfterCollectTimeout(context.Background(), nil, nil2Logger())
	payload := &HeartbeatPayload{HiveID: "cached"}
	overlayFreshStatsAfterCollectTimeout(context.Background(), payload, nil2Logger())
	overlayFreshStatsAfterCollectTimeout(context.Background(), payload, nil2Logger(), nil)
	if payload.FreshAgentStats {
		t.Fatal("overlay without a collector must not mark stats fresh")
	}
}

func TestOverlayFreshStatsLeavesPayloadUntouchedWhenCollectTimesOut(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	payload := &HeartbeatPayload{HiveID: "cached", DashboardURL: "https://cached"}
	// Use the wedged collector; heartbeatFreshStatsTimeout bounds the wait.
	overlayFreshStatsAfterCollectTimeout(context.Background(), payload, nil2Logger(), func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "late"}
	})
	if payload.FreshAgentStats || payload.DashboardURL != "https://cached" {
		t.Fatalf("payload = %+v, want untouched cached payload after overlay timeout", payload)
	}
}

func TestOverlayFreshStatsReplacesOnlyCarriedSections(t *testing.T) {
	health := map[string]any{"ok": true}
	noCadence := []string{"agent-a"}
	payload := &HeartbeatPayload{HiveID: "cached", DashboardURL: "https://cached"}
	overlayFreshStatsAfterCollectTimeout(context.Background(), payload, nil2Logger(), func() *HeartbeatPayload {
		return &HeartbeatPayload{
			Agents:          []AgentSummary{{Name: "fresh-agent"}},
			Health:          health,
			ACMMLevel:       4,
			NoCadenceAgents: noCadence,
		}
	})
	if !payload.FreshAgentStats {
		t.Fatal("overlay with fresh agents must mark stats fresh")
	}
	if len(payload.Agents) != 1 || payload.Agents[0].Name != "fresh-agent" {
		t.Fatalf("agents = %+v, want the fresh agent list", payload.Agents)
	}
	if payload.Health == nil || payload.ACMMLevel != 4 {
		t.Fatal("overlay must adopt the fresh health and ACMM level")
	}
	if len(payload.NoCadenceAgents) != 1 || payload.NoCadenceAgents[0] != "agent-a" {
		t.Fatalf("no-cadence agents = %+v, want the fresh list", payload.NoCadenceAgents)
	}
	if payload.DashboardURL != "https://cached" {
		t.Fatalf("dashboard URL = %q, want cached value kept when fresh omits it", payload.DashboardURL)
	}
}

func TestPostHeartbeatToHubReturnsNilWhenTheHubIsUnreachable(t *testing.T) {
	if got := postHeartbeatToHub(context.Background(), "http://127.0.0.1:0", &HeartbeatPayload{HiveID: "h"}, nil2Logger()); got != nil {
		t.Fatalf("postHeartbeatToHub = %+v, want nil for an unreachable hub", got)
	}
}

func TestPostHeartbeatToHubReturnsNilOnRejection(t *testing.T) {
	server := newRejectingHub(t)
	if got := postHeartbeatToHub(context.Background(), server, &HeartbeatPayload{HiveID: "h"}, nil2Logger()); got != nil {
		t.Fatalf("postHeartbeatToHub = %+v, want nil when the hub rejects the beat", got)
	}
}
