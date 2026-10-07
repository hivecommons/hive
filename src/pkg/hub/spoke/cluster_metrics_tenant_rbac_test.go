package spoke

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClusterHealthWithSafeTenantRBAC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			w.Write([]byte(`{"items":[{"metadata":{"name":"node-a"},"status":{"allocatable":{"cpu":"4","memory":"8Gi","ephemeral-storage":"100Gi"},"capacity":{"cpu":"4","memory":"8Gi","ephemeral-storage":"100Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`))
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			w.Write([]byte(`{"items":[{"metadata":{"name":"node-a"},"usage":{"cpu":"1","memory":"1Gi"}}]}`))
		default:
			// Both the pod list and kubelet proxy are denied by the safe role.
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	report := collectClusterHealthUncached(slog.Default())
	if report == nil || len(report.Nodes) != 1 {
		t.Fatalf("expected node capacity despite denied telemetry: %+v", report)
	}
	if report.Summary.TotalCPUCores != 4 || report.Summary.TotalMemGB != 8 || report.Summary.TotalDiskGB == nil || *report.Summary.TotalDiskGB != 100 {
		t.Fatalf("lost node capacity: %+v", report.Summary)
	}
	if report.Nodes[0].CPUUsedMillis != 1000 {
		t.Fatalf("lost allowed node metrics: %+v", report.Nodes[0])
	}
	if report.Nodes[0].DiskUsedMB != nil || report.Summary.TotalDiskPct != nil || report.Summary.HiveCapacityRemaining != nil {
		t.Fatalf("unavailable telemetry reported as known: %+v", report)
	}
	if !strings.Contains(report.NodeHealthError, "pods API failed") || !strings.Contains(report.NodeHealthError, "403") {
		t.Fatalf("missing partial-health reason: %q", report.NodeHealthError)
	}
}
