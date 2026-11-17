package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// launch_cost_read reads the Cost card's endpoint as the caller, asks for
// only the rows up to its page, passes ref through, and refuses a bad offset
// before any request reaches the router.
func TestDashboardAdminMCPLaunchCostReadsTheCostCardEndpoint(t *testing.T) {
	var seen []string
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/launch-cost", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true,"total_usd":3,"by_ref":[{"ref":"o/r#1","kind":"pr","launches":2},{"ref":"o/r#2","kind":"issue","launches":1}],"by_launch":[{"agent":"reviewer"}],"by_ref_total":5,"by_launch_total":1,"truncated":true,"unattributed":{"usd":1},"backend_unsupported":{"usd":null}}`))
	})
	p := dashboardAdminMCPProvider{server: s, role: "owner", ownerVerified: true}

	got, err := p.Read(context.Background(), adminmcp.ToolLaunchCostRead, map[string]any{"limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/api/launch-cost?max_points=2" {
		t.Fatalf("requested %v", seen)
	}
	out := got.(map[string]any)
	if refs := out["by_ref"].([]any); len(refs) != 2 || out["by_ref_total"] != 5 || out["next_offset"] != 2 {
		t.Fatalf("answer = %#v", out)
	}
	if _, ok := out["by_launch"]; ok {
		t.Fatal("by_launch returned without ref")
	}

	seen = nil
	if _, err := p.Read(context.Background(), adminmcp.ToolLaunchCostRead, map[string]any{"ref": "o/r#1"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/api/launch-cost?max_points=20&ref=o%2Fr%231" {
		t.Fatalf("requested %v", seen)
	}

	seen = nil
	if _, err := p.Read(context.Background(), adminmcp.ToolLaunchCostRead, map[string]any{"offset": float64(-2)}); err == nil || !strings.Contains(err.Error(), "offset must be") {
		t.Fatalf("bad offset = %v, want a refusal", err)
	}
	if len(seen) != 0 {
		t.Fatalf("a refused read reached the router: %v", seen)
	}
}

// The real handler accepts the tool's query: max_points and ref are the
// endpoint's own parameters, so the tool's first page is a 200, not a 400.
func TestLaunchCostEndpointAcceptsTheToolsQuery(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	for _, args := range []map[string]any{nil, {"limit": float64(50), "offset": float64(100)}, {"ref": "o/r#1"}} {
		path, err := adminmcp.LaunchCostReadPath(args)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.handleLaunchCost(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"by_ref"`) {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
