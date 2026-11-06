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
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

type dashboardAdminMCPProvider struct {
	server        *Server
	user          string
	role          string
	authorization string
	// ownerVerified is true only when this hive's own authentication marked
	// the outer request as a verified owner (dashboard token or verified
	// owner session); see adminMCPOwnerVerified.
	ownerVerified bool
	userAgent     string
}

// stdioBuildMaxLen caps the recorded build string so a hostile User-Agent
// cannot park a header-sized blob in /api/version (#12318).
const stdioBuildMaxLen = 64

// stdioBuildPattern is the shape hive-admin-mcp sends: <version>+<short sha>,
// where <short> is a 7-40 hex short SHA or the literal "unknown" (see
// cmd/hive-admin-mcp serverVersion).
var stdioBuildPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*\+(?:[0-9a-fA-F]{7,40}|unknown)$`)

// parseStdioBuildFromUserAgent extracts the stdio client build from an incoming
// User-Agent header (#12318). Only `hive-admin-mcp/<version>+<short>` is
// recognised; anything else (including hivectl's default `hivectl/0.1`, which
// the regular CLI also sends to /api/status and /api/version) returns "" so it
// never overwrites a genuine sighting. The build must match stdioBuildPattern
// and fit in stdioBuildMaxLen bytes, or it is dropped.
func parseStdioBuildFromUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if !strings.HasPrefix(ua, "hive-admin-mcp/") {
		return ""
	}
	build := strings.TrimPrefix(ua, "hive-admin-mcp/")
	if i := strings.IndexAny(build, " \t"); i >= 0 {
		build = build[:i]
	}
	if build == "" || len(build) > stdioBuildMaxLen || !stdioBuildPattern.MatchString(build) {
		return ""
	}
	return build
}

func (s *Server) handleAdminMCP(w http.ResponseWriter, r *http.Request) {
	provider := dashboardAdminMCPProvider{
		server:        s,
		user:          r.Header.Get("X-Hive-User"),
		role:          r.Header.Get("X-Hive-Role"),
		authorization: r.Header.Get("Authorization"),
		ownerVerified: adminMCPOwnerVerified(r),
		userAgent:     r.Header.Get("User-Agent"),
	}
	adminmcp.NewHandler(provider,
		adminmcp.WithWritesEnabled(adminMCPWritesEnabled()),
		adminmcp.WithWritesUnavailable(s.adminMCPWritesUnavailableReason(provider)),
		adminmcp.WithActor(provider.user),
		adminmcp.WithPendingStore(adminmcp.NewFilePendingStore(adminMCPPendingPath())),
		adminmcp.WithHiveID("dashboard"),
	).ServeHTTP(w, r)
}

// adminMCPWritesUnavailableReason reports why a confirmed write could not
// succeed for this caller. A confirmed write runs as the identity this hive
// verified on the request (adminMCPWriteRequest), so it needs one, with a role
// that may change things; a read-only caller is told so up front instead of
// previewing a write every endpoint would refuse.
func (s *Server) adminMCPWritesUnavailableReason(p dashboardAdminMCPProvider) string {
	if strings.TrimSpace(p.user) == "" || strings.TrimSpace(p.role) == "" {
		if s.authToken == "" && !s.directRouteAuthzEnabled() {
			return ""
		}
		return "this request carries no identity the hive verified, so a confirmed write would have no one to run as"
	}
	if !config.RoleAtLeast(p.role, config.RoleReadWrite) {
		return "your role on this hive (" + p.role + ") is read-only, so changes are not allowed"
	}
	return ""
}

func (p dashboardAdminMCPProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if p.server == nil {
		return nil, fmt.Errorf("%w: dashboard server unavailable", adminmcp.ErrForbidden)
	}
	if p.userAgent != "" {
		if build := parseStdioBuildFromUserAgent(p.userAgent); build != "" {
			p.server.RecordStdioBuild(build, time.Now().UTC())
		}
	}
	if tool == adminmcp.ToolReviewQueue {
		return adminmcp.ReadReviewQueue(func(path string) (any, error) { return p.get(tool, path) }, args)
	}
	if adminmcp.LogTool(tool) {
		// Log reads bound themselves (lines, bytes, scan work) and page with
		// next_cursor, so CapResult and the shared limit do not apply.
		path, err := adminmcp.LogReadPath(tool, args)
		if err != nil {
			return nil, err
		}
		return p.get(tool, path)
	}
	if adminmcp.DiagTool(tool) {
		return p.readDiag(tool, args)
	}
	if tool == adminmcp.ToolStageTrackerRead {
		data, err := p.get(tool, adminmcp.StageTrackerReadPath())
		if err != nil {
			return nil, err
		}
		return adminmcp.StageTrackerReadResult(data, args)
	}
	if tool == adminmcp.ToolDelegatedDecisionStatus {
		path, err := adminmcp.DelegatedDecisionStatusPath(args)
		if err != nil {
			return nil, err
		}
		return p.get(tool, path)
	}
	if tool == adminmcp.ToolSettingsRead {
		// An unknown section or a missing agent is refused with its reason.
		path, err := adminmcp.SettingsReadPath(args)
		if err != nil {
			return nil, err
		}
		// Most Settings endpoints answer a verified owner only, as they do
		// for the Settings page. The marker is copied onto the inner request
		// only when this hive verified the outer one (as readDiag does), so a
		// role claimed in a header still reads only what that role may.
		data, err := p.getVerified(tool, path, p.ownerVerified)
		if err != nil {
			return nil, err
		}
		// A Settings view is not a list to page: settings_read has no limit
		// argument, so the row cap would cut a hive's 21st agent out of its
		// governor settings with no way to ask for it. The text cap still
		// bounds the answer.
		return data, nil
	}
	path, ok := adminMCPReadPath(tool, args)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported admin MCP read tool", adminmcp.ErrForbidden)
	}
	data, err := p.get(tool, path)
	if err != nil {
		return nil, err
	}
	if tool == adminmcp.ToolIssuesByBand || tool == adminmcp.ToolPrsByBand {
		return adminmcp.BandReadResult(tool, data, args)
	}
	if tool == adminmcp.ToolGovernorSetup {
		return adminmcp.GovernorSetupProposal(data)
	}
	if tool == adminmcp.ToolVersionRead {
		if m, ok := data.(map[string]any); ok {
			if build, at := p.server.LastSeenStdio(); build != "" {
				m["last_seen_stdio_build"] = build
				if !at.IsZero() {
					m["last_seen_stdio_at"] = at.Format(time.RFC3339)
				}
			}
			if _, ok := m["hub_build"]; !ok {
				m["hub_build"] = versionShort
			}
		}
	}
	return adminmcp.CapResult(data, adminmcp.LimitFromArgs(args)), nil
}

// get serves one admin MCP read through the dashboard mux as the caller and
// decodes the JSON answer.
func (p dashboardAdminMCPProvider) get(tool, path string) (any, error) {
	return p.getVerified(tool, path, false)
}

// getVerified is get with the verified-owner marker set on the inner request
// when ownerVerified is true; callers pass p.ownerVerified, never a constant.
func (p dashboardAdminMCPProvider) getVerified(tool, path string, ownerVerified bool) (any, error) {
	if err := adminmcp.CheckRequestPath(path); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if p.user != "" {
		req.Header.Set("X-Hive-User", p.user)
	}
	if p.role != "" {
		req.Header.Set("X-Hive-Role", p.role)
	}
	if ownerVerified && isOwnerRole(p.role) {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	p.server.mux.ServeHTTP(rec, req)
	if rec.Code < http.StatusOK || rec.Code >= http.StatusMultipleChoices {
		// Same wording as the standalone client's APIError so both transports
		// give the caller the hive's reason; toolErr scrubs the text outbound.
		if reason := adminMCPRefusalReason(rec.Body.Bytes()); reason != "" {
			return nil, fmt.Errorf("Hive API returned HTTP %d: %s", rec.Code, reason)
		}
		return nil, fmt.Errorf("Hive API returned HTTP %d", rec.Code)
	}
	return decodeAdminMCPJSON(rec.Body.Bytes())
}

func adminMCPRefusalReason(body []byte) string {
	reason := strings.TrimSpace(string(body))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		reason = payload.Error
	}
	return reason
}

func adminMCPReadPath(tool string, args map[string]any) (string, bool) {
	if tool == adminmcp.ToolSettingsRead {
		path, err := adminmcp.SettingsReadPath(args)
		return path, err == nil
	}
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
	p.adminMCPWriteRequest(httpReq).ServeHTTP(rec, httpReq)
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

// adminMCPWriteRequest prepares a confirmed write to run as the caller and
// returns the handler chain to serve it. With no verified identity on the
// outer request (an open spoke, or a hand-built provider in a test) the write
// re-enters authenticate with the caller's Authorization header, as before.
// Otherwise it runs as the identity authenticate verified on the outer
// /api/admin/mcp request — the same user, role and verified-owner marker the
// read tools already carry — so a hub-proxied or session caller can write,
// every role check sees that caller, and the write is attributed to them
// rather than to the configured owner. authenticate is not re-entered:
// it strips identity headers by design, and the identity here is the one it
// already verified, never one the client asserted (inbound copies were
// stripped before the outer request reached this handler).
func (p dashboardAdminMCPProvider) adminMCPWriteRequest(req *http.Request) http.Handler {
	inner := p.server.roleEnforcement(p.server.securityHeaders(p.server.mux))
	if strings.TrimSpace(p.user) == "" || strings.TrimSpace(p.role) == "" {
		if strings.TrimSpace(p.authorization) != "" {
			req.Header.Set("Authorization", p.authorization)
		}
		return p.server.authenticate(inner)
	}
	req.Header.Set("X-Hive-User", p.user)
	req.Header.Set("X-Hive-Role", p.role)
	if p.ownerVerified && isOwnerRole(p.role) {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	return inner
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

// adminMCPEndpointPath and adminMCPWritesEnvName are surfaced read-only on
// the Settings → Channels tab; the admin MCP server has no hive.yaml section.
const (
	adminMCPEndpointPath  = "/api/admin/mcp"
	adminMCPWritesEnvName = "HIVE_ADMIN_MCP_ENABLE_WRITES"
)

func adminMCPWritesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(adminMCPWritesEnvName))) {
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
