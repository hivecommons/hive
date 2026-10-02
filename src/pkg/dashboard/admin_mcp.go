package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	adminmcp.NewHandler(provider,
		adminmcp.WithWritesEnabled(adminMCPWritesEnabled()),
		adminmcp.WithWritesUnavailable(s.adminMCPWritesUnavailableReason(provider.authorization)),
		adminmcp.WithPendingStore(adminmcp.NewFilePendingStore(adminMCPPendingPath())),
		adminmcp.WithHiveID("dashboard"),
	).ServeHTTP(w, r)
}

// adminMCPWritesUnavailableReason reports why a confirmed write could not
// authenticate for this caller. ExecuteWrite re-enters authenticate carrying
// only the caller's Authorization header (the write contract never forwards
// identity headers), so a caller admitted by session, hub proof or the internal
// header would preview successfully and then get 401 on confirm.
func (s *Server) adminMCPWritesUnavailableReason(authorization string) string {
	directRoute := s.directRouteAuthzEnabled()
	if s.authToken == "" && !directRoute {
		return ""
	}
	if directRoute {
		return "this spoke enforces per-user direct-route authorization, which refuses the shared dashboard token that admin MCP writes authenticate with"
	}
	if !secureCompare(strings.TrimSpace(authorization), "Bearer "+s.authToken) {
		return "admin MCP writes re-authenticate with the dashboard token (Authorization: Bearer); this request was admitted another way (session, hub proxy or internal header), so a confirmed write would be refused with 401"
	}
	return ""
}

func (p dashboardAdminMCPProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if p.server == nil {
		return nil, fmt.Errorf("%w: dashboard server unavailable", adminmcp.ErrForbidden)
	}
	path, ok := adminMCPReadPath(tool, args)
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
	if tool == adminmcp.ToolIssuesByBand || tool == adminmcp.ToolPrsByBand {
		return adminmcp.BandReadResult(tool, rekeyOverviewBandRows(data), args)
	}
	return adminmcp.CapResult(data, adminmcp.LimitFromArgs(args)), nil
}

// rekeyOverviewBandRows rewrites each row's "band" field from the Overview
// display label (e.g. "Confirm & close") that overviewIssueCSVRow /
// overviewPRCSVRow set it to, back to the band key (e.g. "done") the
// response's own bands[] already carries alongside that label. Without this,
// adminmcp.BandReadResult's band argument — documented and validated against
// the band keys — never matches a row's band field, so band=<key> always
// returns zero rows (#10018). The CSV/JSON Overview export itself is
// unaffected: rekeyOverviewBandRows only edits the decoded copy admin MCP
// feeds to BandReadResult, not the HTTP response already written to rec.
func rekeyOverviewBandRows(data any) any {
	body, ok := data.(map[string]any)
	if !ok {
		return data
	}
	bands, ok := body["bands"].([]any)
	if !ok {
		return data
	}
	labelToKey := make(map[string]string, len(bands))
	for _, raw := range bands {
		spec, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key, _ := spec["key"].(string)
		label, _ := spec["label"].(string)
		if key != "" && label != "" {
			labelToKey[label] = key
		}
	}
	rows, ok := body["rows"].([]any)
	if !ok {
		return data
	}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if label, ok := row["band"].(string); ok {
			if key, ok := labelToKey[label]; ok {
				row["band"] = key
			}
		}
	}
	return data
}

func adminMCPReadPath(tool string, args map[string]any) (string, bool) {
	if tool == adminmcp.ToolAgentNudgeStatus {
		agent := strings.TrimSpace(adminMCPStringArg(args, "agent"))
		if agent == "" {
			return "", false
		}
		return "/api/kick/" + url.PathEscape(agent) + "/status", true
	}
	if tool == adminmcp.ToolIssuesByBand || tool == adminmcp.ToolPrsByBand {
		return adminmcp.BandReadPath(tool, args), true
	}
	if tool == adminmcp.ToolAdvisorRecords {
		return adminmcp.AdvisorRecordsReadPath(args), true
	}
	return adminmcp.ReadPath(tool, adminmcp.LimitFromArgs(args))
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
	var rec writeRecorder = responseRecorderAdapter{ResponseRecorder: httptest.NewRecorder()}
	if req.Path == "/api/backup" {
		rec = newDiscardingWriteRecorder()
	}
	var body io.Reader
	if req.Body != nil {
		data, err := json.Marshal(req.Body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	httpReq := httptest.NewRequest(req.Method, req.Path, body)
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(p.authorization) != "" {
		httpReq.Header.Set("Authorization", p.authorization)
	}
	p.server.authenticate(p.server.roleEnforcement(p.server.securityHeaders(p.server.mux))).ServeHTTP(rec, httpReq)
	status := rec.statusCode()
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		body := rec.bodyBytes()
		return nil, &adminmcp.HiveRefusalError{StatusCode: status, Message: strings.TrimSpace(string(body)), Body: body}
	}
	if req.Path == "/api/backup" {
		return map[string]any{
			"ok":          true,
			"contentType": rec.Header().Get("Content-Type"),
			"bytes":       rec.bytesWritten(),
			"encrypted":   rec.Header().Get("X-Hive-Backup-Encrypted"),
			"files":       rec.Header().Get("X-Hive-Backup-Files"),
			"bead_dirs":   rec.Header().Get("X-Hive-Backup-Bead-Dirs"),
			"archive":     "[not exposed through admin MCP]",
		}, nil
	}
	data, err := decodeAdminMCPJSON(rec.bodyBytes())
	if err != nil {
		return nil, err
	}
	return data, nil
}

type writeRecorder interface {
	http.ResponseWriter
	statusCode() int
	bodyBytes() []byte
	bytesWritten() int
}

type responseRecorderAdapter struct {
	*httptest.ResponseRecorder
}

func (r responseRecorderAdapter) statusCode() int {
	if r.Code == 0 {
		return http.StatusOK
	}
	return r.Code
}
func (r responseRecorderAdapter) bodyBytes() []byte { return r.Body.Bytes() }
func (r responseRecorderAdapter) bytesWritten() int { return r.Body.Len() }

type discardingWriteRecorder struct {
	header http.Header
	code   int
	bytes  int
	body   bytes.Buffer
}

func newDiscardingWriteRecorder() *discardingWriteRecorder {
	return &discardingWriteRecorder{header: http.Header{}}
}

func (r *discardingWriteRecorder) Header() http.Header { return r.header }
func (r *discardingWriteRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *discardingWriteRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	r.bytes += len(p)
	if r.code < http.StatusOK || r.code >= http.StatusMultipleChoices {
		_, _ = r.body.Write(p)
	}
	return len(p), nil
}
func (r *discardingWriteRecorder) statusCode() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}
func (r *discardingWriteRecorder) bodyBytes() []byte { return r.body.Bytes() }
func (r *discardingWriteRecorder) bytesWritten() int { return r.bytes }

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

func adminMCPStringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}
