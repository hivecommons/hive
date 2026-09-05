package spoke

import (
	"log/slog"
	"testing"
	"time"
)

// ============================================================
// cluster_metrics.go + hive_capacity.go
// ============================================================

func TestMinInt(t *testing.T) {
	if minInt(1, 2) != 1 {
		t.Error("minInt(1,2) != 1")
	}
	if minInt(5, 3) != 3 {
		t.Error("minInt(5,3) != 3")
	}
	if minInt(4, 4) != 4 {
		t.Error("minInt(4,4) != 4")
	}
}

func TestK8sAPIGetNoToken(t *testing.T) {
	// In the test environment the service-account token file does not exist,
	// so k8sAPIGet fails at the very first step (reading the SA token).
	_, err := k8sAPIGet("/api/v1/nodes")
	if err == nil {
		t.Error("expected error when SA token missing")
	}
}

func TestCollectClusterHealthNoCluster(t *testing.T) {
	// Reset the package cache so we exercise the uncached path.
	cachedClusterHealthMu.Lock()
	cachedClusterHealth = nil
	cachedClusterHealthTime = time.Time{}
	cachedClusterHealthMu.Unlock()

	// No SA token -> collectClusterHealthUncached returns nil (metrics API fails).
	if got := CollectClusterHealth(slog.Default()); got != nil {
		t.Errorf("expected nil report without k8s API access, got %+v", got)
	}

	// Second call hits the cache branch (cachedClusterHealth is nil though, so
	// TTL check short-circuits to false; still exercises the lock path).
	_ = CollectClusterHealth(slog.Default())
}

func TestCollectClusterHealthCached(t *testing.T) {
	// Seed a fresh cache so CollectClusterHealth returns it without recomputing.
	seed := &HeartbeatClusterHealthReport{
		Summary: HeartbeatClusterSummary{TotalNodes: 3},
	}
	cachedClusterHealthMu.Lock()
	cachedClusterHealth = seed
	cachedClusterHealthTime = time.Now()
	cachedClusterHealthMu.Unlock()

	got := CollectClusterHealth(slog.Default())
	if got == nil || got.Summary.TotalNodes != 3 {
		t.Errorf("expected cached report, got %+v", got)
	}

	// Cleanup for other tests.
	cachedClusterHealthMu.Lock()
	cachedClusterHealth = nil
	cachedClusterHealthTime = time.Time{}
	cachedClusterHealthMu.Unlock()
}

func TestHiveSlotsForNode(t *testing.T) {
	// Not ready -> 0.
	if got := hiveSlotsForNode(100000, 100<<30, 0, 0, false, false); got != 0 {
		t.Errorf("not-ready node should have 0 slots, got %d", got)
	}
	// Cordoned (unschedulable) -> 0.
	if got := hiveSlotsForNode(100000, 100<<30, 0, 0, true, true); got != 0 {
		t.Errorf("cordoned node should have 0 slots, got %d", got)
	}
	// Fully requested (no free CPU) -> 0.
	if got := hiveSlotsForNode(1000, 100<<30, 1000, 0, true, false); got != 0 {
		t.Errorf("no free CPU should be 0 slots, got %d", got)
	}
	// Memory-bound: plenty of CPU but tiny free memory.
	if true {
		// Free CPU large, free mem exactly one hive footprint -> min() picks mem.
		got := hiveSlotsForNode(
			1000*100,        // lots of CPU room
			(2*giToBytes)*2, // room for exactly 2 by memory
			0, 0, true, false,
		)
		if got != 2 {
			t.Errorf("memory-bound slots = %d, want 2", got)
		}
	}
}
