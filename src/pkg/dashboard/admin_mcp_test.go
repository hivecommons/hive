package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

func TestAdminMCPEndpointUsesDashboardAuthentication(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauth.Code)
	}

	auth := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(auth, req)
	if auth.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d body=%s", auth.Code, auth.Body.String())
	}
	if !strings.Contains(auth.Body.String(), adminmcp.ToolHiveStatus) {
		t.Fatalf("tools/list body = %s", auth.Body.String())
	}
}

func TestAdminMCPEndpointDoesNotExposeHiveSelector(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer secret")
	s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux))).ServeHTTP(rec, req)
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, tool := range resp.Result.Tools {
		if strings.Contains(tool.Name, "select") {
			t.Fatalf("endpoint exposed selector tool %q", tool.Name)
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["hive"]; ok {
			t.Fatalf("endpoint tool %q accepts hive selector", tool.Name)
		}
	}
}

func TestDashboardAdminMCPOutboundTextIsScrubbed(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	githubToken := "gh" + "o_" + strings.Repeat("a", 24)
	bearer := "Bearer " + strings.Repeat("b", 20)
	s.mux.HandleFunc("GET /api/status/summary", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"title": githubToken + " " + bearer})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`))
	adminmcp.NewHandler(dashboardAdminMCPProvider{server: s}).ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), githubToken) || strings.Contains(rec.Body.String(), bearer) {
		t.Fatalf("dashboard MCP body retained raw text: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "redacted:github-token") || !strings.Contains(rec.Body.String(), "redacted:bearer-token") {
		t.Fatalf("dashboard MCP body missing markers: %s", rec.Body.String())
	}
}

func TestAdminMCPExecuteWriteForwardsJSONBodyAndAuth(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	var gotBody map[string]any
	var sawAuth bool
	s.mux.HandleFunc("PUT /api/admin-mcp-test/body", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("X-Hive-Role") == "owner"
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("content-type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	provider := dashboardAdminMCPProvider{server: s, authorization: "Bearer " + s.authToken}
	result, err := provider.ExecuteWrite(context.Background(), adminmcp.WriteRequest{Method: http.MethodPut, Path: "/api/admin-mcp-test/body", Body: map[string]any{"level": 5}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth {
		t.Fatal("write was not authenticated as owner")
	}
	if gotBody["level"] != float64(5) {
		t.Fatalf("body = %#v", gotBody)
	}
	out, ok := result.(map[string]any)
	if !ok || out["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestAdminMCPAgentNudgeStatusReadPath(t *testing.T) {
	path, ok := adminMCPReadPath(adminmcp.ToolAgentNudgeStatus, map[string]any{"agent": "team/scanner"})
	if !ok || path != "/api/kick/team%2Fscanner/status" {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
	if _, ok := adminMCPReadPath(adminmcp.ToolAgentNudgeStatus, map[string]any{}); ok {
		t.Fatal("expected missing agent to be rejected")
	}
}

func TestAdminMCPWritePreviewFailsFastWithoutDashboardBearer(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))
	preview := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`

	// Admitted as owner through the internal header, which ExecuteWrite does not
	// forward: confirm would 401, so preview must refuse instead of minting a
	// confirmation.
	internal := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(preview))
	req.Header.Set("X-Hive-Internal", "secret")
	h.ServeHTTP(internal, req)
	if internal.Code != http.StatusOK {
		t.Fatalf("internal status = %d body=%s", internal.Code, internal.Body.String())
	}
	if strings.Contains(internal.Body.String(), "confirmation_id") || !strings.Contains(internal.Body.String(), "Authorization: Bearer") {
		t.Fatalf("preview without bearer = %s", internal.Body.String())
	}

	bearer := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(preview))
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(bearer, req)
	if !strings.Contains(bearer.Body.String(), "confirmation_id") {
		t.Fatalf("preview with bearer = %s", bearer.Body.String())
	}
}
