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

func TestBuildClusterHealthPushReportedAwaitingHeartbeat(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()

	s := &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"vllm-d": {ID: "vllm-d", Name: "vLLM-d", PullOnly: true},
		},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	if len(resp.Clusters) != 1 {
		t.Fatalf("clusters = %+v, want one push-reported cluster", resp.Clusters)
	}
	got := resp.Clusters[0]
	if got.Error != "" {
		t.Fatalf("push-reported awaiting heartbeat rendered as error %q", got.Error)
	}
	if got.Status != perClusterHealthStatusAwaitingHeartbeat {
		t.Fatalf("status = %q, want %q", got.Status, perClusterHealthStatusAwaitingHeartbeat)
	}
	if got.HiveCount != 0 || got.Summary.HiveCount != 0 {
		t.Fatalf("hive counts = cluster %d summary %d, want 0", got.HiveCount, got.Summary.HiveCount)
	}
	if got.Note != "push-reported · awaiting spoke heartbeat" {
		t.Fatalf("note = %q", got.Note)
	}
}

func TestBuildClusterHealthPushReportedOldSpokeExplainsMissingHealth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()

	now := time.Now().UTC()
	s := &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"vllm-d": {ID: "vllm-d", Name: "vLLM-d", PullOnly: true},
		},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
		registry: Registry{
			Hives: []RegistryEntry{{
				ID:            "hosted-vllmd",
				ClusterID:     "vllm-d",
				Version:       "5.132.4-2-g80eec9a63",
				GitHash:       "80eec9a",
				LastHeartbeat: now.Format(time.RFC3339),
			}},
		},
	}

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	if len(resp.Clusters) != 1 {
		t.Fatalf("clusters = %+v, want one push-reported cluster", resp.Clusters)
	}
	got := resp.Clusters[0]
	if got.Status != perClusterHealthStatusMissingHealth {
		t.Fatalf("status = %q, want %q", got.Status, perClusterHealthStatusMissingHealth)
	}
	want := "push-reported · heartbeat carries no node health — spoke build predates #10567; upgrade spokes to ≥ v5.133.0 (last seen just now)"
	if got.Note != want {
		t.Fatalf("note = %q, want %q", got.Note, want)
	}
}

func TestBuildClusterHealthPushReportedCurrentSpokeExplainsMissingHealth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()

	now := time.Now().UTC()
	s := &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"vllm-d": {ID: "vllm-d", Name: "vLLM-d", PullOnly: true},
		},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
		registry: Registry{
			Hives: []RegistryEntry{{
				ID:            "hosted-vllmd",
				ClusterID:     "vllm-d",
				Version:       clusterHealthUpgradeVersion,
				GitHash:       clusterHealthUpgradeFixSHA,
				LastHeartbeat: now.Format(time.RFC3339),
			}},
		},
	}

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	if len(resp.Clusters) != 1 {
		t.Fatalf("clusters = %+v, want one push-reported cluster", resp.Clusters)
	}
	got := resp.Clusters[0]
	want := "push-reported · heartbeat carries no node health — spoke is not sending cluster_health; set hub.cluster_id or HIVE_CLUSTER_ID, then check node-metrics RBAC and metrics-server (last seen just now)"
	if got.Note != want {
		t.Fatalf("note = %q, want %q", got.Note, want)
	}
}

func TestHeartbeatHealthKeepsRecentNodeDataOverEmptyReport(t *testing.T) {
	now := time.Now()
	withNodes := &HeartbeatHealthEntry{Report: sampleHeartbeatReport(), ReceivedAt: now}
	empty := &HeartbeatClusterHealthReport{NodeHealthError: "nodes API failed: forbidden"}
	if shouldReplaceHeartbeatHealth(withNodes, empty, now.Add(time.Minute)) {
		t.Fatal("empty report replaced recent node data; one broken spoke must not blank a push-reported cluster")
	}
	if !shouldReplaceHeartbeatHealth(withNodes, empty, now.Add(heartbeatHealthStaleness+time.Minute)) {
		t.Fatal("stale node data should yield to a current empty/error report")
	}
	if !shouldReplaceHeartbeatHealth(withNodes, sampleHeartbeatReport(), now.Add(time.Minute)) {
		t.Fatal("fresh node data should replace previous data")
	}
	degradedWithNodes := sampleHeartbeatReport()
	degradedWithNodes.NodeHealthError = "metrics API failed: forbidden"
	if shouldReplaceHeartbeatHealth(withNodes, degradedWithNodes, now.Add(time.Minute)) {
		t.Fatal("partial node data replaced recent complete node data")
	}
}

func TestConvertHeartbeatToPerClusterHealthMarksStale(t *testing.T) {
	entry := &HeartbeatHealthEntry{
		Report:     sampleHeartbeatReport(),
		ReceivedAt: time.Now().Add(-(heartbeatHealthStaleness + time.Minute)),
	}

	got := convertHeartbeatToPerClusterHealth("vllm-d", "vLLM-d", entry, 1)
	if !got.DataStale {
		t.Fatal("stale heartbeat report should be marked stale")
	}
	if got.Status != perClusterHealthStatusStaleHeartbeat {
		t.Fatalf("status = %q, want %q", got.Status, perClusterHealthStatusStaleHeartbeat)
	}
	if got.Note == "" {
		t.Fatal("stale heartbeat note should explain the warning")
	}
}

func TestConvertHeartbeatToPerClusterHealthShowsNodeHealthError(t *testing.T) {
	report := sampleHeartbeatReport()
	report.NodeHealthError = "metrics API failed: k8s API /apis/metrics.k8s.io/v1beta1/nodes: HTTP 403: forbidden"
	entry := &HeartbeatHealthEntry{Report: report, ReceivedAt: time.Now()}

	got := convertHeartbeatToPerClusterHealth("vllm-d", "vLLM-d", entry, 36)
	if got.Status != perClusterHealthStatusMissingHealth {
		t.Fatalf("status = %q, want %q", got.Status, perClusterHealthStatusMissingHealth)
	}
	if got.Error != report.NodeHealthError {
		t.Fatalf("error = %q, want %q", got.Error, report.NodeHealthError)
	}
	want := "push-reported · node health partial — " + report.NodeHealthError
	if got.Note != want {
		t.Fatalf("note = %q, want %q", got.Note, want)
	}
	if got.Summary.TotalNodes != 1 || got.Summary.TotalCPUCores != 4 || got.Summary.HiveCount != 36 {
		t.Fatalf("summary = %+v, want heartbeat capacity preserved", got.Summary)
	}
}
