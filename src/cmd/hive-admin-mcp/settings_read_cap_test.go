package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// settings_read went through the list tools' row cap (20 by default) although
// it has no limit argument, so the governor settings of a hive with 30 agents
// came back with 20 and no way to read the rest.
func TestStdioSettingsReadReturnsTheWholeSection(t *testing.T) {
	agents := make([]any, 30)
	for i := range agents {
		agents[i] = map[string]any{"name": fmt.Sprintf("agent-%02d", i), "cadence": "15m"}
	}
	backends := make([]any, 25)
	for i := range backends {
		backends[i] = map[string]any{"name": fmt.Sprintf("backend-%02d", i)}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config/governor", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents, "cadenceScope": "global"})
	})
	mux.HandleFunc("GET /api/config/backends", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(backends)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	p := readProvider{roster: r}

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

// The list tools keep their cap.
func TestStdioListToolsStillCapRows(t *testing.T) {
	agents := make([]any, 30)
	for i := range agents {
		agents[i] = map[string]any{"name": fmt.Sprintf("agent-%02d", i)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agents)
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	data, err := (readProvider{roster: r}).Read(context.Background(), adminmcp.ToolAgentsList, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	capped, _ := data.(map[string]any)
	if items, _ := capped["items"].([]any); len(items) != adminmcp.DefaultResultLimit || capped["truncated"] != true {
		t.Fatalf("agents_list = %v", capped)
	}
}
