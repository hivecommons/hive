package spoke

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const budgetTestNodes = `{"items":[{"metadata":{"name":"node-a"},"status":{"allocatable":{"cpu":"4","memory":"8Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`

func TestClusterHealthCollectionBudget(t *testing.T) {
	// A stalled API must leave time for the rest of heartbeat collection, even
	// on a cold cache. Previously this consumed the entire heartbeat budget.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	start := time.Now()
	report := collectClusterHealthUncached(nil)
	if elapsed := time.Since(start); elapsed >= heartbeatTimeout {
		t.Fatalf("cluster health took %s, exhausted heartbeat budget %s", elapsed, heartbeatTimeout)
	}
	if report == nil || !strings.Contains(report.NodeHealthError, "context deadline exceeded") {
		t.Fatalf("expected partial report with deadline error, got %+v", report)
	}
}

func TestClusterHealthSharesContextAcrossRequests(t *testing.T) {
	for _, stallPath := range []string{
		"/api/v1/nodes",
		"/apis/metrics.k8s.io/v1beta1/nodes",
		nodeStatsSummaryPath("node-a"),
		"/api/v1/pods",
	} {
		t.Run(stallPath, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == stallPath {
					// Expire the shared collection context while this request is
					// in flight. Later requests must use the same spent budget.
					cancel()
					<-r.Context().Done()
					return
				}
				if ctx.Err() != nil {
					t.Errorf("API request after collection cancellation: %s", r.URL.Path)
				}
				if r.URL.Path == "/api/v1/nodes" {
					_, _ = w.Write([]byte(budgetTestNodes))
				} else {
					_, _ = w.Write([]byte(`{"items":[]}`))
				}
			}))
			defer srv.Close()
			withFakeK8sAPI(t, srv)

			report := collectClusterHealthWithContext(ctx, nil)
			if report == nil || !strings.Contains(report.NodeHealthError, "context canceled") {
				t.Fatalf("expected partial report with cancellation error, got %+v", report)
			}
			if stallPath != "/api/v1/nodes" {
				if report.Summary.TotalNodes != 1 || report.Summary.TotalCPUCores != 4 || report.Summary.TotalMemGB != 8 {
					t.Fatalf("lost node capacity after cancellation: %+v", report.Summary)
				}
				if report.Summary.HiveCapacityRemaining != nil {
					t.Fatal("capacity estimate should be unavailable without pod requests")
				}
				if report.Nodes[0].DiskPercent != nil {
					t.Fatal("missing disk usage must stay unavailable")
				}
			}
		})
	}
}

func TestK8sAPIGetSpentCollectionBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withMissingK8sToken(t)
	_, err := k8sAPIGetWithContext(ctx, "/api/v1/nodes")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("spent budget should stop before credential access, got %v", err)
	}
}
