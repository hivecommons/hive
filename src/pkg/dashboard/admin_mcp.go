package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

type dashboardAdminMCPProvider struct {
	server *Server
	user   string
	role   string
}

func (s *Server) handleAdminMCP(w http.ResponseWriter, r *http.Request) {
	adminmcp.NewHandler(dashboardAdminMCPProvider{
		server: s,
		user:   r.Header.Get("X-Hive-User"),
		role:   r.Header.Get("X-Hive-Role"),
	}).ServeHTTP(w, r)
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
