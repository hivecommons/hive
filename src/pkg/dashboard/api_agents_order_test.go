package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

func agentsOrderServer(n int) *Server {
	agents := make(map[string]config.AgentConfig, n)
	for i := 0; i < n; i++ {
		agents[fmt.Sprintf("agent-%02d", i)] = config.AgentConfig{Enabled: true}
	}
	cfg := &config.Config{}
	cfg.Agents = agents
	s := &Server{mux: http.NewServeMux(), deps: &Dependencies{Config: cfg}}
	s.mux.HandleFunc("GET /api/agents", s.handleAgentsList)
	return s
}

// GET /api/agents walked the agents map, so every call answered in a
// different order.
func TestAgentsListIsSortedByName(t *testing.T) {
	s := agentsOrderServer(30)
	for call := 0; call < 5; call++ {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		var list []agentListEntry
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(list))
		for i, a := range list {
			names[i] = a.Name
		}
		if len(names) != 30 || !sort.StringsAreSorted(names) {
			t.Fatalf("call %d: agents are not in name order: %v", call, names)
		}
	}
}

// The admin MCP keeps the first `limit` agents. With a random order a hive
// with more agents than the limit got a different subset on every call, and
// no call could be relied on to show a given agent.
func TestAdminMCPAgentsListCapKeepsTheSameAgents(t *testing.T) {
	s := agentsOrderServer(30)
	p := dashboardAdminMCPProvider{server: s}
	first := ""
	for call := 0; call < 5; call++ {
		data, err := p.Read(context.Background(), adminmcp.ToolAgentsList, map[string]any{"limit": float64(10)})
		if err != nil {
			t.Fatal(err)
		}
		capped, _ := data.(map[string]any)
		items, _ := capped["items"].([]any)
		if len(items) != 10 || capped["total"] != 30 {
			t.Fatalf("capped list = %d items of %v", len(items), capped["total"])
		}
		b, _ := json.Marshal(items)
		if first == "" {
			first = string(b)
			if got := items[0].(map[string]any)["name"]; got != "agent-00" {
				t.Fatalf("first agent = %v, want agent-00", got)
			}
			continue
		}
		if string(b) != first {
			t.Fatalf("call %d kept different agents than the first call", call)
		}
	}
}
