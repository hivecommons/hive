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
	"time"

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

func TestParseStdioBuildFromUserAgent(t *testing.T) {
	for _, tc := range []struct {
		ua   string
		want string
	}{
		{"hive-admin-mcp/0.1.0+abc1234", "0.1.0+abc1234"},
		{"hive-admin-mcp/2.0.0+def5678", "2.0.0+def5678"},
		{"hive-admin-mcp/0.1.0+unknown", "0.1.0+unknown"},
		{"hive-admin-mcp/0.1.0+abc1234 (linux/amd64)", "0.1.0+abc1234"},
		// hivectl's default UA is also what the regular CLI sends to
		// /api/status and /api/version; it must never be recorded.
		{"hivectl/0.1", ""},
		{"hivectl/0.1 (linux/amd64)", ""},
		// Shape and length validation: only <version>+<7-40 hex|unknown>.
		{"hive-admin-mcp/", ""},
		{"hive-admin-mcp/0.1.0", ""},
		{"hive-admin-mcp/0.1.0+abc", ""},
		{"hive-admin-mcp/0.1.0+zzzzzzz", ""},
		{"hive-admin-mcp/<script>+abc1234", ""},
		{"hive-admin-mcp/0.1.0+" + strings.Repeat("a", 41), ""},
		{"hive-admin-mcp/" + strings.Repeat("9", stdioBuildMaxLen) + "+abc1234", ""},
		{"Mozilla/5.0", ""},
		{"curl/8.0", ""},
		{"", ""},
	} {
		if got := parseStdioBuildFromUserAgent(tc.ua); got != tc.want {
			t.Errorf("parseStdioBuildFromUserAgent(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

func TestAdminMCPRecordsStdioUserAgent(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/version", s.handleVersion)

	before := time.Now().UTC().Add(-time.Second)

	// Admin MCP read path with hive-admin-mcp User-Agent
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"version_read","arguments":{}}}`))
	req.Header.Set("User-Agent", "hive-admin-mcp/0.1.0+abc1234")
	s.handleAdminMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	build, at := s.LastSeenStdio()
	if build != "0.1.0+abc1234" {
		t.Fatalf("recorded build = %q, want %q", build, "0.1.0+abc1234")
	}
	if at.Before(before) || at.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("recorded timestamp %v not within expected range", at)
	}

	// Verify version_read tool returns last_seen_stdio_build, last_seen_stdio_at, and hub_build
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpcResp); err != nil {
		t.Fatal(err)
	}
	if len(rpcResp.Result.Content) == 0 {
		t.Fatalf("no content in result: %s", rec.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(rpcResp.Result.Content[0].Text), &env); err != nil {
		t.Fatal(err)
	}
	data := env.Data
	if got, _ := data["last_seen_stdio_build"].(string); got != "0.1.0+abc1234" {
		t.Fatalf("last_seen_stdio_build = %q, want 0.1.0+abc1234", got)
	}
	if got, _ := data["last_seen_stdio_at"].(string); got == "" {
		t.Fatal("last_seen_stdio_at is empty")
	}
	if got, _ := data["hub_build"].(string); got == "" {
		t.Fatal("hub_build is empty")
	}

	// Verify GET /api/version also returns last_seen_stdio_build, last_seen_stdio_at, and hub_build
	vRec := httptest.NewRecorder()
	vReq := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	s.handleVersion(vRec, vReq)
	if vRec.Code != http.StatusOK {
		t.Fatalf("GET /api/version status = %d", vRec.Code)
	}
	var vData map[string]any
	if err := json.Unmarshal(vRec.Body.Bytes(), &vData); err != nil {
		t.Fatal(err)
	}
	if got, _ := vData["last_seen_stdio_build"].(string); got != "0.1.0+abc1234" {
		t.Fatalf("/api/version last_seen_stdio_build = %q, want 0.1.0+abc1234", got)
	}
	if got, _ := vData["last_seen_stdio_at"].(string); got == "" {
		t.Fatal("/api/version last_seen_stdio_at is empty")
	}
	if got, _ := vData["hub_build"].(string); got == "" {
		t.Fatal("/api/version hub_build is empty")
	}
}

// TestHivectlUserAgentNeverOverwritesStdioSighting: the regular hivectl CLI
// (default UA hivectl/0.1) hits /api/status, /api/status/summary and
// /api/version, and dashboard sessions poll them too. None of those may clobber
// a genuine hive-admin-mcp sighting, or the skew badge would vanish (#12318).
func TestHivectlUserAgentNeverOverwritesStdioSighting(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/version", s.handleVersion)

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.Header.Set("User-Agent", "hive-admin-mcp/0.1.0+abc1234")
	s.handleVersion(httptest.NewRecorder(), req)
	if build, _ := s.LastSeenStdio(); build != "0.1.0+abc1234" {
		t.Fatalf("seed sighting = %q", build)
	}
	_, seededAt := s.LastSeenStdio()

	for _, ua := range []string{
		"hivectl/0.1",
		"hivectl/0.1 (linux/amd64)",
		"Mozilla/5.0",
		"hive-admin-mcp/not+a+build",
		"hive-admin-mcp/0.1.0+" + strings.Repeat("f", 200),
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/version", nil)
		r.Header.Set("User-Agent", ua)
		s.handleVersion(httptest.NewRecorder(), r)
		// handleStatus / handleStatusSummary share this hook.
		s.RecordStdioFromRequest(r)
		mcp := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"version_read","arguments":{}}}`))
		mcp.Header.Set("User-Agent", ua)
		s.handleAdminMCP(httptest.NewRecorder(), mcp)

		build, at := s.LastSeenStdio()
		if build != "0.1.0+abc1234" || !at.Equal(seededAt) {
			t.Fatalf("UA %q overwrote sighting: build=%q at=%v (want 0.1.0+abc1234 at %v)", ua, build, at, seededAt)
		}
	}

	// A newer genuine stdio build still replaces the earlier one.
	req = httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.Header.Set("User-Agent", "hive-admin-mcp/0.2.0+def5678")
	s.handleVersion(httptest.NewRecorder(), req)
	if build, _ := s.LastSeenStdio(); build != "0.2.0+def5678" {
		t.Fatalf("newer stdio build not recorded: %q", build)
	}
}

func TestAdminMCPNoPriorStdioSighting(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/version", s.handleVersion)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"version_read","arguments":{}}}`))
	s.handleAdminMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpcResp); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(rpcResp.Result.Content[0].Text), &env); err != nil {
		t.Fatal(err)
	}
	data := env.Data
	if _, ok := data["last_seen_stdio_build"]; ok {
		t.Fatalf("unexpected last_seen_stdio_build on fresh server: %#v", data["last_seen_stdio_build"])
	}
	if _, ok := data["last_seen_stdio_at"]; ok {
		t.Fatalf("unexpected last_seen_stdio_at on fresh server: %#v", data["last_seen_stdio_at"])
	}
	if got, _ := data["hub_build"].(string); got == "" {
		t.Fatal("hub_build is missing on fresh server")
	}
}

func TestVersionEndpointRecordsStdioUserAgent(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/version", s.handleVersion)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.Header.Set("User-Agent", "hive-admin-mcp/2.0.0+def5678")
	s.handleVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	build, at := s.LastSeenStdio()
	if build != "2.0.0+def5678" {
		t.Fatalf("recorded build = %q, want '2.0.0+def5678'", build)
	}
	if at.IsZero() {
		t.Fatal("recorded timestamp is zero")
	}

	var vData map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &vData); err != nil {
		t.Fatal(err)
	}
	if got, _ := vData["last_seen_stdio_build"].(string); got != "2.0.0+def5678" {
		t.Fatalf("version response last_seen_stdio_build = %q, want '2.0.0+def5678'", got)
	}
}
