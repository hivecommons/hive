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

	ToolTaskContext     = "task_context"
	ToolRelatedWork     = "related_work"
	ToolCIHealth        = "ci_health"
	ToolContextBundle   = "context_bundle"
	ToolRepoConventions = "repo_conventions"
	ToolDependencies    = "dependencies"
	ToolHistory         = "history"
	ToolKnowledge       = "knowledge"

	DefaultPageSize            = 20
	MaxPageSize                = 20
	MaxTextBytes               = 16 * 1024
	DefaultTaskMCPHistoryLimit = 20
	MaxTaskMCPDependencies     = 20
	MaxTaskMCPKnowledgeResults = 20
	MaxTaskMCPRepoConventions  = 20

	HeaderTaskID = "X-Hive-Task-ID"
)

var ErrForbidden = errors.New("task scope forbidden")

type RefusalData struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	LeaseID string `json:"lease_id,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Repo    string `json:"repo,omitempty"`
}

type RefusalError struct {
	Data RefusalData
	Err  error
}

func (e RefusalError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Data.Reason
}

func (e RefusalError) Unwrap() error { return e.Err }

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
	TaskContext     *TaskContextData     `json:"task_context,omitempty"`
	RelatedWork     *RelatedWorkData     `json:"related_work,omitempty"`
	CIHealth        *CIHealthData        `json:"ci_health,omitempty"`
	RepoConventions *RepoConventionsData `json:"repo_conventions,omitempty"`
	Dependencies    *DependenciesData    `json:"dependencies,omitempty"`
	History         *HistoryData         `json:"history,omitempty"`
	Knowledge       *KnowledgeData       `json:"knowledge,omitempty"`
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

type RepoConventionsData struct {
	Repo        string           `json:"repo"`
	Source      string           `json:"source"`
	Conventions []RepoConvention `json:"conventions"`
}

type RepoConvention struct {
	Slug   string     `json:"slug,omitempty"`
	Layer  string     `json:"layer,omitempty"`
	Source string     `json:"source"`
	Data   ServedText `json:"data"`
}

type DependenciesData struct {
	TaskID        string           `json:"task_id"`
	BlockedBy     []DependencyNode `json:"blocked_by"`
	Blocks        []DependencyNode `json:"blocks"`
	SuggestedPlan []DependencyNode `json:"suggested_plan,omitempty"`
}

type DependencyNode struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type HistoryData struct {
	Items []RelatedItem `json:"items"`
}

type KnowledgeData struct {
	Items []KnowledgeItem `json:"items"`
}

type KnowledgeItem struct {
	Repo       string     `json:"repo"`
	Slug       string     `json:"slug,omitempty"`
	Layer      string     `json:"layer,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	Tags       []string   `json:"tags,omitempty"`
	Data       ServedText `json:"data"`
}

type Provider interface {
	Scope(*http.Request, map[string]any) (Scope, error)
	TaskContext(context.Context, Scope) (TaskContextData, error)
	RelatedWork(context.Context, Scope, PageRequest) (RelatedWorkData, PageInfo, error)
	CIHealth(context.Context, Scope, PageRequest) (CIHealthData, PageInfo, error)
	RepoConventions(context.Context, Scope, PageRequest) (RepoConventionsData, PageInfo, error)
	Dependencies(context.Context, Scope, PageRequest) (DependenciesData, PageInfo, error)
	History(context.Context, Scope, PageRequest) (HistoryData, PageInfo, error)
	Knowledge(context.Context, Scope, string, PageRequest) (KnowledgeData, PageInfo, error)
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
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	p.Arguments["_tool"] = p.Name
	scope, err := h.Provider.Scope(r, p.Arguments)
	if err != nil {
		var refusal RefusalError
		if errors.As(err, &refusal) {
			data := refusal.Data
			if data.Type == "" {
				data.Type = "refusal"
			}
			if data.Reason == "" {
				data.Reason = err.Error()
			}
			b, marshalErr := json.Marshal(DataEnvelope{Data: data})
			if marshalErr != nil {
				return nil, toolErr(marshalErr)
			}
			return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}, nil
		}
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
	case ToolRepoConventions:
		data, pageInfo, err := h.Provider.RepoConventions(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data, Page: &pageInfo}
	case ToolDependencies:
		data, pageInfo, err := h.Provider.Dependencies(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data, Page: &pageInfo}
	case ToolHistory:
		data, pageInfo, err := h.Provider.History(r.Context(), scope, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data, Page: &pageInfo}
	case ToolKnowledge:
		data, pageInfo, err := h.Provider.Knowledge(r.Context(), scope, queryFromArgs(p.Arguments), page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: data, Page: &pageInfo}
	case ToolContextBundle:
		bundle, pageInfo, err := h.contextBundle(r, scope, p.Arguments, page)
		if err != nil {
			return nil, toolErr(err)
		}
		env = DataEnvelope{Data: bundle, Page: &pageInfo}
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, toolErr(err)
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}, nil
}

func (h *Handler) contextBundle(r *http.Request, scope Scope, args map[string]any, page PageRequest) (BundleData, PageInfo, error) {
	include := includeSet(args)
	var bundle BundleData
	var pages []PageInfo
	if include[ToolTaskContext] {
		tc, err := h.Provider.TaskContext(r.Context(), scope)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.TaskContext = &tc
	}
	if include[ToolRelatedWork] {
		related, relatedPage, err := h.Provider.RelatedWork(r.Context(), scope, page)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.RelatedWork = &related
		pages = append(pages, relatedPage)
	}
	if include[ToolCIHealth] {
		ci, ciPage, err := h.Provider.CIHealth(r.Context(), scope, page)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.CIHealth = &ci
		pages = append(pages, ciPage)
	}
	if include[ToolRepoConventions] {
		data, pi, err := h.Provider.RepoConventions(r.Context(), scope, page)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.RepoConventions = &data
		pages = append(pages, pi)
	}
	if include[ToolDependencies] {
		data, pi, err := h.Provider.Dependencies(r.Context(), scope, page)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.Dependencies = &data
		pages = append(pages, pi)
	}
	if include[ToolHistory] {
		data, pi, err := h.Provider.History(r.Context(), scope, page)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.History = &data
		pages = append(pages, pi)
	}
	if include[ToolKnowledge] {
		data, pi, err := h.Provider.Knowledge(r.Context(), scope, queryFromArgs(args), page)
		if err != nil {
			return BundleData{}, PageInfo{}, err
		}
		bundle.Knowledge = &data
		pages = append(pages, pi)
	}
	return bundle, combinePages(page.Limit, pages...), nil
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

func queryFromArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	q, _ := args["query"].(string)
	return strings.TrimSpace(q)
}

func includeSet(args map[string]any) map[string]bool {
	defaults := map[string]bool{ToolTaskContext: true, ToolRelatedWork: true, ToolCIHealth: true}
	if args == nil {
		return defaults
	}
	raw, ok := args["include"]
	if !ok || raw == nil {
		return defaults
	}
	out := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		switch v {
		case ToolTaskContext, ToolRelatedWork, ToolCIHealth, ToolRepoConventions, ToolDependencies, ToolHistory, ToolKnowledge:
			out[v] = true
		}
	}
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			add(s)
		}
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

func combinePages(limit int, pages ...PageInfo) PageInfo {
	info := PageInfo{Limit: limit}
	for _, page := range pages {
		if info.Limit == 0 && page.Limit > 0 {
			info.Limit = page.Limit
		}
		if page.More {
			info.More = true
		}
		if info.NextCursor == "" {
			info.NextCursor = page.NextCursor
		}
	}
	return info
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
		{ToolRepoConventions, "Return cached per-repository conventions from Hive's knowledge store."},
		{ToolDependencies, "Return cached bead dependency and suggested-plan context for the scoped task."},
		{ToolHistory, "Return cached recent merged PRs and closed issues related to the scoped task."},
		{ToolKnowledge, "Search cached knowledge-store entries scoped to the task repository."},
		{ToolContextBundle, "Return selected task MCP payloads in one capped response."},
	}
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{"name": d.name, "description": d.desc, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "maximum": MaxPageSize}, "cursor": map[string]any{"type": "string"}, "task_id": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "number": map[string]any{"type": "integer"}, "query": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}, "include": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}})
	}
	return out
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
	start, end, next, more := pageBounds(len(items), page)
	out := items[start:end]
	if out == nil {
		out = []CheckHealth{}
	}
	return CIHealthData{Checks: out}, PageInfo{NextCursor: next, Limit: page.Limit, More: more}
}

func PaginateConventions(items []RepoConvention, page PageRequest) ([]RepoConvention, PageInfo) {
	page = page.Normalized()
	start, end, next, more := pageBounds(len(items), page)
	out := items[start:end]
	if out == nil {
		out = []RepoConvention{}
	}
	return out, PageInfo{NextCursor: next, Limit: page.Limit, More: more}
}

func PaginateDependencies(data DependenciesData, page PageRequest) (DependenciesData, PageInfo) {
	page = page.Normalized()
	all := make([]DependencyNode, 0, len(data.BlockedBy)+len(data.Blocks)+len(data.SuggestedPlan))
	all = append(all, data.BlockedBy...)
	all = append(all, data.Blocks...)
	all = append(all, data.SuggestedPlan...)
	start, end, next, more := pageBounds(len(all), page)
	keep := map[string]int{}
	for i := start; i < end; i++ {
		keep[all[i].ID]++
	}
	filter := func(in []DependencyNode) []DependencyNode {
		out := make([]DependencyNode, 0, len(in))
		for _, node := range in {
			if keep[node.ID] > 0 {
				out = append(out, node)
				keep[node.ID]--
			}
		}
		if out == nil {
			out = []DependencyNode{}
		}
		return out
	}
	data.BlockedBy = filter(data.BlockedBy)
	data.Blocks = filter(data.Blocks)
	data.SuggestedPlan = filter(data.SuggestedPlan)
	return data, PageInfo{NextCursor: next, Limit: page.Limit, More: more}
}

func PaginateHistory(items []RelatedItem, page PageRequest) (HistoryData, PageInfo) {
	data, info := PaginateRelated(items, page)
	return HistoryData{Items: data.Items}, info
}

func PaginateKnowledge(items []KnowledgeItem, page PageRequest) (KnowledgeData, PageInfo) {
	page = page.Normalized()
	start, end, next, more := pageBounds(len(items), page)
	out := items[start:end]
	if out == nil {
		out = []KnowledgeItem{}
	}
	return KnowledgeData{Items: out}, PageInfo{NextCursor: next, Limit: page.Limit, More: more}
}

func pageBounds(length int, page PageRequest) (start, end int, next string, more bool) {
	start = cursorIndex(page.Cursor)
	if start > length {
		start = length
	}
	end = start + page.Limit
	more = end < length
	if end > length {
		end = length
	}
	if more {
		next = strconv.Itoa(end)
	}
	return start, end, next, more
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
