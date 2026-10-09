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
	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
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

func TestAdminMCPAdvisorRecordsMatchesREST(t *testing.T) {
	s := advisorTestServer(t)
	// provider.Read serves through s.mux, so the REST routes must be registered.
	s.RegisterAPI(testDeps(t))
	store := advisor.NewStore("")
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2026-01-01T00:00:00Z", Severity: "concern", Text: "old flag"})
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2026-03-01T00:00:00Z", Severity: "blocker", Text: "march flag"})
	store.Append(advisor.Record{Agent: "other", Timestamp: "2026-03-02T00:00:00Z", Severity: "concern", Text: "other flag"})
	s.SetAdvisorRecords(store)

	args := map[string]any{"agent": "scout", "since": "2026-02-01T00:00:00Z", "until": "2026-04-01T00:00:00Z"}
	path, ok := adminMCPReadPath(adminmcp.ToolAdvisorRecords, args)
	if !ok || !strings.HasPrefix(path, "/api/advisor/records?") {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
	provider := dashboardAdminMCPProvider{server: s, role: config.RoleReadWrite}
	result, err := provider.Read(context.Background(), adminmcp.ToolAdvisorRecords, args)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := result.(map[string]any)
	records, _ := out["records"].([]any)
	if len(records) != 1 || records[0].(map[string]any)["text"] != "march flag" {
		t.Fatalf("admin MCP must return what REST returns for the same agent and window: %#v", result)
	}

	fleet, err := provider.Read(context.Background(), adminmcp.ToolAdvisorRecords, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fleet.(map[string]any)["records"].([]any); len(got) != 3 {
		t.Fatalf("fleet-wide read: %#v", fleet)
	}

	readOnly := dashboardAdminMCPProvider{server: s, role: config.RoleRead}
	if _, err := readOnly.Read(context.Background(), adminmcp.ToolAdvisorRecords, args); err == nil {
		t.Fatal("advisor records must keep the REST read-write floor through admin MCP")
	}
}

// TestDashboardAdminMCPVersionReadReturnsAPIVersion: version_read answers with
// the same currentCommit the authenticated /api/version reports (#11220).
func TestDashboardAdminMCPVersionReadReturnsAPIVersion(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"currentCommit": "1ffbbae", "branch": "v6"})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"version_read","arguments":{}}}`))
	adminmcp.NewHandler(dashboardAdminMCPProvider{server: s}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1ffbbae") || !strings.Contains(rec.Body.String(), "currentCommit") {
		t.Fatalf("version_read body = %s", rec.Body.String())
	}
}
