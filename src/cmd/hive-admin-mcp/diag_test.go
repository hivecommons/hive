package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// TestReadDiagToolsPassThrough: the diagnostic reads (#11943) go to the
// /api/diag routes with their own arguments, come back uncapped, map a hive
// refusal to the typed admin MCP error without its body, and refuse a bad
// argument before any request.
func TestReadDiagToolsPassThrough(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/diag/config":
			http.Error(w, "owner only: body-detail", http.StatusForbidden)
			return
		case "/api/diag/runs/r1/log":
			http.Error(w, "role read", http.StatusForbidden)
			return
		case "/api/diag/health":
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		entries := make([]map[string]any, 300)
		for i := range entries {
			entries[i] = map[string]any{"id": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "diag_text", "text": strings.Repeat("x", 100000), "kicks": entries})
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r}

	data, err := provider.Read(context.Background(), adminmcp.ToolAgentLogRead, map[string]any{"agent": "alpha", "max_bytes": float64(100000)})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := data.(map[string]any)
	if kicks, _ := m["kicks"].([]any); len(kicks) != 300 || m["_admin_mcp"] != nil || len(m["text"].(string)) != 100000 {
		t.Fatalf("diag result was capped: %d entries, %#v", len(kicks), m["_admin_mcp"])
	}
	if got := seen(); len(got) != 1 || got[0] != "/api/diag/agents/alpha/log?max_bytes=100000" {
		t.Fatalf("requests = %q", got)
	}

	_, err = provider.Read(context.Background(), adminmcp.ToolConfigEffectiveRead, map[string]any{})
	if !errors.Is(err, adminmcp.ErrOwnerUnverified) || strings.Contains(err.Error(), "body-detail") {
		t.Fatalf("config refusal: err = %v", err)
	}
	_, err = provider.Read(context.Background(), adminmcp.ToolRunLogRead, map[string]any{"run": "r1"})
	if !errors.Is(err, adminmcp.ErrForbidden) || errors.Is(err, adminmcp.ErrOwnerUnverified) {
		t.Fatalf("run log refusal: err = %v", err)
	}
	_, err = provider.Read(context.Background(), adminmcp.ToolHealthDeepRead, map[string]any{})
	if err == nil || errors.Is(err, adminmcp.ErrForbidden) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("health failure: err = %v", err)
	}

	before := len(seen())
	for _, args := range []map[string]any{{}, {"agent": "a", "kick_id": "../x"}} {
		if _, err := provider.Read(context.Background(), adminmcp.ToolAgentKickRead, args); err == nil {
			t.Fatalf("bad args %v accepted", args)
		}
	}
	if got := seen(); len(got) != before {
		t.Fatalf("a refused argument must not reach the hive: %q", got)
	}
}
