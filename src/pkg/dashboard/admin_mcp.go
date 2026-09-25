package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

type dashboardAdminMCPProvider struct {
	server        *Server
	user          string
	role          string
	authorization string
}

func (s *Server) handleAdminMCP(w http.ResponseWriter, r *http.Request) {
	provider := dashboardAdminMCPProvider{
		server:        s,
		user:          r.Header.Get("X-Hive-User"),
		role:          r.Header.Get("X-Hive-Role"),
		authorization: r.Header.Get("Authorization"),
	}
	adminmcp.NewHandler(provider, adminmcp.WithWritesEnabled(adminMCPWritesEnabled()), adminmcp.WithPendingStore(adminmcp.NewFilePendingStore(adminMCPPendingPath())), adminmcp.WithHiveID("dashboard")).ServeHTTP(w, r)
}

func (p dashboardAdminMCPProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if p.server == nil {
		return nil, fmt.Errorf("%w: dashboard server unavailable", adminmcp.ErrForbidden)
	}
	path, ok := adminMCPReadPath(tool, adminmcp.LimitFromArgs(args))
	if !ok {
		return nil, fmt.Errorf("%w: unsupported admin MCP read tool", adminmcp.ErrForbidden)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if p.user != "" {
		req.Header.Set("X-Hive-User", p.user)
	}
	if p.role != "" {
		req.Header.Set("X-Hive-Role", p.role)
	}
	p.server.mux.ServeHTTP(rec, req)
	if rec.Code < http.StatusOK || rec.Code >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("admin MCP read %s returned HTTP %d", tool, rec.Code)
	}
	data, err := decodeAdminMCPJSON(rec.Body.Bytes())
	if err != nil {
		return nil, err
	}
	return adminmcp.CapResult(data, adminmcp.LimitFromArgs(args)), nil
}

func adminMCPReadPath(tool string, limit int) (string, bool) {
	return adminmcp.ReadPath(tool, limit)
}

func decodeAdminMCPJSON(data []byte) (any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}
	var out any
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p dashboardAdminMCPProvider) ExecuteWrite(_ context.Context, req adminmcp.WriteRequest) (any, error) {
	if p.server == nil {
		return nil, fmt.Errorf("%w: dashboard server unavailable", adminmcp.ErrForbidden)
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(req.Method, req.Path, nil)
	if strings.TrimSpace(p.authorization) != "" {
		httpReq.Header.Set("Authorization", p.authorization)
	}
	p.server.authenticate(p.server.roleEnforcement(p.server.securityHeaders(p.server.mux))).ServeHTTP(rec, httpReq)
	if rec.Code < http.StatusOK || rec.Code >= http.StatusMultipleChoices {
		return nil, &adminmcp.HiveRefusalError{StatusCode: rec.Code, Message: strings.TrimSpace(rec.Body.String()), Body: rec.Body.Bytes()}
	}
	data, err := decodeAdminMCPJSON(rec.Body.Bytes())
	if err != nil {
		return nil, err
	}
	return data, nil
}

func adminMCPWritesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HIVE_ADMIN_MCP_ENABLE_WRITES"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func adminMCPPendingPath() string {
	if path := strings.TrimSpace(os.Getenv("HIVE_ADMIN_MCP_PENDING_FILE")); path != "" {
		return path
	}
	return "/data/admin-mcp-pending-confirmations.json"
}
