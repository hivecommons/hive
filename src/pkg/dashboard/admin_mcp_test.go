package dashboard

import (
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

func TestAdminMCPExecuteWriteForwardsJSONBody(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.mux.HandleFunc("POST /api/admin-mcp-test-body", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["repo"] != "hivecommons/hive" {
			t.Fatalf("body = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "repo": body["repo"]})
	})
	provider := dashboardAdminMCPProvider{server: s, authorization: "Bearer secret"}
	result, err := provider.ExecuteWrite(t.Context(), adminmcp.WriteRequest{Method: http.MethodPost, Path: "/api/admin-mcp-test-body", Body: map[string]any{"repo": "hivecommons/hive"}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(map[string]any)
	if !ok || got["repo"] != "hivecommons/hive" {
		t.Fatalf("result = %#v", result)
	}
}
