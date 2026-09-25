package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

type dashboardAdminMCPProvider struct{ server *Server }

func (s *Server) handleAdminMCP(w http.ResponseWriter, r *http.Request) {
	adminmcp.NewHandler(dashboardAdminMCPProvider{server: s}).ServeHTTP(w, r)
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
	values := url.Values{}
	if limit > 0 {
		values.Set("limit", fmt.Sprint(limit))
	}
	suffix := ""
	if encoded := values.Encode(); encoded != "" {
		suffix = "?" + encoded
	}
	switch tool {
	case adminmcp.ToolHiveStatus:
		return "/api/status/summary", true
	case adminmcp.ToolAgentsList:
		return "/api/agents" + suffix, true
	case adminmcp.ToolRunsList:
		return "/api/runs" + suffix, true
	case adminmcp.ToolClaimsList:
		return "/api/claims" + suffix, true
	default:
		return "", false
	}
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
