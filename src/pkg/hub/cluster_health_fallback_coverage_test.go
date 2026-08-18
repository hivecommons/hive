package hub

import (
	"log/slog"
	"testing"
	"time"
)

// ============================================================
// saas.go — buildClusterHealth heartbeat-fallback + heartbeat-only cluster
//
// With the fail-fast fake kubectl (exit 1), each per-cluster kubectl query
// errors, so buildClusterHealth falls back to heartbeat-reported health. A
// heartbeat-only cluster not present in s.clusters is also included.
// ============================================================

func sampleHeartbeatReport() *HeartbeatClusterHealthReport {
	return &HeartbeatClusterHealthReport{
		Nodes: []HeartbeatNodeMetric{{
			Name: "node-a", CPUCores: 4, CPUPercent: 50, MemTotalMB: 8192, MemPercent: 40,
			Ready: true, Conditions: []string{"Ready"},
		}},
		Summary: HeartbeatClusterSummary{
			TotalNodes: 1, ReadyNodes: 1, TotalCPUCores: 4, TotalCPUPct: 50, TotalMemGB: 8, TotalMemPct: 40,
		},
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func TestBuildClusterHealthHeartbeatFallback(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	// NOTE: no scripted kubectl here — the fail-fast fake kubectl (exit 1) is on
	// PATH, so buildSingleClusterHealth errors and the fallback path runs.

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()

	s := &HubServer{
		logger:          slog.Default(),
		clusters:        map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true, Name: "OKE"}},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}
	// Heartbeat health for the in-clusters cluster (fallback on kubectl error)...
	s.heartbeatHealth["hive-oke"] = &HeartbeatHealthEntry{Report: sampleHeartbeatReport(), ReceivedAt: time.Now()}
	// ...and for a heartbeat-only cluster NOT in s.clusters (vllm-d-style).
	s.heartbeatHealth["vllm-d"] = &HeartbeatHealthEntry{Report: sampleHeartbeatReport(), ReceivedAt: time.Now()}

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	// Expect both clusters represented (kubectl-fallback + heartbeat-only).
	if len(resp.Clusters) < 2 {
		t.Errorf("expected >=2 clusters (fallback + heartbeat-only), got %+v", resp.Clusters)
	}
	// Aggregated summary should reflect the heartbeat-reported nodes.
	if resp.Summary.TotalNodes == 0 {
		t.Errorf("expected aggregated nodes, got %+v", resp.Summary)
	}

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()
}
