package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stdio binary routes github_api_usage to the same GET as the dashboard
// endpoint does, GET /api/gh-rate-limits with the tool's limit as top and the
// per-caller view, and shapes the answer with the same function.
func TestStdioGitHubAPIUsageReadsTheRateLimitEndpoint(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"core":                  map[string]any{"limit": 15000, "remaining": 612, "reset": "2026-11-12T19:40:00Z", "observed_at": "2026-11-12T19:00:05Z"},
			"core_reset_in_seconds": 2395,
			"api_budget":            map[string]any{"mode": "conserve", "since": "2026-11-12T18:52:00Z", "skipped_steps": []string{"recommendations"}},
			"api_budget_thresholds": map[string]any{"reserve": 800, "critical": 250, "agent_reserve_floor": 400},
			"top_consumers": []any{
				map[string]any{"caller": "hive:eval_cycle", "endpoint": "/repos/{owner}/{repo}/pulls", "method": "GET", "requests": 500, "charged": 120, "not_modified": 380, "rate_limited": 0, "last_status": 200, "last_seen": "2026-11-12T19:00:00Z"},
				map[string]any{"caller": "agent:builder", "endpoint": "/repos/{owner}/{repo}/pulls/{number}", "method": "GET", "requests": 310, "charged": 300, "not_modified": 10, "rate_limited": 2, "last_status": 403, "last_seen": "2026-11-12T19:00:03Z"},
			},
			"top_consumers_total": 7,
			"by_caller": []any{
				map[string]any{"caller": "agent:builder", "requests": 420, "charged": 400, "not_modified": 20, "rate_limited": 2},
				map[string]any{"caller": "hive:eval_cycle", "requests": 700, "charged": 180, "not_modified": 520, "rate_limited": 0},
			},
			"window_totals":  map[string]any{"requests": 1120, "charged": 580, "not_modified": 540, "rate_limited": 2},
			"window_seconds": 3600,
			"agent_caps":     map[string]any{"fleet_cap": 300, "window_s": 3600, "agents": []any{map[string]any{"agent": "builder", "cap": 300, "source": "fleet", "calls": 300}}},
			"etag_cache":     map[string]any{"hits": 540, "misses": 580, "entries": 64},
		})
	}))
	defer server.Close()
	provider := readProvider{roster: &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, timeout: time.Second}}

	data, err := provider.Read(context.Background(), adminmcp.ToolGitHubAPIUsage, map[string]any{"limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := data.(map[string]any)
	rows, _ := out["top_consumers"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["caller"] != "agent:builder" || rows[1].(map[string]any)["caller"] != "hive:eval_cycle" {
		t.Fatalf("top_consumers = %#v, want agent:builder (300 charged) before hive:eval_cycle (120)", rows)
	}
	if out["top_consumers_total"] != 7 || out["top_consumers_truncated"] != true {
		t.Fatalf("total=%v truncated=%v", out["top_consumers_total"], out["top_consumers_truncated"])
	}
	if byCaller, _ := out["by_caller"].([]any); len(byCaller) != 2 || byCaller[0].(map[string]any)["charged"] != float64(400) {
		t.Fatalf("by_caller = %#v", out["by_caller"])
	}
	budget, _ := out["api_budget"].(map[string]any)
	core, _ := out["core"].(map[string]any)
	if budget["mode"] != "conserve" || budget["reserve"] != float64(800) || core["reset_in_seconds"] != float64(2395) || out["window_seconds"] != float64(3600) {
		t.Fatalf("api_budget=%#v core=%#v window_seconds=%v", budget, core, out["window_seconds"])
	}

	// The registered stdio handler answers with the same data, in the
	// admin MCP's data envelope.
	result, err := provider.handler(adminmcp.ToolGitHubAPIUsage)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("stdio result = %#v", result)
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(mustMarshalContentText(t, result.Content[0])), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data["type"] != adminmcp.ToolGitHubAPIUsage || envelope.Data["agent_caps"] == nil || envelope.Data["etag_cache"] == nil {
		t.Fatalf("stdio answer = %#v", envelope.Data)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "GET /api/gh-rate-limits?top=2&usage=1" || seen[1] != "GET /api/gh-rate-limits?top=20&usage=1" {
		t.Fatalf("requests = %q, want one GET per call with the limit as top and usage=1", seen)
	}
}
