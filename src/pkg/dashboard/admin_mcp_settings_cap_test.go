package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// settings_read went through the list tools' row cap (20 by default) although
// it has no limit argument, so the governor settings of a hive with 30 agents
// came back with 20 and no way to read the rest.
func TestDashboardAdminMCPSettingsReadReturnsTheWholeSection(t *testing.T) {
	agents := make([]any, 30)
	for i := range agents {
		agents[i] = map[string]any{"name": fmt.Sprintf("agent-%02d", i), "cadence": "15m"}
	}
	backends := make([]any, 25)
	for i := range backends {
		backends[i] = map[string]any{"name": fmt.Sprintf("backend-%02d", i)}
	}
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/config/governor", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents, "cadenceScope": "global"})
	})
	s.mux.HandleFunc("GET /api/config/backends", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(backends)
	})
	p := dashboardAdminMCPProvider{server: s, role: "owner", ownerVerified: true}

	data, err := p.Read(context.Background(), adminmcp.ToolSettingsRead, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	governor, _ := data.(map[string]any)
	if kept, _ := governor["agents"].([]any); len(kept) != 30 {
		t.Fatalf("governor settings list %d of 30 agents", len(kept))
	}
	if _, cut := governor["_admin_mcp"]; cut {
		t.Fatalf("governor settings are marked truncated: %v", governor["_admin_mcp"])
	}

	data, err = p.Read(context.Background(), adminmcp.ToolSettingsRead, map[string]any{"section": "backends"})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := data.([]any); len(list) != 25 {
		t.Fatalf("backends section = %T with %d entries, want the list of 25", data, len(list))
	}
}
