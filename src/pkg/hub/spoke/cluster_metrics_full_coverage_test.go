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
	// Metrics API returns 500 -> nil report (metrics-server unavailable path).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if report := collectClusterHealthUncached(slog.Default()); report != nil {
		t.Errorf("expected nil report when metrics API fails, got %+v", report)
	}
}

func TestCollectClusterHealthUncachedNodesFail(t *testing.T) {
	// Metrics OK but nodes API fails -> nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "metrics.k8s.io") {
			w.Write([]byte(`{"items":[]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if report := collectClusterHealthUncached(slog.Default()); report != nil {
		t.Errorf("expected nil report when nodes API fails, got %+v", report)
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

	if report := collectClusterHealthUncached(slog.Default()); report != nil {
		t.Errorf("expected nil report on bad nodes JSON, got %+v", report)
	}
}

func TestCollectClusterHealthResetCacheHelper(t *testing.T) {
	// Sanity: ensure our tests leave the cache clean for later runs.
	cachedClusterHealthMu.Lock()
	cachedClusterHealth = nil
	cachedClusterHealthTime = time.Time{}
	cachedClusterHealthMu.Unlock()
}
