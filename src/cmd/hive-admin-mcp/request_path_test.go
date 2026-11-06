package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// hubCounting serves every route and counts the requests that reach it.
func hubCounting(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Value) {
	t.Helper()
	var hits atomic.Int64
	var last atomic.Value
	last.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store(r.Method + " " + r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// Registered as on the hive, so the router's redirect to the cleaned
	// path would land on a real handler.
	mux.HandleFunc("POST /api/model/{agent}/{model}", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store("model " + r.PathValue("agent"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /api/self-upgrade", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store("SELF-UPGRADE")
		_, _ = w.Write([]byte(`{"status":"upgrading"}`))
	})
	mux.HandleFunc("GET /api/kick/{agent}/status", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store("kick-status " + r.PathValue("agent"))
		_, _ = w.Write([]byte(`{"status":"delivered"}`))
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store("FLEET-STATUS")
		_, _ = w.Write([]byte(`{"agents":[]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &hits, &last
}

// agent.model with agent ".." and model "self-upgrade" built the path
// /api/model/../self-upgrade. The hive's router redirects that to
// /api/self-upgrade and the stdio client followed the redirect with the same
// method and token, so a registered write ran an operation the admin MCP
// excludes.
func TestStdioWriteWithDotNameNeverReachesTheHive(t *testing.T) {
	server, hits, last := hubCounting(t)
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	p := readProvider{roster: r, writesEnabled: true, pendingStore: adminmcp.NewMemoryPendingStore()}

	_, err := p.callWrite(context.Background(), adminmcp.ToolWritePreview, map[string]any{"operation": adminmcp.WriteOpAgentModel, "args": map[string]any{"agent": "..", "model": "self-upgrade"}})
	if !errors.Is(err, adminmcp.ErrRequestPath) {
		t.Fatalf("preview = %v, want ErrRequestPath", err)
	}
	// The client itself refuses the path, whatever previewed it.
	if _, err := p.ExecuteWrite(context.Background(), adminmcp.WriteRequest{Method: http.MethodPost, Path: "/api/model/../self-upgrade"}); !errors.Is(err, adminmcp.ErrRequestPath) {
		t.Fatalf("ExecuteWrite = %v, want ErrRequestPath", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the hive; the last was %q", n, last.Load())
	}
}

// agent_nudge_status with agent ".." built /api/kick/../status and came back
// with the whole fleet status as if it were that agent's nudge outcome.
func TestStdioReadWithDotNameNeverReachesTheHive(t *testing.T) {
	server, hits, last := hubCounting(t)
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	p := readProvider{roster: r}
	for tool, args := range map[string]map[string]any{
		adminmcp.ToolAgentNudgeStatus: {"agent": ".."},
		adminmcp.ToolSettingsRead:     {"section": "agent_variables", "agent": ".."},
		adminmcp.ToolAgentKicksList:   {"agent": ".."},
		adminmcp.ToolAgentKickRead:    {"agent": "scanner", "kick_id": "."},
		adminmcp.ToolRunDetailRead:    {"run": ".."},
	} {
		if _, err := p.Read(context.Background(), tool, args); !errors.Is(err, adminmcp.ErrRequestPath) {
			t.Errorf("%s %v = %v, want ErrRequestPath", tool, args, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the hive; the last was %q", n, last.Load())
	}
	// An ordinary name still reads.
	if _, err := p.Read(context.Background(), adminmcp.ToolAgentNudgeStatus, map[string]any{"agent": "scanner"}); err != nil {
		t.Fatal(err)
	}
	if got := last.Load(); got != "kick-status scanner" {
		t.Fatalf("an ordinary read reached %q", got)
	}
}
