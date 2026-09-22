// Package taskmcp serves the read-only, task-scoped MCP surface used by
// contributor agents.
package taskmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	EndpointPath = "/api/contribute/mcp"

	ProtocolVersion = "2025-03-26"
	ServerName      = "hive-task-mcp"
	ServerVersion   = "0.1.0"

	ToolTaskContext   = "task_context"
	ToolRelatedWork   = "related_work"
	ToolCIHealth      = "ci_health"
	ToolContextBundle = "context_bundle"

	DefaultPageSize = 20
	MaxPageSize     = 20
	MaxTextBytes    = 16 * 1024

	HeaderTaskID = "X-Hive-Task-ID"
)

var ErrForbidden = errors.New("task scope forbidden")

type Scope struct {
	TaskID string `json:"task_id,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
}

type PageRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

func (p PageRequest) Normalized() PageRequest {
	if p.Limit <= 0 || p.Limit > MaxPageSize {
		p.Limit = DefaultPageSize
	}
	return p
}

type PageInfo struct {
	NextCursor string `json:"next_cursor,omitempty"`
	Limit      int    `json:"limit"`
	More       bool   `json:"more"`
}

type DataEnvelope struct {
	Data any       `json:"data"`
	Page *PageInfo `json:"page,omitempty"`
}

type BundleData struct {
	TaskContext TaskContextData `json:"task_context"`
	RelatedWork RelatedWorkData `json:"related_work"`
	CIHealth    CIHealthData    `json:"ci_health"`
}

type TaskContextData struct {
	Assignment AssignmentData `json:"assignment"`
	Data       ServedText     `json:"data"`
	Labels     []string       `json:"labels,omitempty"`
	Claim      *ClaimData     `json:"claim,omitempty"`
	Lease      LeaseData      `json:"lease"`
	Policies   PolicyData     `json:"policies"`
}

type ServedText struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type AssignmentData struct {
	TaskID     string `json:"task_id"`
	Kind       string `json:"kind,omitempty"`
	Role       string `json:"role,omitempty"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	Key        string `json:"key,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`
	Complexity string `json:"complexity,omitempty"`
}

type ClaimData struct {
	PRNumber int    `json:"pr_number,omitempty"`
	PRRepo   string `json:"pr_repo,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	Author   string `json:"author,omitempty"`
	Decision string `json:"decision,omitempty"`
}

type LeaseData struct {
	Generation uint64 `json:"generation,omitempty"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
}

type PolicyData struct {
	Hold             HoldPolicyData      `json:"hold"`
	LevelGate        LevelGatePolicyData `json:"level_gate"`
	Standby          []StandbyPolicyData `json:"standby,omitempty"`
	PRTemplate       PRTemplatePolicy    `json:"pr_template"`
	CacheOnly        bool                `json:"cache_only"`
	HubLaunchedOnly  bool                `json:"hub_launched_only"`
	StreamableHTTP   bool                `json:"streamable_http"`
	RoutePrefix      string              `json:"route_prefix"`
	ServedTextSchema string              `json:"served_text_schema"`
	SecurityWithheld bool                `json:"security_withheld,omitempty"`
	RepoScoped       bool                `json:"repo_scoped"`
}

type HoldPolicyData struct {
	Held   bool   `json:"held"`
	Reason string `json:"reason,omitempty"`
}

type LevelGatePolicyData struct {
	ACMMLevel  int    `json:"acmm_level,omitempty"`
	Complexity string `json:"complexity,omitempty"`
}

type StandbyPolicyData struct {
	Lane        string `json:"lane"`
	Enabled     bool   `json:"enabled"`
	Floor       string `json:"floor,omitempty"`
	DailyCap    int    `json:"daily_cap,omitempty"`
	Qualifies   int    `json:"qualifies,omitempty"`
	ItemTier    string `json:"item_tier,omitempty"`
	ItemMatched bool   `json:"item_matched,omitempty"`
}

type PRTemplatePolicy struct {
	Required bool `json:"required"`
}

type RelatedWorkData struct {
	Items []RelatedItem `json:"items"`
}

type RelatedItem struct {
	Kind      string     `json:"kind"`
	Repo      string     `json:"repo"`
	Number    int        `json:"number"`
	State     string     `json:"state,omitempty"`
	Author    string     `json:"author,omitempty"`
	URL       string     `json:"url,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
	Reasons   []string   `json:"reasons,omitempty"`
	Files     []string   `json:"files,omitempty"`
	Data      ServedText `json:"data"`
}

type CIHealthData struct {
	Checks       []CheckHealth `json:"checks"`
	CacheStale   bool          `json:"cache_stale,omitempty"`
	CacheAgeSecs int64         `json:"cache_age_seconds,omitempty"`
}

type CheckHealth struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	DefaultBranch   string `json:"default_branch,omitempty"`
	FailingPRs      int    `json:"failing_prs"`
	LastGreenSHA    string `json:"last_green_sha,omitempty"`
	PendingApproval bool   `json:"pending_approval,omitempty"`
}

type Provider interface {
	Scope(*http.Request, map[string]any) (Scope, error)
	TaskContext(context.Context, Scope) (TaskContextData, error)
	RelatedWork(context.Context, Scope, PageRequest) (RelatedWorkData, PageInfo, error)
	CIHealth(context.Context, Scope, PageRequest) (CIHealthData, PageInfo, error)
}

type Handler struct {
	Provider Provider
}

func NewHandler(provider Provider) *Handler { return &Handler{Provider: provider} }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxTextBytes)).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32600, Message: "invalid request"}})
		return
	}
	result, rpcErr := h.dispatch(r, req)
	writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
}

func (h *Handler) dispatch(r *http.Request, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{"protocolVersion": ProtocolVersion, "serverInfo": map[string]string{"name": ServerName, "version": ServerVersion}, "capabilities": map[string]any{"tools": map[string]any{}}}, nil
	case "notifications/initialized":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		return h.callTool(r, req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (h *Handler) callTool(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	if h.Provider == nil {
		return nil, &rpcError{Code: -32000, Message: "provider unavailable"}
	}
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, &rpcError{Code: -32602, Message: "missing tool params"}
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: -32602, Message: "invalid tool params"}
	}
	scope, err := h.Provider.Scope(r, p.Arguments)
	if err != nil {
		code := -32000
		if errors.Is(err, ErrForbidden) {
			code = -32003
		}
		return nil, &rpcError{Code: code, Message: err.Error()}
	}
	page := pageFromArgs(p.Arguments).Normalized()
	var env DataEnvelope
	switch p.Name {
	case ToolTaskContext:
		data, err := h.Provider.TaskContext(r.Context(), scope)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data}
	case ToolRelatedWork:
		data, pageInfo, err := h.Provider.RelatedWork(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data, Page: &pageInfo}
	case ToolCIHealth:
		data, pageInfo, err := h.Provider.CIHealth(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data, Page: &pageInfo}
	case ToolContextBundle:
		tc, err := h.Provider.TaskContext(r.Context(), scope)
		if err != nil {
			return nil, toolErr(err)
		}
		related, relatedPage, err := h.Provider.RelatedWork(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		ci, ciPage, err := h.Provider.CIHealth(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: BundleData{TaskContext: tc, RelatedWork: related, CIHealth: ci}, Page: &PageInfo{Limit: page.Limit, More: relatedPage.More || ciPage.More, NextCursor: firstNonEmpty(relatedPage.NextCursor, ciPage.NextCursor)}}
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, toolErr(err)
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}, nil
}

func toolErr(err error) *rpcError {
	if errors.Is(err, ErrForbidden) {
		return &rpcError{Code: -32003, Message: err.Error()}
	}
	return &rpcError{Code: -32000, Message: err.Error()}
}

func pageFromArgs(args map[string]any) PageRequest {
	var p PageRequest
	if args == nil {
		return p
	}
	if v, ok := args["cursor"].(string); ok {
		p.Cursor = v
	}
	switch v := args["limit"].(type) {
	case float64:
		p.Limit = int(v)
	case int:
		p.Limit = v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			p.Limit = n
		}
	}
	return p
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func tools() []map[string]any {
	defs := []struct{ name, desc string }{
		{ToolTaskContext, "Return the scoped assignment, lease and policy context for the current task."},
		{ToolRelatedWork, "Return capped, paginated related work from Hive's cached task context."},
		{ToolCIHealth, "Return capped, paginated cached CI health for the scoped repository."},
		{ToolContextBundle, "Return task_context, related_work and ci_health in one capped response."},
	}
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{"name": d.name, "description": d.desc, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "maximum": MaxPageSize}, "cursor": map[string]any{"type": "string"}, "task_id": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "number": map[string]any{"type": "integer"}}}})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func PaginateRelated(items []RelatedItem, page PageRequest) (RelatedWorkData, PageInfo) {
	page = page.Normalized()
	start := cursorIndex(page.Cursor)
	if start > len(items) {
		start = len(items)
	}
	end := start + page.Limit
	more := end < len(items)
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if more {
		next = strconv.Itoa(end)
	}
	out := items[start:end]
	if out == nil {
		out = []RelatedItem{}
	}
	return RelatedWorkData{Items: out}, PageInfo{NextCursor: next, Limit: page.Limit, More: more}
}

func PaginateChecks(items []CheckHealth, page PageRequest) (CIHealthData, PageInfo) {
	page = page.Normalized()
	start := cursorIndex(page.Cursor)
	if start > len(items) {
		start = len(items)
	}
	end := start + page.Limit
	more := end < len(items)
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if more {
		next = strconv.Itoa(end)
	}
	out := items[start:end]
	if out == nil {
		out = []CheckHealth{}
	}
	return CIHealthData{Checks: out}, PageInfo{NextCursor: next, Limit: page.Limit, More: more}
}

func cursorIndex(cursor string) int {
	n, err := strconv.Atoi(strings.TrimSpace(cursor))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func SortRelated(items []RelatedItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Repo != items[j].Repo {
			return items[i].Repo < items[j].Repo
		}
		return items[i].Number < items[j].Number
	})
}

func RequireRepo(scope Scope, repo string) error {
	if strings.TrimSpace(scope.Repo) == "" || strings.TrimSpace(repo) == "" || !strings.EqualFold(scope.Repo, repo) {
		return fmt.Errorf("%w: repo outside task scope", ErrForbidden)
	}
	return nil
}
