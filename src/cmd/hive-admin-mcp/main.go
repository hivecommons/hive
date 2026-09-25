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
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultTimeout  = 15 * time.Second
	envHives        = "HIVE_ADMIN_MCP_HIVES"
	envActiveHive   = "HIVE_ADMIN_MCP_ACTIVE"
	envEnableWrites = "HIVE_ADMIN_MCP_ENABLE_WRITES"
	envPendingFile  = "HIVE_ADMIN_MCP_PENDING_FILE"
)

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
	server := mcp.NewServer(&mcp.Implementation{Name: adminmcp.ServerName, Version: adminmcp.ServerVersion}, nil)
	provider := readProvider{roster: r, writesEnabled: adminMCPWritesEnabled(), pendingStore: adminmcp.NewFilePendingStore(adminMCPPendingPath())}
	for _, def := range adminmcp.ToolsWithWritesEnabled(provider.writesEnabled) {
		name, _ := def["name"].(string)
		desc, _ := def["description"].(string)
		tool := &mcp.Tool{Name: name, Description: desc, InputSchema: def["inputSchema"]}
		server.AddTool(tool, provider.handler(name))
	}
	server.AddTool(&mcp.Tool{Name: "select_hive", Description: "Select the single active hive after verifying reachability and dashboard-token authentication.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}, "additionalProperties": false}}, provider.selectHive)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("hive-admin-mcp server failed: %v", err)
		os.Exit(1)
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
				return textResult(adminmcp.DataEnvelope{Data: adminmcp.Scrub(refusal)}, true)
			}
			if err != nil {
				return textResult(map[string]any{"error": err.Error()}, true)
			}
			return textResult(adminmcp.DataEnvelope{Data: adminmcp.Scrub(data)}, false)
		}
		data, err := p.Read(ctx, name, args)
		if err != nil {
			return textResult(map[string]any{"error": err.Error()}, true)
		}
		return textResult(adminmcp.DataEnvelope{Data: adminmcp.Scrub(data)}, false)
	}
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
	path, ok := readPath(tool, args)
	if !ok {
		return nil, fmt.Errorf("unsupported admin MCP read tool %q", tool)
	}
	client, _, err := p.roster.activeClient()
	if err != nil {
		return nil, err
	}
	data, err := client.Do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return adminmcp.CapResult(data, adminmcp.LimitFromArgs(args)), nil
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
		return textResult(map[string]any{"selected": false, "diagnosis": err.Error()}, true)
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
	client, err := hivectl.NewClient(h.Address, h.Token, r.timeout)
	return client, h, err
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
	r.mu.Unlock()
	if idx < 0 {
		return hiveConfig{}, fmt.Errorf("unknown hive %q", name)
	}
	client, err := hivectl.NewClient(h.Address, h.Token, r.timeout)
	if err != nil {
		return hiveConfig{}, err
	}
	if _, err := client.Do(ctx, http.MethodGet, "/api/status/summary", nil, nil); err != nil {
		return hiveConfig{}, diagnoseSelectionError(err)
	}
	r.mu.Lock()
	r.active = idx
	r.mu.Unlock()
	return h, nil
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
	return adminmcp.ReadPath(tool, adminmcp.LimitFromArgs(args))
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
