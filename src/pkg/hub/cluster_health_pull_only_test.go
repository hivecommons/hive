package hub

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func resetClusterHealthCacheForTest() {
	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()
}

func findClusterHealth(t *testing.T, resp *ClusterHealthResponse, id string) PerClusterHealth {
	t.Helper()
	for _, c := range resp.Clusters {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("cluster %q missing from health response: %+v", id, resp.Clusters)
	return PerClusterHealth{}
}

// A configured pull-only cluster with no heartbeat health yet is an expected,
// informational state — not the red "cluster is pull-only: not reachable"
// error banner (#10559).
func TestBuildClusterHealth_PullOnlyAwaitingHeartbeatIsNotAnError(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetClusterHealthCacheForTest()

	s := &HubServer{
		logger:          slog.Default(),
		clusters:        map[string]ClusterConfig{"vllm-d": {ID: "vllm-d", Name: "vLLM-d (GPU)", PullOnly: true}},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	c := findClusterHealth(t, resp, "vllm-d")
	if c.Error != "" {
		t.Errorf("Error = %q, want empty for a pull-only cluster awaiting its first heartbeat", c.Error)
	}
	if !c.PullOnly || !c.AwaitingHeartbeat {
		t.Errorf("PullOnly=%v AwaitingHeartbeat=%v, want both true", c.PullOnly, c.AwaitingHeartbeat)
	}
	if c.LastSeen != "never" {
		t.Errorf("LastSeen = %q, want %q with no spoke registered on the cluster", c.LastSeen, "never")
	}
	if c.Name != "vLLM-d (GPU)" {
		t.Errorf("Name = %q, want the configured cluster name", c.Name)
	}
}

// Once a spoke on the pull-only cluster reports health over the heartbeat,
// the card shows its node stats and is no longer "awaiting".
func TestBuildClusterHealth_PullOnlyUsesHeartbeatHealth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetClusterHealthCacheForTest()

	s := &HubServer{
		logger:          slog.Default(),
		clusters:        map[string]ClusterConfig{"vllm-d": {ID: "vllm-d", Name: "vLLM-d (GPU)", PullOnly: true}},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}
	s.heartbeatHealth["vllm-d"] = &HeartbeatHealthEntry{Report: sampleHeartbeatReport(), ReceivedAt: time.Now()}

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	c := findClusterHealth(t, resp, "vllm-d")
	if c.Error != "" || c.AwaitingHeartbeat {
		t.Errorf("Error=%q AwaitingHeartbeat=%v, want heartbeat-backed health", c.Error, c.AwaitingHeartbeat)
	}
	if !c.PullOnly || c.DataSource != "heartbeat" {
		t.Errorf("PullOnly=%v DataSource=%q, want true/heartbeat", c.PullOnly, c.DataSource)
	}
	if c.Summary.TotalNodes != 1 || len(c.Nodes) != 1 {
		t.Errorf("nodes = %d (summary %d), want the heartbeat-reported node", len(c.Nodes), c.Summary.TotalNodes)
	}
}

func TestLastSpokeHeartbeatForCluster(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := &HubServer{registry: Registry{Hives: []RegistryEntry{
		{ID: "a", ClusterID: "vllm-d", LastHeartbeat: now.Add(-90 * time.Minute).Format(time.RFC3339)},
		{ID: "b", ClusterID: "vllm-d", LastHeartbeat: now.Add(-7 * time.Minute).Format(time.RFC3339)},
		{ID: "c", ClusterID: "hive-oke", LastHeartbeat: now.Format(time.RFC3339)},
		{ID: "d", ClusterID: "vllm-d", LastHeartbeat: "garbage"},
	}}}
	if got := s.lastSpokeHeartbeatForCluster("vllm-d", now); got != "7m ago" {
		t.Errorf("vllm-d last seen = %q, want the most recent spoke on that cluster (7m ago)", got)
	}
	if got := s.lastSpokeHeartbeatForCluster("other", now); got != "never" {
		t.Errorf("other last seen = %q, want never", got)
	}
	if got := s.lastSpokeHeartbeatForCluster("hive-oke", now); !strings.Contains(got, "just now") {
		t.Errorf("hive-oke last seen = %q, want just now", got)
	}
}
