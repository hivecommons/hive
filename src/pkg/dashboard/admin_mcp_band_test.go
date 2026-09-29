package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

func TestAdminMCPIssuesByBandReadPathOmitsBandAndLimit(t *testing.T) {
	path, ok := adminMCPReadPath(adminmcp.ToolIssuesByBand, map[string]any{"repo": "owner/name", "band": "done", "stale": true, "limit": float64(5)})
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.HasPrefix(path, "/api/overview/issues.json?") {
		t.Fatalf("path = %q", path)
	}
	if !strings.Contains(path, "repo=owner%2Fname") || !strings.Contains(path, "stale=true") {
		t.Fatalf("path = %q, want repo and stale forwarded", path)
	}
	if strings.Contains(path, "band=") || strings.Contains(path, "limit=") {
		t.Fatalf("path = %q, must not forward band or limit — the tool filters/caps rows itself so bands[] keeps its real counts", path)
	}
}

func TestAdminMCPPrsByBandReadPathWithoutFilters(t *testing.T) {
	path, ok := adminMCPReadPath(adminmcp.ToolPrsByBand, map[string]any{})
	if !ok || path != "/api/overview/prs.json" {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
}

func TestDashboardAdminMCPIssuesByBandFiltersRowsButKeepsBands(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/overview/issues.json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("band") != "" {
			t.Fatalf("handler saw a band filter %q; the tool must fetch every band", r.URL.Query().Get("band"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bands": []any{
				map[string]any{"key": "ready", "label": "Unclaimed", "rule": "no other band matched", "count": 1},
				map[string]any{"key": "done", "label": "Confirm & close", "rule": "verify and close", "count": 2},
			},
			"rows": []any{
				map[string]any{"number": 1, "band": "ready"},
				map[string]any{"number": 2, "band": "done"},
				map[string]any{"number": 3, "band": "done"},
			},
		})
	})
	provider := dashboardAdminMCPProvider{server: s}
	result, err := provider.Read(context.Background(), adminmcp.ToolIssuesByBand, map[string]any{"band": "done"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", result)
	}
	bands, ok := body["bands"].([]any)
	if !ok || len(bands) != 2 {
		t.Fatalf("bands = %#v, want both bands even when filtered", body["bands"])
	}
	rows, ok := body["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %#v, want only the 2 done-band rows", body["rows"])
	}
}

func TestDashboardAdminMCPIssuesByBandRefusesUnknownBand(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/overview/issues.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"bands": []any{}, "rows": []any{}})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issues_by_band","arguments":{"band":"nope"}}}`))
	adminmcp.NewHandler(dashboardAdminMCPProvider{server: s}).ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"type":"refusal"`) || !strings.Contains(rec.Body.String(), "ready") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
