package hub

import (
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func replenisherTestServer(t *testing.T, clusters map[string]ClusterConfig) *HubServer {
	t.Helper()
	dir := t.TempDir()
	orig := saasHivesDir
	saasHivesDir = dir
	t.Cleanup(func() { saasHivesDir = orig })
	return &HubServer{
		clusters:          clusters,
		poolReplenishHold: make(map[string]time.Time),
		logger:            slog.Default(),
	}
}

func stubProvision(t *testing.T, fn func(*SaaSHive) error) {
	t.Helper()
	orig := provisionHiveFn
	provisionHiveFn = func(h *SaaSHive, req *CreateHiveRequest, cluster *ClusterConfig, keys map[int64]fleetAppKey, logger *slog.Logger) error {
		return fn(h)
	}
	t.Cleanup(func() { provisionWG.Wait(); provisionHiveFn = orig })
}

func seedAvailable(t *testing.T, id, clusterID string) {
	t.Helper()
	h := &SaaSHive{
		ID: id, Owner: primaryHubAdmin(), Org: placeholderOrgPrefix + id,
		Status: statusAvailable, ClusterID: clusterID,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestReplenishPoolsSeedsToTarget verifies the watermark math: below
// pool_min, the loop provisions up to pool_target through the queue and the
// new slots land statusAvailable with the placeholder shape.
func TestReplenishPoolsSeedsToTarget(t *testing.T) {
	s := replenisherTestServer(t, map[string]ClusterConfig{
		"test-c1": {ID: "test-c1", InCluster: true, Domain: "example.test", PoolMin: 2, PoolTarget: 4},
	})
	stubProvision(t, func(h *SaaSHive) error { return nil })

	seedAvailable(t, "hosted-available-testc1-existing", "test-c1")

	s.replenishPools()
	provisionWG.Wait()

	if n := countCleanAvailable("test-c1"); n != 4 {
		t.Errorf("available after replenish = %d, want pool_target 4", n)
	}
	for _, h := range listSaaSHives() {
		if h.ID == "hosted-available-testc1-existing" {
			continue
		}
		if !strings.HasPrefix(h.Org, placeholderOrgPrefix) || h.Status != statusAvailable ||
			!isHubAdmin(h.Owner) || h.ClusterID != "test-c1" {
			t.Errorf("seeded slot has wrong shape: %+v", h)
		}
	}
}

// TestReplenishPoolsRespectsWatermarkAndDisabled verifies that a pool at or
// above pool_min is untouched and pool_target==0 disables the loop entirely.
func TestReplenishPoolsRespectsWatermarkAndDisabled(t *testing.T) {
	s := replenisherTestServer(t, map[string]ClusterConfig{
		"test-c1": {ID: "test-c1", InCluster: true, Domain: "example.test", PoolMin: 1, PoolTarget: 3},
		"test-c2": {ID: "test-c2", InCluster: true, Domain: "example.test"}, // disabled
	})
	called := new(atomic.Int64)
	stubProvision(t, func(h *SaaSHive) error { called.Add(1); return nil })

	seedAvailable(t, "hosted-available-testc1-a", "test-c1")
	seedAvailable(t, "hosted-available-testc2-a", "test-c2")

	s.replenishPools()
	provisionWG.Wait()
	if n := called.Load(); n != 0 {
		t.Errorf("provision called %d times, want 0 (at watermark / disabled)", n)
	}
}

// TestReplenishPoolsRespectsMaxHives verifies the capacity gate stops seeding.
func TestReplenishPoolsRespectsMaxHives(t *testing.T) {
	s := replenisherTestServer(t, map[string]ClusterConfig{
		"test-c1": {ID: "test-c1", InCluster: true, Domain: "example.test", PoolMin: 5, PoolTarget: 5, MaxHives: 2},
	})
	stubProvision(t, func(h *SaaSHive) error { return nil })

	seedAvailable(t, "hosted-available-testc1-a", "test-c1")

	s.replenishPools()
	provisionWG.Wait()
	if n := clusterHiveCount("test-c1"); n > 2 {
		t.Errorf("cluster has %d hives, max_hives 2 violated", n)
	}
}

// TestReplenishPoolsBacksOffOnFailure verifies a failed seed marks the record
// error, suspends the cluster, and a second sweep inside the hold provisions
// nothing.
func TestReplenishPoolsBacksOffOnFailure(t *testing.T) {
	s := replenisherTestServer(t, map[string]ClusterConfig{
		"test-c1": {ID: "test-c1", InCluster: true, Domain: "example.test", PoolMin: 2, PoolTarget: 2},
	})
	calls := new(atomic.Int64)
	stubProvision(t, func(h *SaaSHive) error { calls.Add(1); return errors.New("boom") })

	s.replenishPools()
	provisionWG.Wait()
	firstCalls := calls.Load()
	if firstCalls == 0 {
		t.Fatal("expected at least one provision attempt")
	}

	errored := 0
	for _, h := range listSaaSHives() {
		if h.Status == "error" {
			errored++
		}
	}
	if errored == 0 {
		t.Error("failed seed did not mark record error")
	}

	s.replenishPools()
	provisionWG.Wait()
	if n := calls.Load(); n != firstCalls {
		t.Errorf("sweep during backoff attempted %d more provisions", n-firstCalls)
	}
}
