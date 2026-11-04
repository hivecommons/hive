package spoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The real entry point must finish before the heartbeat gives up, even when
// the nodes API never responds. A per-request 10s timeout misses that budget.
func TestClusterHealthFitsHeartbeatBudget(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	withFakeK8sAPI(t, srv)

	started := time.Now()
	report := collectClusterHealthUncached(nil)
	elapsed := time.Since(started)
	if elapsed >= heartbeatTimeout {
		t.Fatalf("cluster health took %s, exhausted heartbeat budget %s", elapsed, heartbeatTimeout)
	}
	if report == nil || !strings.Contains(report.NodeHealthError, "context deadline exceeded") {
		t.Fatalf("report = %+v, want nodes API deadline error", report)
	}
}

// Cancellation during optional disk collection must reach the active request
// and every subsequent request, while preserving the capacity/usage collected
// earlier. Fresh Background contexts per API call would hang this collector.
func TestClusterHealthSharesContextAndPreservesPartialReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var diskRequests, podRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"a"},"status":{"allocatable":{"cpu":"4","memory":"8Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
				{"metadata":{"name":"b"},"status":{"allocatable":{"cpu":"4","memory":"8Gi"}}}
			]}`))
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"a"},"usage":{"cpu":"1000m","memory":"1Gi"}}]}`))
		case "/api/v1/pods":
			podRequests.Add(1)
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			diskRequests.Add(1)
			cancel()
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
				t.Error("disk API request did not receive collection cancellation")
			}
		}
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	report := collectClusterHealthWithContext(ctx, nil)
	if diskRequests.Load() != 1 || podRequests.Load() != 0 {
		t.Fatalf("requests after cancellation: disk=%d pods=%d", diskRequests.Load(), podRequests.Load())
	}
	if report.Summary.TotalNodes != 2 || report.Summary.TotalCPUCores != 8 || report.Summary.TotalCPUPct != 12 {
		t.Fatalf("lost partial node data: %+v", report.Summary)
	}
	if report.Summary.HiveCapacityRemaining != nil || !strings.Contains(report.NodeHealthError, "context canceled") {
		t.Fatalf("report must mark missing pod data: %+v", report)
	}
	for _, node := range report.Nodes {
		if node.DiskPercent != nil {
			t.Fatalf("node %s has fabricated disk usage", node.Name)
		}
	}
}
