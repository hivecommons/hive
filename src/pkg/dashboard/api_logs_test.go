package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logread"
)

// writeHiveLog writes n slog-shaped records into a temp log directory; extra
// messages are appended after them.
func writeHiveLog(t *testing.T, n int, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"time":%q,"level":"INFO","msg":"line %04d"}`+"\n", start.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), i)
	}
	for i, msg := range extra {
		fmt.Fprintf(&b, `{"time":%q,"level":"WARN","msg":%q}`+"\n", start.Add(time.Duration(n+i)*time.Second).Format(time.RFC3339Nano), msg)
	}
	if err := os.WriteFile(filepath.Join(dir, logread.HiveLogFilename), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func logTestDeps(dir string) *Dependencies {
	cfg := &config.Config{}
	cfg.Governor.Logging.Dir = dir
	return &Dependencies{Config: cfg}
}

func logTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	t.Cleanup(config.SetSecretFileRootsForTest(t.TempDir()))
	s := &Server{mux: http.NewServeMux(), deps: logTestDeps(dir)}
	s.registerLogReadRoutes()
	return s
}

func serveLog(s *Server, role, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestLogReadRequiresReadWrite(t *testing.T) {
	s := logTestServer(t, writeHiveLog(t, 5))
	for _, tc := range []struct {
		role string
		code int
	}{
		{"", http.StatusForbidden},
		{config.RoleRead, http.StatusForbidden},
		{config.RoleReadWrite, http.StatusOK},
		{config.RoleOwner, http.StatusOK},
	} {
		rec := serveLog(s, tc.role, "/api/logs/read")
		if rec.Code != tc.code {
			t.Fatalf("role %q: status %d, want %d (%s)", tc.role, rec.Code, tc.code, rec.Body.String())
		}
		if tc.code != http.StatusOK {
			if strings.Contains(rec.Body.String(), "line 000") {
				t.Fatalf("role %q saw log text: %s", tc.role, rec.Body.String())
			}
			continue
		}
		var res logread.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Status != logread.StatusOK || res.LineCount != 5 || res.Untrusted != logread.UntrustedMarker {
			t.Fatalf("role %q: %+v", tc.role, res)
		}
	}
	// The sources list names sources only, so the read role may see it.
	rec := serveLog(s, config.RoleRead, "/api/logs/sources")
	var list logread.SourcesResult
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("sources: %d %s", rec.Code, rec.Body.String())
	}
	if list.Default != logread.SourceHive || len(list.Sources) != 7 || !list.Sources[0].Available {
		t.Fatalf("sources = %+v", list)
	}
}

func TestLogReadRequestValidation(t *testing.T) {
	s := logTestServer(t, writeHiveLog(t, 30))
	for _, q := range []string{
		"since=2026-03-01T12:00:00",
		"until=yesterday",
		"since=2026-03-01T12:00:10Z&until=2026-03-01T12:00:00Z",
		"min_level=fatal",
		"lines=many",
	} {
		rec := serveLog(s, config.RoleReadWrite, "/api/logs/read?"+q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
	}
	rec := serveLog(s, config.RoleReadWrite, "/api/logs/read?lines=3&since=2026-03-01T07:00:05-05:00&until=2026-03-01T12:00:20Z&min_level=info&include=LINE&exclude=0020&exclude=0019")
	var res logread.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if res.LineCount != 3 || !strings.Contains(res.Lines[2].Text, "line 0018") || res.Window.Since != "2026-03-01T12:00:05Z" || !res.HasMore {
		t.Fatalf("res = %+v", res)
	}
	if unknown := serveLog(s, config.RoleReadWrite, "/api/logs/read?source=kernel"); !strings.Contains(unknown.Body.String(), logread.StatusNotAvailable) || unknown.Code != http.StatusOK {
		t.Fatalf("unknown source: %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestLogReadWorkBoundFromConfig(t *testing.T) {
	s := logTestServer(t, writeHiveLog(t, 10))
	s.deps.Config.Governor.Logging.ReadScanMaxMB = 1
	opts := s.logReadOptions(nil)
	if opts.WorkBoundBytes != 1<<20 || opts.LogDir == "" {
		t.Fatalf("opts = %+v", opts)
	}
	if got := (&Server{}).logReadOptions(nil); got.LogDir != "" || got.WorkBoundBytes != 0 {
		t.Fatalf("no deps: %+v", got)
	}
}

// TestLogReadOverBearerScrubsOwnSecrets is the path the standalone admin MCP
// binary takes: the dashboard token as a Bearer header through the full
// middleware chain. The token itself, written into the log, comes back
// redacted, and filtering on it matches nothing.
func TestLogReadOverBearerScrubsOwnSecrets(t *testing.T) {
	const token = "dashboard-token-0123456789"
	t.Setenv("FOO_PASSPHRASE", "env-passphrase-abcdefgh")
	t.Cleanup(config.SetSecretFileRootsForTest(t.TempDir()))
	dir := writeHiveLog(t, 2, "probe used ?t="+token, "env passphrase env-passphrase-abcdefgh leaked")
	s := NewServerWithAuth(0, token, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = logTestDeps(dir)
	s.registerLogReadRoutes()
	h := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/logs/read", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauth.Code)
	}

	read := func(query string) logread.Result {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/logs/read"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "env-passphrase-abcdefgh") {
			t.Fatalf("own secret leaked: %s", rec.Body.String())
		}
		var res logread.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := read("")
	if res.LineCount != 4 {
		t.Fatalf("lines = %d", res.LineCount)
	}
	var counted int
	for _, r := range res.Redactions {
		if r.Kind == logread.SecretValueKind {
			counted = r.Count
		}
	}
	if counted != 2 {
		t.Fatalf("redactions = %+v", res.Redactions)
	}
	if filtered := read("?include=" + token); filtered.LineCount != 0 {
		t.Fatalf("include on a raw secret matched %d lines", filtered.LineCount)
	}
}

// TestAdminMCPLogReadThroughProvider: the in-hive admin MCP provider serves
// log_read through the same route, uncapped by the shared list limit, and
// keeps the read-write floor.
func TestAdminMCPLogReadThroughProvider(t *testing.T) {
	s := logTestServer(t, writeHiveLog(t, 300))
	provider := dashboardAdminMCPProvider{server: s, role: config.RoleReadWrite}
	data, err := provider.Read(context.Background(), adminmcp.ToolLogRead, map[string]any{"lines": float64(250)})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := data.(map[string]any)
	if lines, _ := m["lines"].([]any); len(lines) != 250 || m["_admin_mcp"] != nil {
		t.Fatalf("log_read through admin MCP returned %d lines", len(lines))
	}
	if _, err := provider.Read(context.Background(), adminmcp.ToolLogRead, map[string]any{"since": "2026-03-01T12:00:00"}); err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("offset-less since: %v", err)
	}
	sources, err := provider.Read(context.Background(), adminmcp.ToolLogSources, map[string]any{})
	if err != nil || sources.(map[string]any)["default_source"] != logread.SourceHive {
		t.Fatalf("sources = %#v, %v", sources, err)
	}

	readOnly := dashboardAdminMCPProvider{server: s, role: config.RoleRead}
	if _, err := readOnly.Read(context.Background(), adminmcp.ToolLogRead, map[string]any{}); err == nil {
		t.Fatal("log_read must keep the read-write floor through admin MCP")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"log_read","arguments":{"lines":3}}}`))
	adminmcp.NewHandler(provider).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "line 0299") || !strings.Contains(rec.Body.String(), "UNTRUSTED CONTENT") {
		t.Fatalf("tools/call log_read = %d %s", rec.Code, rec.Body.String())
	}
}
