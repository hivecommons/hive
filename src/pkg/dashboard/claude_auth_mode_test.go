package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/claude/claudetest"
	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

const claudeAuthModeSentinel = "sk-ant-oat01-STATUS-SENTINEL-4b8e2d6f1a"

// claudeAuthModeServer builds a dashboard whose log goes to the returned
// buffer, with the operator token hermetic: token "" leaves it unset.
func claudeAuthModeServer(t *testing.T, token string) (*Server, *bytes.Buffer) {
	t.Helper()
	claudetest.SetOperatorToken(t, token)
	origRead := claudeReadToken
	claudeReadToken = func(string) string { return "" }
	t.Cleanup(func() { claudeReadToken = origRead })

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.Logger = logger
	s.RegisterAPI(deps)
	return s, &buf
}

func readClaudeAuthStatus(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	rec := doGet(s, "/api/claude-auth/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/claude-auth/status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got, rec.Body.String()
}

func readClaudeAuthStatusMCP(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	provider := dashboardAdminMCPProvider{server: s, role: config.RoleReadWrite}
	data, err := provider.Read(context.Background(), adminmcp.ToolClaudeAuthStatus, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := adminmcp.EncodeResult(data)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := data.(map[string]any)
	return got, string(out)
}

// With the operator's long-lived token set, the dashboard status and the
// admin MCP claude_auth_status read both report mode "long-lived token" and
// logged in, even with no dashboard sign-in on disk (#17759).
func TestClaudeAuthStatus_LongLivedTokenMode(t *testing.T) {
	s, _ := claudeAuthModeServer(t, claudeAuthModeSentinel)

	got, _ := readClaudeAuthStatus(t, s)
	if got["mode"] != claude.AuthModeLongLivedToken || got["logged_in"] != true {
		t.Fatalf("dashboard status = %v; want mode %q, logged_in true", got, claude.AuthModeLongLivedToken)
	}
	mcp, _ := readClaudeAuthStatusMCP(t, s)
	if mcp["mode"] != claude.AuthModeLongLivedToken || mcp["logged_in"] != true {
		t.Fatalf("admin MCP claude_auth_status = %v; want mode %q, logged_in true", mcp, claude.AuthModeLongLivedToken)
	}
}

// Without it, both surfaces report the dashboard sign-in, logged in only when
// the sign-in's credential is on disk.
func TestClaudeAuthStatus_DashboardSignInMode(t *testing.T) {
	s, _ := claudeAuthModeServer(t, "")

	got, _ := readClaudeAuthStatus(t, s)
	if got["mode"] != claude.AuthModeDashboardSignIn || got["logged_in"] != false {
		t.Fatalf("dashboard status = %v; want mode %q, logged_in false", got, claude.AuthModeDashboardSignIn)
	}
	mcp, _ := readClaudeAuthStatusMCP(t, s)
	if mcp["mode"] != claude.AuthModeDashboardSignIn || mcp["logged_in"] != false {
		t.Fatalf("admin MCP claude_auth_status = %v; want mode %q", mcp, claude.AuthModeDashboardSignIn)
	}

	claudeReadToken = func(string) string { return "sk-ant-oat-dashboard" }
	if got, _ := readClaudeAuthStatus(t, s); got["mode"] != claude.AuthModeDashboardSignIn || got["logged_in"] != true {
		t.Fatalf("dashboard status with a sign-in = %v; want mode %q, logged_in true", got, claude.AuthModeDashboardSignIn)
	}
}

// The token value, or any prefix of it, never reaches the status API, the
// admin MCP read, or the hive log.
func TestClaudeAuthStatus_NeverExposesToken(t *testing.T) {
	s, logs := claudeAuthModeServer(t, claudeAuthModeSentinel)
	prefix := claudeAuthModeSentinel[:len("sk-ant-oat01-STATUS")]

	_, body := readClaudeAuthStatus(t, s)
	_, mcp := readClaudeAuthStatusMCP(t, s)
	for surface, out := range map[string]string{"status API": body, "admin MCP": mcp, "hive log": logs.String()} {
		if strings.Contains(out, prefix) {
			t.Fatalf("%s exposed the operator token: %s", surface, out)
		}
	}
}
