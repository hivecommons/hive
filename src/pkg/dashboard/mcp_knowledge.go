package dashboard

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Public knowledge MCP endpoint (#10615).
//
// A hive owner can flip HIVE_PUBLIC_KNOWLEDGE on to let ANY external agent
// (Goose, Claude, Copilot, a curl loop) read this hive's operational knowledge
// over a Model Context Protocol endpoint with no hub session, dashboard token,
// or contributor registration. The surface is strictly read-only: the only
// JSON-RPC methods served are the MCP handshake plus tools/list and
// tools/call, and the only tools are search/get/export. There is no code path
// from this handler to CreateFact/UpdateFact/DeleteFact or any vault mutation,
// so an anonymous caller can never overwrite a knowledge base.
//
// Default is OFF and the endpoint 404s. isPublicPath and the hub's
// publicExactPaths admit /mcp/knowledge past authentication only so this
// handler can run its own fail-closed check; they do not weaken anything else.

const (
	// publicKnowledgeMCPPath is the spoke endpoint external agents connect to.
	publicKnowledgeMCPPath = "/mcp/knowledge"

	// publicKnowledgeEnabledEnv is the owner switch. Same spelling as
	// HIVE_METRICS_ENABLED: 1|true|yes|on. Read on each request so an owner
	// can close the surface without a pod roll.
	publicKnowledgeEnabledEnv = "HIVE_PUBLIC_KNOWLEDGE"

	// publicKnowledgeTagsEnv optionally narrows the public projection to facts
	// carrying at least one of the listed (comma-separated) tags. Empty means
	// every fact of a public type is exposed.
	publicKnowledgeTagsEnv = "HIVE_PUBLIC_KNOWLEDGE_TAGS"

	// mcpProtocolVersion is the MCP spec revision this endpoint speaks.
	mcpProtocolVersion = "2025-03-26"

	// publicKnowledgeDefaultLimit / MaxLimit bound knowledge_search results so
	// an anonymous caller cannot pull the whole base one query at a time
	// faster than knowledge_export already allows.
	publicKnowledgeDefaultLimit = 10
	publicKnowledgeMaxLimit     = 50

	// publicKnowledgeMaxBodyBytes caps a JSON-RPC request body.
	publicKnowledgeMaxBodyBytes = 64 << 10

	// JSON-RPC 2.0 error codes.
	jsonRPCParseError     = -32700
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
)

// publicKnowledgeTypes is the allow-list of fact types that leave the hive
// through the public endpoint. Operational knowledge only — ideation and
// governance types (idea, vision, constitution, requirement, constraint,
// stakeholder, decision) describe the project's internal direction and stay
// behind authentication.
var publicKnowledgeTypes = map[string]struct{}{
	"pattern":       {},
	"gotcha":        {},
	"regression":    {},
	"test_scaffold": {},
	"integration":   {},
	"coverage_rule": {},
	"general":       {},
	"":              {}, // untyped facts render under "general"
}

// publicKnowledgeTypeOrder fixes section order in knowledge_export output.
var publicKnowledgeTypeOrder = []string{
	"pattern", "gotcha", "regression", "coverage_rule", "test_scaffold", "integration", "general",
}

var publicKnowledgeTypeLabels = map[string]string{
	"pattern":       "Patterns",
	"gotcha":        "Gotchas",
	"regression":    "Regressions",
	"test_scaffold": "Test Scaffolds",
	"integration":   "Integration",
	"coverage_rule": "Coverage Rules",
	"general":       "General",
}

// publicKnowledgeEnabled reports whether the owner has opened the read-only
// knowledge endpoint.
func publicKnowledgeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(publicKnowledgeEnabledEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// publicKnowledgeTags returns the lower-cased tag allow-list, or nil for "all".
func publicKnowledgeTags() []string {
	raw := strings.TrimSpace(os.Getenv(publicKnowledgeTagsEnv))
	if raw == "" {
		return nil
	}
	var tags []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// publicFact is the outward projection of a knowledge.Fact. Sources (PR,
// comment, author), usage counters, confidence reasoning, and lifecycle
// phase are deliberately absent.
type publicFact struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	Type       string   `json:"type"`
	Body       string   `json:"body"`
	Confidence float64  `json:"confidence,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Related    []string `json:"related,omitempty"`
}

func toPublicFact(f knowledge.Fact) publicFact {
	t := string(f.Type)
	if t == "" {
		t = "general"
	}
	pf := publicFact{Slug: f.Slug, Title: f.Title, Type: t, Body: f.Body, Tags: f.Tags, Related: f.Related}
	if f.ConfidenceScored {
		pf.Confidence = f.Confidence
	}
	return pf
}

// publicKnowledgeFilter keeps only facts of a public type that match the tag
// allow-list (if any). It is the single choke point every tool goes through.
func publicKnowledgeFilter(facts []knowledge.Fact, tags []string) []knowledge.Fact {
	out := make([]knowledge.Fact, 0, len(facts))
	for _, f := range facts {
		if _, ok := publicKnowledgeTypes[string(f.Type)]; !ok {
			continue
		}
		if len(tags) > 0 && !factHasAnyTag(f, tags) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func factHasAnyTag(f knowledge.Fact, tags []string) bool {
	for _, have := range f.Tags {
		have = strings.ToLower(have)
		for _, want := range tags {
			if have == want {
				return true
			}
		}
	}
	return false
}

// renderPublicKnowledgeMarkdown is the knowledge_export body. It mirrors the
// authenticated /api/knowledge/export layout but only over the public
// projection, and sorts deterministically so unchanged knowledge yields an
// identical document.
func renderPublicKnowledgeMarkdown(facts []knowledge.Fact) string {
	grouped := make(map[string][]knowledge.Fact)
	for _, f := range facts {
		t := string(f.Type)
		if t == "" {
			t = "general"
		}
		grouped[t] = append(grouped[t], f)
	}

	var sb strings.Builder
	sb.WriteString("# Hive Public Knowledge\n\n")
	sb.WriteString("This document is auto-generated from a hive knowledge base and served read-only.\n")
	sb.WriteString("It refreshes as the hive learns — do not edit manually.\n\n")

	for _, t := range publicKnowledgeTypeOrder {
		ff := grouped[t]
		if len(ff) == 0 {
			continue
		}
		sortFactsStable(ff)
		label := publicKnowledgeTypeLabels[t]
		if label == "" {
			label = titleCaseWords(t)
		}
		sb.WriteString("## " + label + "\n\n")
		for _, f := range ff {
			sb.WriteString("### " + f.Title + "\n\n")
			if f.Body != "" {
				sb.WriteString(f.Body + "\n\n")
			}
			if len(f.Tags) > 0 {
				sb.WriteString("Tags: " + strings.Join(f.Tags, ", ") + "\n\n")
			}
		}
	}
	return sb.String()
}

// --- JSON-RPC / MCP wire types -------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type mcpToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
	Annotations interface{} `json:"annotations,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// mcpReadOnlyAnnotations tells MCP clients every tool here is safe to call
// without confirmation: it neither mutates nor reaches outside the hive.
var mcpReadOnlyAnnotations = map[string]interface{}{
	"readOnlyHint":    true,
	"destructiveHint": false,
	"idempotentHint":  true,
	"openWorldHint":   false,
}

func publicKnowledgeToolDefs() []mcpToolDef {
	return []mcpToolDef{
		{
			Name:        "knowledge_search",
			Description: "Search this hive's public operational knowledge (patterns, gotchas, regressions, integrations) by free-text query. Returns matching facts as JSON.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "Free-text search query."},
					"type":  map[string]interface{}{"type": "string", "description": "Optional fact type filter: pattern, gotcha, regression, test_scaffold, integration, coverage_rule, general."},
					"limit": map[string]interface{}{"type": "integer", "description": fmt.Sprintf("Max results (default %d, max %d).", publicKnowledgeDefaultLimit, publicKnowledgeMaxLimit)},
				},
				"required": []string{"query"},
			},
			Annotations: mcpReadOnlyAnnotations,
		},
		{
			Name:        "knowledge_get",
			Description: "Fetch a single public knowledge fact by slug (as returned by knowledge_search).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"slug": map[string]interface{}{"type": "string", "description": "Fact slug."},
				},
				"required": []string{"slug"},
			},
			Annotations: mcpReadOnlyAnnotations,
		},
		{
			Name:        "knowledge_export",
			Description: "Export the entire public knowledge base as a single Markdown document, grouped by fact type.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Annotations: mcpReadOnlyAnnotations,
		},
	}
}

// handlePublicKnowledgeMCP serves POST /mcp/knowledge. It is the ONLY
// anonymous entry point into knowledge and fails closed when the owner switch
// is off, regardless of what the auth middleware admitted.
func (s *Server) handlePublicKnowledgeMCP(w http.ResponseWriter, r *http.Request) {
	if !publicKnowledgeEnabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jsonError(w, "public knowledge MCP endpoint accepts POST only (no SSE stream)", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, publicKnowledgeMaxBodyBytes+1))
	if err != nil || len(body) > publicKnowledgeMaxBodyBytes {
		writeJSONRPC(w, http.StatusRequestEntityTooLarge, jsonRPCResponse{JSONRPC: "2.0", ID: nullID, Error: &jsonRPCError{Code: jsonRPCInvalidRequest, Message: "request body too large"}})
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPC(w, http.StatusBadRequest, jsonRPCResponse{JSONRPC: "2.0", ID: nullID, Error: &jsonRPCError{Code: jsonRPCParseError, Message: "invalid JSON"}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSONRPC(w, http.StatusBadRequest, jsonRPCResponse{JSONRPC: "2.0", ID: idOrNull(req.ID), Error: &jsonRPCError{Code: jsonRPCInvalidRequest, Message: "expected JSON-RPC 2.0 request with a method"}})
		return
	}

	// Notifications carry no id and get no body (MCP Streamable HTTP: 202).
	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := jsonRPCResponse{JSONRPC: "2.0", ID: idOrNull(req.ID)}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]interface{}{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{"listChanged": false}},
			"serverInfo":      map[string]interface{}{"name": "hive-public-knowledge", "version": "1"},
			"instructions":    "Read-only access to this hive's operational knowledge. Use knowledge_search for targeted lookups and knowledge_export for the full Markdown document.",
		}
	case "ping":
		resp.Result = map[string]interface{}{}
	case "tools/list":
		resp.Result = map[string]interface{}{"tools": publicKnowledgeToolDefs()}
	case "tools/call":
		result, rpcErr := s.callPublicKnowledgeTool(r, req.Params)
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &jsonRPCError{Code: jsonRPCMethodNotFound, Message: "method not found: " + req.Method}
	}
	writeJSONRPC(w, http.StatusOK, resp)
}

var nullID = json.RawMessage("null")

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return nullID
	}
	return id
}

func writeJSONRPC(w http.ResponseWriter, status int, resp jsonRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// callPublicKnowledgeTool dispatches tools/call. Unknown tool names are a
// JSON-RPC invalid-params error; tool-level problems (fact not found) come
// back as isError results per the MCP spec so the model can recover.
func (s *Server) callPublicKnowledgeTool(r *http.Request, raw json.RawMessage) (interface{}, *jsonRPCError) {
	var params mcpToolCallParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "invalid tools/call params"}
		}
	}
	if !s.ensureKnowledge() {
		return toolText("Knowledge base not available."), nil
	}
	tags := publicKnowledgeTags()

	switch params.Name {
	case "knowledge_search":
		var args struct {
			Query string `json:"query"`
			Type  string `json:"type"`
			Limit int    `json:"limit"`
		}
		if len(params.Arguments) > 0 {
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "invalid knowledge_search arguments"}
			}
		}
		args.Query = strings.TrimSpace(args.Query)
		if args.Query == "" {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "query is required"}
		}
		if _, ok := publicKnowledgeTypes[args.Type]; !ok {
			return toolError(fmt.Sprintf("type %q is not a public fact type", args.Type)), nil
		}
		limit := args.Limit
		if limit <= 0 {
			limit = publicKnowledgeDefaultLimit
		}
		if limit > publicKnowledgeMaxLimit {
			limit = publicKnowledgeMaxLimit
		}
		// Over-fetch so the public filter does not starve the result set, then
		// trim to the requested bound.
		facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, args.Query, args.Type, publicKnowledgeMaxLimit*2)
		facts = publicKnowledgeFilter(facts, tags)
		if len(facts) > limit {
			facts = facts[:limit]
		}
		out := make([]publicFact, 0, len(facts))
		for _, f := range facts {
			out = append(out, toPublicFact(f))
		}
		return toolJSON(map[string]interface{}{"query": args.Query, "count": len(out), "results": out})

	case "knowledge_get":
		var args struct {
			Slug string `json:"slug"`
		}
		if len(params.Arguments) > 0 {
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "invalid knowledge_get arguments"}
			}
		}
		args.Slug = strings.TrimSpace(args.Slug)
		if args.Slug == "" {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "slug is required"}
		}
		// Resolve through the same filtered listing search uses so a private
		// fact can never be fetched by guessing its slug.
		all := publicKnowledgeFilter(s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0), tags)
		for _, f := range all {
			if f.Slug == args.Slug {
				return toolJSON(toPublicFact(f))
			}
		}
		return toolError("fact not found: " + args.Slug), nil

	case "knowledge_export":
		all := publicKnowledgeFilter(s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0), tags)
		md := renderPublicKnowledgeMarkdown(all)
		sum := sha256.Sum256([]byte(md))
		return map[string]interface{}{
			"content": []mcpContent{{Type: "text", Text: md}},
			"_meta":   map[string]interface{}{"etag": fmt.Sprintf(`"%x"`, sum), "facts": len(all)},
		}, nil

	case "":
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "tool name is required"}
	default:
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "unknown tool: " + params.Name}
	}
}

func toolText(text string) mcpToolResult {
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: text}}}
}

func toolError(text string) mcpToolResult {
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: text}}, IsError: true}
}

func toolJSON(v interface{}) (interface{}, *jsonRPCError) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolError("failed to encode result"), nil
	}
	return toolText(string(b)), nil
}

// publicKnowledgeTypeList is exported for docs/tests: the sorted public types.
func publicKnowledgeTypeList() []string {
	out := make([]string, 0, len(publicKnowledgeTypes))
	for t := range publicKnowledgeTypes {
		if t != "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
