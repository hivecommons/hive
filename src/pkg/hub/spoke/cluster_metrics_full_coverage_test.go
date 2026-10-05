package spoke

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// cluster_metrics.go — full collectClusterHealthUncached path
//
// Serves the three K8s API responses (metrics, nodes, pods) from a fake
// in-cluster API server so the whole parse/aggregate pipeline runs.
// ============================================================

func TestCollectClusterHealthUncachedFull(t *testing.T) {
	nodesJSON := `{"items":[{
		"metadata":{"name":"node-a","labels":{"nvidia.com/gpu.product":"A100"}},
		"spec":{"unschedulable":false},
		"status":{
			"allocatable":{"cpu":"4","memory":"8Gi","nvidia.com/gpu":"2","pods":"110"},
			"capacity":{"cpu":"4","memory":"8Gi","nvidia.com/gpu":"2","pods":"110"},
			"conditions":[{"type":"Ready","status":"True"},{"type":"DiskPressure","status":"False"}]
		}
	},{
		"metadata":{"name":"node-b","labels":{}},
		"spec":{"unschedulable":true},
		"status":{
			"allocatable":{"cpu":"2","memory":"4Gi","pods":"110"},
			"capacity":{"cpu":"2","memory":"4Gi","pods":"110"},
			"conditions":[{"type":"Ready","status":"False"},{"type":"DiskPressure","status":"True"}]
		}
	}]}`
	metricsJSON := `{"items":[
		{"metadata":{"name":"node-a"},"usage":{"cpu":"2000m","memory":"4Gi"}},
		{"metadata":{"name":"node-b"},"usage":{"cpu":"500m","memory":"1Gi"}}
	]}`
	podsJSON := `{"items":[
		{"metadata":{"namespace":"hive-hosted-x"},"spec":{"nodeName":"node-a","containers":[{"resources":{"requests":{"cpu":"500m","memory":"512Mi"}}}]}},
		{"metadata":{"namespace":"default"},"spec":{"nodeName":"node-a","containers":[{"resources":{"requests":{"cpu":"100m","memory":"128Mi"}}}]}}
	]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "metrics.k8s.io"):
			w.Write([]byte(metricsJSON))
		case strings.Contains(r.URL.RawQuery, "status.phase"):
			w.Write([]byte(podsJSON))
		case strings.Contains(r.URL.Path, "/api/v1/nodes"):
			w.Write([]byte(nodesJSON))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	report := collectClusterHealthUncached(slog.Default())
	if report == nil {
		t.Fatal("expected a report, got nil")
	}
	if report.Summary.TotalNodes == 0 {
		t.Error("expected nodes in summary")
	}
	if report.GPUSummary == nil || report.GPUSummary.Total != 2 {
		t.Errorf("expected GPU summary total 2, got %+v", report.GPUSummary)
	}
	// node-a is Ready+schedulable, so hive-capacity remaining should be computed.
	if report.Summary.HiveCapacityRemaining == nil {
		t.Error("expected HiveCapacityRemaining to be computed")
	}
}

func TestCollectClusterHealthUncachedMetricsFail(t *testing.T) {
	// Metrics API returns 500 -> capacity-only report with a precise error.
	nodesJSON := `{"items":[{
		"metadata":{"name":"node-a","labels":{}},
		"status":{
			"allocatable":{"cpu":"4","memory":"8Gi","ephemeral-storage":"100Gi","pods":"110"},
			"capacity":{"cpu":"4","memory":"8Gi","ephemeral-storage":"100Gi","pods":"110"},
			"conditions":[{"type":"Ready","status":"True"}]
		}
	}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/nodes") {
			w.Write([]byte(nodesJSON))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	report := collectClusterHealthUncached(slog.Default())
	if report == nil {
		t.Fatal("expected capacity-only report when metrics API fails")
	}
	if report.Summary.TotalNodes != 1 || report.Summary.TotalCPUCores != 4 || report.Summary.TotalMemGB != 8 {
		t.Fatalf("summary = %+v, want node/cpu/memory capacity", report.Summary)
	}
	if report.Summary.TotalDiskGB == nil || *report.Summary.TotalDiskGB != 100 || report.Summary.TotalDiskPct != nil {
		t.Fatalf("disk summary = %+v/%+v, want 100GiB capacity with unknown percent", report.Summary.TotalDiskGB, report.Summary.TotalDiskPct)
	}
	if report.NodeHealthError == "" || !strings.Contains(report.NodeHealthError, "metrics API failed") {
		t.Fatalf("node health error = %q, want metrics API failure", report.NodeHealthError)
	}
	if len(report.Nodes) != 1 || report.Nodes[0].DiskTotalMB == nil {
		t.Fatalf("nodes = %+v, want disk capacity from node allocatable/capacity", report.Nodes)
	}
}

func TestCollectClusterHealthUncachedNodesFail(t *testing.T) {
	// Nodes API fails -> report contains the precise reason for the hub.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	report := collectClusterHealthUncached(slog.Default())
	if report == nil {
		t.Fatal("expected error report when nodes API fails")
	}
	if report.Summary.TotalNodes != 0 {
		t.Fatalf("summary = %+v, want no nodes", report.Summary)
	}
	if report.NodeHealthError == "" || !strings.Contains(report.NodeHealthError, "nodes API failed") {
		t.Fatalf("node health error = %q, want nodes API failure", report.NodeHealthError)
	}
}

func TestCollectClusterHealthUncachedBadNodesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "metrics.k8s.io") {
			w.Write([]byte(`{"items":[]}`))
			return
		}
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	report := collectClusterHealthUncached(slog.Default())
	if report == nil {
		t.Fatal("expected error report on bad nodes JSON")
	}
	if report.NodeHealthError == "" || !strings.Contains(report.NodeHealthError, "failed to parse nodes JSON") {
		t.Fatalf("node health error = %q, want bad nodes JSON", report.NodeHealthError)
	}
}

func TestCollectClusterHealthResetCacheHelper(t *testing.T) {
	// Sanity: ensure our tests leave the cache clean for later runs.
	cachedClusterHealthMu.Lock()
	cachedClusterHealth = nil
	cachedClusterHealthTime = time.Time{}
	cachedClusterHealthMu.Unlock()
}
