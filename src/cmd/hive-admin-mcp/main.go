package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultTimeout  = 15 * time.Second
	skewTimeout     = 3 * time.Second
	envHives        = "HIVE_ADMIN_MCP_HIVES"
	envActiveHive   = "HIVE_ADMIN_MCP_ACTIVE"
	envEnableWrites = "HIVE_ADMIN_MCP_ENABLE_WRITES"
	envPendingFile  = "HIVE_ADMIN_MCP_PENDING_FILE"
)

// Stamped by -ldflags in the image build, mirroring cmd/hive.
var (
	gitHash   = "unknown"
	gitShort  = "unknown"
	gitBranch = "unknown"
	version   = ""
)

type stdioBuild struct{ Version, SHA, Short, Branch string }

// buildInfo reports this binary's build: ldflag values when stamped, else the
// vcs.revision Go embeds for a checkout build, else "unknown".
func buildInfo() stdioBuild {
	b := stdioBuild{Version: version, SHA: gitHash, Short: gitShort, Branch: gitBranch}
	if b.SHA == "" || b.SHA == "unknown" {
		b.SHA = "unknown"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, kv := range info.Settings {
				if kv.Key == "vcs.revision" && kv.Value != "" {
					b.SHA = kv.Value
				}
			}
		}
	}
	if b.Short == "" || b.Short == "unknown" {
		b.Short = "unknown"
		if b.SHA != "unknown" {
			b.Short = b.SHA[:min(7, len(b.SHA))]
		}
	}
	if b.Branch == "" {
		b.Branch = "unknown"
	}
	return b
}

// buildSkew is true only when both builds are known and their short SHAs
// differ; an unknown side is never reported as skew.
func buildSkew(stdioShort, hubShort string) bool {
	a, b := strings.TrimSpace(stdioShort), strings.TrimSpace(hubShort)
	if a == "" || b == "" || a == "unknown" || b == "unknown" {
		return false
	}
	n := min(len(a), len(b), 7)
	return !strings.EqualFold(a[:n], b[:n])
}

// serverVersion is serverInfo.version: the catalogue version plus this build's
// short SHA. The User-Agent carries the same value so the hub can tell which
// stdio build is calling it.
func serverVersion() string {
	return adminmcp.ServerVersion + "+" + buildInfo().Short
}

func userAgent() string {
	return "hive-admin-mcp/" + serverVersion()
}

// hubShort reads the hub's short SHA from an /api/version body.
func hubShort(m map[string]any) string {
	short, _ := m["short"].(string)
	if short == "" {
		short, _ = m["currentCommit"].(string)
	}
	return short
}

// skewNotice is the one-line hint carried by initialize instructions and by
// errors the binary raises itself; empty unless buildSkew reports skew.
func skewNotice(stdioShort, hubShort, hive string) string {
	if !buildSkew(stdioShort, hubShort) {
		return ""
	}
	return fmt.Sprintf("hive-admin-mcp build %s differs from hub build %s on hive %q, so tools added or changed since may be missing or fail; update the binary (extract it from the hub image or go build ./cmd/hive-admin-mcp, see \"Getting the binary\" in src/docs/admin-mcp.md)", strings.TrimSpace(stdioShort), strings.TrimSpace(hubShort), hive)
}

type hiveConfig struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Token   string `json:"token"`
}

type roster struct {
	mu      sync.Mutex
	hives   []hiveConfig
	active  int
	timeout time.Duration
	// clients caches one hivectl.Client per roster entry: each client owns its
	// own transport, so building one per call would discard the connection pool.
	clients map[string]*hivectl.Client
	// notice is the skew notice for the active hive, refreshed at startup and
	// on select_hive; empty when builds match or either SHA is unknown.
	notice string
}

type readProvider struct {
	roster        *roster
	writesEnabled bool
	pendingStore  adminmcp.PendingStore
}

func main() {
	r, err := loadRosterFromEnv()
	if err != nil {
		log.Printf("hive-admin-mcp configuration error: %v", err)
		os.Exit(2)
	}
	r.refreshSkew(context.Background())
	server := newServer(readProvider{roster: r, writesEnabled: adminMCPWritesEnabled(), pendingStore: adminmcp.NewFilePendingStore(adminMCPPendingPath())})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("hive-admin-mcp server failed: %v", err)
		os.Exit(1)
	}
}

// newServer builds the stdio MCP server. Instructions are fixed at initialize,
// so a skew notice known at startup goes there; skew found later reaches the
// agent only through the error hints.
func newServer(provider readProvider) *mcp.Server {
	var opts *mcp.ServerOptions
	if notice := provider.roster.skewNotice(); notice != "" {
		opts = &mcp.ServerOptions{Instructions: notice}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: adminmcp.ServerName, Version: serverVersion()}, opts)
	server.AddReceivingMiddleware(provider.roster.unknownToolMiddleware)
	for _, def := range adminmcp.ToolsWithWritesEnabled(provider.writesEnabled) {
		name, _ := def["name"].(string)
		desc, _ := def["description"].(string)
		tool := &mcp.Tool{Name: name, Description: desc, InputSchema: def["inputSchema"]}
		server.AddTool(tool, provider.handler(name))
	}
	server.AddTool(&mcp.Tool{Name: "select_hive", Description: "Select the single active hive after verifying reachability and dashboard-token authentication.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}, "additionalProperties": false}}, provider.selectHive)
	return server
}

// unknownToolMiddleware appends the skew notice to the SDK's "unknown tool"
// error, which a stale binary returns for every tool added since it was built.
func (r *roster) unknownToolMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		var wire *jsonrpc.Error
		if method == "tools/call" && errors.As(err, &wire) && strings.HasPrefix(wire.Message, "unknown tool ") {
			if notice := r.skewNotice(); notice != "" {
				annotated := *wire
				annotated.Message += "; " + notice
				return res, &annotated
			}
		}
		return res, err
	}
}

func loadRosterFromEnv() (*roster, error) {
	raw := strings.TrimSpace(os.Getenv(envHives))
	if raw == "" {
		return nil, fmt.Errorf("%s must contain a JSON array of {name,address,token}", envHives)
	}
	var hives []hiveConfig
	if err := json.Unmarshal([]byte(raw), &hives); err != nil {
		return nil, fmt.Errorf("parse %s: %w", envHives, err)
	}
	if len(hives) == 0 {
		return nil, fmt.Errorf("%s must contain at least one hive", envHives)
	}
	activeName := strings.TrimSpace(os.Getenv(envActiveHive))
	active := 0
	seen := map[string]bool{}
	for i, h := range hives {
		if strings.TrimSpace(h.Name) == "" || strings.TrimSpace(h.Address) == "" || strings.TrimSpace(h.Token) == "" {
			return nil, fmt.Errorf("hive roster entries require name, address, and token")
		}
		if seen[h.Name] {
			return nil, fmt.Errorf("duplicate hive name %q", h.Name)
		}
		seen[h.Name] = true
		if activeName != "" && h.Name == activeName {
			active = i
		}
	}
	if activeName != "" && hives[active].Name != activeName {
		return nil, fmt.Errorf("active hive %q is not in the roster", activeName)
	}
	return &roster{hives: hives, active: active, timeout: defaultTimeout}, nil
}

func (p readProvider) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
		}
		if name == adminmcp.ToolWritePreview || name == adminmcp.ToolWriteConfirm {
			data, err := p.callWrite(ctx, name, args)
			if refusal, ok := adminmcp.HiveRefusalFromError(err); ok {
				return dataResult(refusal, true)
			}
			if err != nil {
				return textResult(map[string]any{"error": adminmcp.Scrub(err.Error())}, true)
			}
			return dataResult(data, false)
		}
		data, err := p.Read(ctx, name, args)
		if err != nil {
			return textResult(map[string]any{"error": adminmcp.Scrub(p.roster.annotateSkew(err).Error())}, true)
		}
		return dataResult(data, false)
	}
}

// unsupportedToolError is raised by the binary itself for a tool its compiled
// catalogue cannot route.
type unsupportedToolError struct{ tool string }

func (e *unsupportedToolError) Error() string {
	return fmt.Sprintf("unsupported admin MCP read tool %q", e.tool)
}

// annotateSkew appends the skew notice to read errors a build mismatch can
// explain: tools this binary cannot route, and hub 404s. Other errors, and all
// errors without skew, are returned unchanged.
func (r *roster) annotateSkew(err error) error {
	notice := r.skewNotice()
	if notice == "" {
		return err
	}
	var unsupported *unsupportedToolError
	var api *hivectl.APIError
	if errors.As(err, &unsupported) || (errors.As(err, &api) && api.StatusCode == http.StatusNotFound) {
		return fmt.Errorf("%w; %s", err, notice)
	}
	return err
}

func (p readProvider) callWrite(ctx context.Context, tool string, args map[string]any) (any, error) {
	_, hive, err := p.roster.activeClient()
	if err != nil {
		return nil, err
	}
	store := p.pendingStore
	if store == nil {
		store = adminmcp.NewMemoryPendingStore()
	}
	h := adminmcp.NewHandler(p, adminmcp.WithWritesEnabled(p.writesEnabled), adminmcp.WithPendingStore(store), adminmcp.WithHiveID(hive.Name))
	if tool == adminmcp.ToolWritePreview {
		return h.PreviewWrite(ctx, args)
	}
	return h.ConfirmWrite(ctx, args)
}

func (p readProvider) ExecuteWrite(ctx context.Context, req adminmcp.WriteRequest) (any, error) {
	client, _, err := p.roster.activeClient()
	if err != nil {
		return nil, err
	}
	if req.Path == "/api/backup" {
		meta, err := client.DoDiscard(ctx, req.Method, req.Path, nil, req.Body)
		if err != nil {
			var api *hivectl.APIError
			if errors.As(err, &api) {
				return nil, &adminmcp.HiveRefusalError{StatusCode: api.StatusCode, Message: api.Message, Body: api.Body}
			}
			return nil, err
		}
		return map[string]any{
			"ok":          true,
			"contentType": meta.ContentType,
			"bytes":       meta.ContentLength,
			"encrypted":   meta.Headers.Get("X-Hive-Backup-Encrypted"),
			"files":       meta.Headers.Get("X-Hive-Backup-Files"),
			"bead_dirs":   meta.Headers.Get("X-Hive-Backup-Bead-Dirs"),
			"archive":     "[not exposed through admin MCP]",
		}, nil
	}
	data, err := client.Do(ctx, req.Method, req.Path, nil, req.Body)
	if err != nil {
		var api *hivectl.APIError
		if errors.As(err, &api) {
			return nil, &adminmcp.HiveRefusalError{StatusCode: api.StatusCode, Message: api.Message, Body: api.Body}
		}
		return nil, err
	}
	return data, nil
}

func (p readProvider) Read(ctx context.Context, tool string, args map[string]any) (any, error) {
	if tool == adminmcp.ToolExclusionCatalogue {
		return adminmcp.Catalogue(p.writesEnabled, adminmcp.DefaultWriteRegistry()), nil
	}
	if tool == adminmcp.ToolRefuseOperation {
		operation, _ := args["operation"].(string)
		refusal, _ := adminmcp.RefusalFor(operation)
		return refusal, nil
	}
	if tool == adminmcp.ToolReviewQueue {
		client, _, err := p.roster.activeClient()
		if err != nil {
			return nil, err
		}
		return adminmcp.ReadReviewQueue(func(path string) (any, error) { return getPath(ctx, client, path) }, args)
	}
	if adminmcp.LogTool(tool) {
		// Log reads bound themselves (lines, bytes, scan work) and page with
		// next_cursor, so CapResult and the shared limit do not apply.
		path, err := adminmcp.LogReadPath(tool, args)
		if err != nil {
			return nil, err
		}
		client, _, err := p.roster.activeClient()
		if err != nil {
			return nil, err
		}
		return getPath(ctx, client, path)
	}
	if adminmcp.DiagTool(tool) {
		return p.readDiag(ctx, tool, args)
	}
	path, ok := readPath(tool, args)
	if !ok {
		return nil, &unsupportedToolError{tool: tool}
	}
	client, _, err := p.roster.activeClient()
	if err != nil {
		return nil, err
	}
	data, err := getPath(ctx, client, path)
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
			b := buildInfo()
			m["stdio_build"] = map[string]any{"version": b.Version, "sha": b.SHA, "short": b.Short, "branch": b.Branch}
			m["skew"] = buildSkew(b.Short, hubShort(m))
		}
	}
	return adminmcp.CapResult(data, adminmcp.LimitFromArgs(args)), nil
}

// getPath GETs a read path built by readPath or adminmcp. Those paths carry
// their query as a "?..." suffix; hivectl.Client takes the query separately
// and would percent-encode a "?" left in the path (#9160).
func getPath(ctx context.Context, client *hivectl.Client, path string) (any, error) {
	apiPath, rawQuery, _ := strings.Cut(path, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, err
	}
	return client.Do(ctx, http.MethodGet, apiPath, query, nil)
}

func (p readProvider) selectHive(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Name string `json:"name"`
	}
	if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
	}
	selected, err := p.roster.selectHive(ctx, args.Name)
	if err != nil {
		return textResult(map[string]any{"selected": false, "diagnosis": adminmcp.Scrub(err.Error())}, true)
	}
	return textResult(adminmcp.DataEnvelope{Data: map[string]any{"selected": true, "active_hive": selected.Name, "address": selected.Address}}, false)
}

func (r *roster) activeClient() (*hivectl.Client, hiveConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hives) == 0 || r.active < 0 || r.active >= len(r.hives) {
		return nil, hiveConfig{}, errors.New("no active hive selected")
	}
	h := r.hives[r.active]
	client, err := r.clientLocked(h)
	return client, h, err
}

// clientLocked returns the cached client for h, creating it on first use.
// The caller must hold r.mu.
func (r *roster) clientLocked(h hiveConfig) (*hivectl.Client, error) {
	if client, ok := r.clients[h.Name]; ok {
		return client, nil
	}
	client, err := hivectl.NewClient(h.Address, h.Token, r.timeout)
	if err != nil {
		return nil, err
	}
	client.SetUserAgent(userAgent())
	if r.clients == nil {
		r.clients = map[string]*hivectl.Client{}
	}
	r.clients[h.Name] = client
	return client, nil
}

func (r *roster) selectHive(ctx context.Context, name string) (hiveConfig, error) {
	r.mu.Lock()
	idx := -1
	var h hiveConfig
	for i, candidate := range r.hives {
		if candidate.Name == strings.TrimSpace(name) {
			idx, h = i, candidate
			break
		}
	}
	if idx < 0 {
		r.mu.Unlock()
		return hiveConfig{}, fmt.Errorf("unknown hive %q", name)
	}
	client, err := r.clientLocked(h)
	r.mu.Unlock()
	if err != nil {
		return hiveConfig{}, err
	}
	if _, err := client.Do(ctx, http.MethodGet, "/api/status/summary", nil, nil); err != nil {
		return hiveConfig{}, diagnoseSelectionError(err)
	}
	r.mu.Lock()
	r.active = idx
	r.mu.Unlock()
	r.refreshSkewFor(ctx, client, h.Name)
	return h, nil
}

func (r *roster) skewNotice() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notice
}

// refreshSkew recomputes the skew notice for the active hive. It never fails
// startup: an unreachable hub or unreadable version leaves no notice.
func (r *roster) refreshSkew(ctx context.Context) {
	client, h, err := r.activeClient()
	if err != nil {
		r.setNotice("")
		return
	}
	r.refreshSkewFor(ctx, client, h.Name)
}

func (r *roster) refreshSkewFor(ctx context.Context, client *hivectl.Client, hive string) {
	ctx, cancel := context.WithTimeout(ctx, skewTimeout)
	defer cancel()
	notice := ""
	if data, err := client.Do(ctx, http.MethodGet, "/api/version", nil, nil); err == nil {
		if m, ok := data.(map[string]any); ok {
			notice = skewNotice(buildInfo().Short, hubShort(m), hive)
		}
	}
	r.setNotice(notice)
}

func (r *roster) setNotice(notice string) {
	r.mu.Lock()
	r.notice = notice
	r.mu.Unlock()
}

func diagnoseSelectionError(err error) error {
	var conn *hivectl.ConnectionError
	if errors.As(err, &conn) {
		return fmt.Errorf("unreachable host: %w", err)
	}
	var api *hivectl.APIError
	if errors.As(err, &api) && api.StatusCode == http.StatusUnauthorized {
		msg := strings.ToLower(api.Message + " " + string(api.Body))
		if strings.Contains(msg, "direct-route") || strings.Contains(msg, "per-user") || strings.Contains(msg, "shared token") {
			return fmt.Errorf("direct-route spoke refused shared dashboard-token authentication; use a per-user session route instead")
		}
		return fmt.Errorf("wrong dashboard token for selected hive")
	}
	return err
}

func readPath(tool string, args map[string]any) (string, bool) {
	if tool == adminmcp.ToolAgentNudgeStatus {
		agent := strings.TrimSpace(stringArg(args, "agent"))
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

// dataResult encodes a tool result through adminmcp.EncodeResult so stdio
// applies the same scrubbing, envelope and text cap as the dashboard endpoint.
func dataResult(data any, isError bool) (*mcp.CallToolResult, error) {
	b, err := adminmcp.EncodeResult(data)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: isError}, nil
}

func textResult(v any, isError bool) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: isError}, nil
}

func adminMCPWritesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envEnableWrites))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func adminMCPPendingPath() string {
	if path := strings.TrimSpace(os.Getenv(envPendingFile)); path != "" {
		return path
	}
	if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, "hive", "admin-mcp-pending-confirmations.json")
	}
	return filepath.Join(".", ".hive-admin-mcp-pending-confirmations.json")
}

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}
