package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// spend_read without arguments reads GET /api/cost as before; with hours it
// reads GET /api/cost/agents, capping the agents rows to limit; a bad window
// is refused with its reason before any request reaches the router (#18968).
func TestDashboardAdminMCPSpendReadWindow(t *testing.T) {
	var seen []string
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/cost", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"estimated":{"total_usd":9}}`))
	})
	s.mux.HandleFunc("GET /api/cost/agents", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"coverage":"full","total_usd":6,"agents":[{"name":"reviewer","usd":4,"total_tokens":4000},{"name":"scanner","usd":2,"total_tokens":9000}]}`))
	})
	p := dashboardAdminMCPProvider{server: s, role: "owner", ownerVerified: true}

	if _, err := p.Read(context.Background(), adminmcp.ToolSpendRead, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/api/cost" {
		t.Fatalf("no-argument read requested %v, want /api/cost", seen)
	}

	seen = nil
	got, err := p.Read(context.Background(), adminmcp.ToolSpendRead, map[string]any{"hours": float64(24), "limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/api/cost/agents?hours=24" {
		t.Fatalf("window read requested %v, want /api/cost/agents?hours=24", seen)
	}
	out := got.(map[string]any)
	if rows := out["agents"].([]any); len(rows) != 1 || out["agents_truncated"] != true {
		t.Fatalf("answer = %#v, want agents capped to 1", out)
	}

	seen = nil
	for _, bad := range []map[string]any{
		{"hours": float64(-1)},
		{"hours": float64(721)},
		{"hours": float64(1.5)},
		{"since": "2026-11-18T00:00:00Z", "until": "2026-11-17T00:00:00Z"},
		{"since": "2026-11-18T00:00:00Z", "hours": float64(3)},
		{"since": "yesterday"},
	} {
		if _, err := p.Read(context.Background(), adminmcp.ToolSpendRead, bad); err == nil {
			t.Errorf("%v accepted, want a refusal", bad)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("a refused read reached the router: %v", seen)
	}
}

// The real handler accepts the tool's window query, and the MCP's answer for
// "the last 24 hours" is the dashboard's answer for the same range.
func TestCostAgentsEndpointAcceptsTheToolsQuery(t *testing.T) {
	s := newTestServer()
	now := time.Now().UTC()
	s.SeedCostHistory([]CostHistoryEntry{
		agentSnap(now.Add(-3*time.Hour), map[string]float64{"scanner": 1, "reviewer": 1}, map[string]CostAgentSnap{"scanner": {Input: 100}, "reviewer": {Input: 10}}),
		agentSnap(now.Add(-2*time.Hour), map[string]float64{"scanner": 2, "reviewer": 5}, map[string]CostAgentSnap{"scanner": {Input: 900}, "reviewer": {Input: 20}}),
	})
	path, err := adminmcp.SpendReadPath(map[string]any{"hours": float64(24)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleCostAgents(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
	}
	var mcp costAgentWindowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mcp); err != nil {
		t.Fatal(err)
	}
	dash := costAgentWindow(s.CostHistory(), now.Add(-24*time.Hour).UnixMilli(), now.UnixMilli())
	if len(mcp.Agents) != 2 || mcp.Agents[0].Name != "reviewer" || mcp.Agents[0].USD != dash.Agents[0].USD || mcp.Agents[1].USD != dash.Agents[1].USD {
		t.Fatalf("mcp rows %+v, dashboard rows %+v", mcp.Agents, dash.Agents)
	}
	if !strings.Contains(rec.Body.String(), `"coverage":"partial"`) && !strings.Contains(rec.Body.String(), `"coverage":"full"`) {
		t.Errorf("answer does not state its coverage: %s", rec.Body.String())
	}
}
