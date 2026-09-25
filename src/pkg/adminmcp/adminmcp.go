// Package adminmcp defines Hive's operator-facing admin MCP tools independent
// of the transport that serves them.
package adminmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	EndpointPath = "/api/admin/mcp"

	ProtocolVersion = "2025-03-26"
	ServerName      = "hive-admin-mcp"
	ServerVersion   = "0.1.0"

	ToolHiveStatus         = "hive_status"
	ToolAgentsList         = "agents_list"
	ToolRunsList           = "runs_list"
	ToolClaimsList         = "claims_list"
	ToolExclusionCatalogue = "exclusion_catalogue"
	ToolRefuseOperation    = "refuse_operation"

	DefaultResultLimit = 20
	MaxResultLimit     = 50
	MaxRequestBytes    = 64 * 1024
	MaxTextBytes       = 256 * 1024

	RefusalKindMechanical  = "mechanically_impossible"
	RefusalKindCategorical = "categorically_excluded"
	RefusalKindSwitchedOff = "switched_off"
)

var ErrForbidden = errors.New("admin MCP forbidden")

type Provider interface {
	Read(ctx context.Context, tool string, args map[string]any) (any, error)
}

type DataEnvelope struct {
	Data any `json:"data"`
}

type RefusalData struct {
	Type                  string `json:"type"`
	Operation             string `json:"operation"`
	Kind                  string `json:"kind"`
	Reason                string `json:"reason"`
	DoNotApproximate      string `json:"do_not_approximate,omitempty"`
	Tracker               string `json:"tracker,omitempty"`
	WhatWouldNeedToChange string `json:"what_would_need_to_change,omitempty"`
}

type PreviewContract struct {
	Mode    string `json:"mode"`
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"`
}

type ConfirmContract struct {
	Required bool   `json:"required"`
	Token    string `json:"token,omitempty"`
	Note     string `json:"note"`
}

type ToolMetadata struct {
	Preview PreviewContract `json:"preview"`
	Confirm ConfirmContract `json:"confirm"`
	Writes  bool            `json:"writes"`
}

type Exclusion struct {
	Operation             string `json:"operation"`
	Kind                  string `json:"kind"`
	Reason                string `json:"reason"`
	DoNotApproximate      string `json:"do_not_approximate,omitempty"`
	Tracker               string `json:"tracker,omitempty"`
	WhatWouldNeedToChange string `json:"what_would_need_to_change,omitempty"`
}

const judgementTracker = "https://github.com/hivecommons/hive/issues/8697"
const doNotApproximate = "Do not approximate this excluded operation by combining other admin MCP operations."

var exclusions = []Exclusion{
	{Operation: "POST /api/self-upgrade", Kind: RefusalKindMechanical, Reason: "The upgrade restarts the process serving the API the request arrived on, so the call severs its own connection and cannot report its outcome. The operator would be unable to distinguish a successful upgrade from a failed one.", WhatWouldNeedToChange: "Expose an upgrade mechanism that can report completion from outside the process being restarted."},
	{Operation: "POST /api/contribute/invite", Kind: RefusalKindMechanical, Reason: "`handleContributeInvite` resolves the caller's identity server-side via `resolveViewerUsername` and requires *that user's* contributor profile to hold a `trusted`, `merger` or `advisor` tier. Authority here is derived from who the caller is, not from the owner role, and there is no owner override. A credential carrying no personal identity gets 401.", WhatWouldNeedToChange: "Add an owner-authorized invite path that does not depend on per-user contributor identity."},
	{Operation: "fan-out across hives", Kind: RefusalKindMechanical, Reason: "Exactly one hive is active at a time by design, so an all-hives mode would contradict the surface rather than extend it.", WhatWouldNeedToChange: "Replace the single-active-hive model with an explicit multi-hive design."},
	{Operation: "POST /api/prs/{owner}/{repo}/{number}/queue-automerge", Kind: RefusalKindCategorical, Reason: "The only available action that directly causes a merge into real code, and the one a preview cannot meaningfully soften.", DoNotApproximate: doNotApproximate, Tracker: judgementTracker},
	{Operation: "PUT /api/config/agent/{name}/prompt", Kind: RefusalKindCategorical, Reason: "Kick text reaches the CLI once and is confirmed verbatim for that reason. A prompt reaches it on *every* future kick, and Hive does not scan that path either. Verbatim confirmation is a reasonable guard for one string and a weak one for a standing instruction.", DoNotApproximate: doNotApproximate, Tracker: judgementTracker},
	{Operation: "POST /api/release-channel", Kind: RefusalKindCategorical, Reason: "Changes which code the hive runs at its next upgrade. The effect is deferred and invisible at the moment of the change, so a preview can say nothing useful about it.", DoNotApproximate: doNotApproximate, Tracker: judgementTracker},
	{Operation: "knowledge writes", Kind: RefusalKindCategorical, Reason: "Agents read knowledge when they are handed work, so a write changes fleet behaviour with nothing in any fleet or agent view showing that it happened. Knowledge **reads** are in scope.", DoNotApproximate: doNotApproximate, Tracker: judgementTracker},
	{Operation: "Anything that issues, rotates, reads back or exchanges a credential", Kind: RefusalKindCategorical, Reason: "The surface exists to hand Hive state to a language model, and credential material is the one class of content that must never reach one. There is no version of this design in which these belong.", Tracker: judgementTracker},
	{Operation: "purely presentational endpoints", Kind: RefusalKindCategorical, Reason: "They have no meaning in a conversation.", Tracker: judgementTracker},
	{Operation: "Any tool taking a path, URL, or method as an argument", Kind: RefusalKindCategorical, Reason: "The allowlist is the design: Hive redacts per handler, so an unvetted endpoint is an unvetted redaction story.", Tracker: judgementTracker},
	{Operation: "writes in phase 1", Kind: RefusalKindSwitchedOff, Reason: "Admin MCP phase 1 is read-only; write tools arrive only after the preview and confirmation contract is implemented in a later phase.", Tracker: "https://github.com/hivecommons/hive/issues/8699"},
}

func Exclusions() []Exclusion {
	out := make([]Exclusion, len(exclusions))
	copy(out, exclusions)
	return out
}

func RefusalFor(operation string) (RefusalData, bool) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		operation = "unspecified operation"
	}
	needle := strings.ToLower(operation)
	for _, ex := range exclusions {
		if strings.ToLower(ex.Operation) == needle || strings.Contains(strings.ToLower(ex.Operation), needle) || strings.Contains(needle, strings.ToLower(ex.Operation)) {
			return refusalData(ex), true
		}
	}
	return refusalData(Exclusion{Operation: operation, Kind: RefusalKindSwitchedOff, Reason: "This operation is not an admin MCP phase 1 read tool; phase 1 is read-only and does not approximate excluded writes.", Tracker: "https://github.com/hivecommons/hive/issues/8699"}), false
}

func refusalData(ex Exclusion) RefusalData {
	return RefusalData{Type: "refusal", Operation: ex.Operation, Kind: ex.Kind, Reason: ex.Reason, DoNotApproximate: ex.DoNotApproximate, Tracker: ex.Tracker, WhatWouldNeedToChange: ex.WhatWouldNeedToChange}
}

type Handler struct{ Provider Provider }

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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBytes)).Decode(&req); err != nil {
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
		return map[string]any{"tools": Tools()}, nil
	case "tools/call":
		return h.callTool(r.Context(), req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (h *Handler) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
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
	if !AllowedTool(p.Name) {
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}
	var data any
	switch p.Name {
	case ToolExclusionCatalogue:
		data = map[string]any{"exclusions": Exclusions(), "writes_enabled": false, "preview": phaseOneMetadata().Preview, "confirm": phaseOneMetadata().Confirm}
	case ToolRefuseOperation:
		data, _ = RefusalFor(stringArg(p.Arguments, "operation"))
	default:
		if h.Provider == nil {
			return nil, &rpcError{Code: -32000, Message: "provider unavailable"}
		}
		result, err := h.Provider.Read(ctx, p.Name, p.Arguments)
		if err != nil {
			return nil, toolErr(err)
		}
		data = result
	}
	b, err := json.Marshal(DataEnvelope{Data: Scrub(data)})
	if err != nil {
		return nil, toolErr(err)
	}
	if len(b) > MaxTextBytes {
		return nil, &rpcError{Code: -32000, Message: "admin MCP result exceeds text cap"}
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}, nil
}

func Tools() []map[string]any {
	defs := []struct{ name, desc string }{
		{ToolHiveStatus, "Read this hive's dashboard status summary."},
		{ToolAgentsList, "Read the capped list of agents known to this hive."},
		{ToolRunsList, "Read the capped list of runs known to this hive."},
		{ToolClaimsList, "Read the capped list of issue claims known to this hive."},
		{ToolExclusionCatalogue, "Return the askable catalogue of operations deliberately excluded from admin MCP."},
		{ToolRefuseOperation, "Return the recorded refusal for an excluded or unavailable operation."},
	}
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{"name": d.name, "description": d.desc, "inputSchema": inputSchema(d.name), "annotations": map[string]any{"readOnlyHint": true}, "metadata": phaseOneMetadata()})
	}
	return out
}

func AllowedTool(name string) bool {
	switch name {
	case ToolHiveStatus, ToolAgentsList, ToolRunsList, ToolClaimsList, ToolExclusionCatalogue, ToolRefuseOperation:
		return true
	}
	return false
}

func phaseOneMetadata() ToolMetadata {
	return ToolMetadata{Preview: PreviewContract{Mode: "read_only_phase_1", Enabled: false, Note: "Write previews are scaffolded but no write tools ship in phase 1."}, Confirm: ConfirmContract{Required: false, Note: "Confirmation is reserved for later write phases; reads require no confirmation."}, Writes: false}
}

func inputSchema(name string) map[string]any {
	props := map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResultLimit}}
	required := []string{}
	if name == ToolRefuseOperation {
		props = map[string]any{"operation": map[string]any{"type": "string"}}
		required = []string{"operation"}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func LimitFromArgs(args map[string]any) int {
	limit := DefaultResultLimit
	switch v := args["limit"].(type) {
	case float64:
		limit = int(v)
	case int:
		limit = v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	return normalizeLimit(limit)
}

func CapResult(v any, limit int) any {
	limit = normalizeLimit(limit)
	switch x := v.(type) {
	case []any:
		if len(x) > limit {
			return x[:limit]
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x)+1)
		for k, value := range x {
			if items, ok := value.([]any); ok && cappedField(k) && len(items) > limit {
				out[k] = items[:limit]
				out[k+"_truncated"] = true
				continue
			}
			out[k] = value
		}
		return out
	default:
		return v
	}
}

func cappedField(key string) bool {
	switch strings.ToLower(key) {
	case "agents", "runs", "claims", "items", "data":
		return true
	default:
		return false
	}
}

func normalizeLimit(limit int) int {
	if limit <= 0 || limit > MaxResultLimit {
		return DefaultResultLimit
	}
	return limit
}

func Scrub(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var decoded any
	if err := json.Unmarshal(maskSensitive(b), &decoded); err != nil {
		return v
	}
	return decoded
}

func maskSensitive(b []byte) []byte {
	var buf bytes.Buffer
	dec := json.NewDecoder(bytes.NewReader(b))
	enc := json.NewEncoder(&buf)
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, val := range x {
				if sensitiveKey(k) {
					out[k] = "[masked:" + maskLabel(k) + "]"
				} else {
					out[k] = walk(val)
				}
			}
			return out
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case string:
			if looksSecret(x) {
				return "[masked:secret-like]"
			}
		}
		return v
	}
	var value any
	if err := dec.Decode(&value); err != nil {
		return b
	}
	_ = enc.Encode(walk(value))
	return bytes.TrimSpace(buf.Bytes())
}

func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "token") || strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "credential") || strings.Contains(k, "authorization")
}
func maskLabel(k string) string {
	k = strings.ToLower(k)
	if strings.Contains(k, "token") {
		return "token"
	}
	if strings.Contains(k, "authorization") {
		return "authorization"
	}
	return "secret"
}
func looksSecret(s string) bool {
	return strings.HasPrefix(s, "Bearer ") || strings.HasPrefix(s, "ghp_") || strings.HasPrefix(s, "github_pat_") || strings.HasPrefix(s, "hive_")
}
func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}
func toolErr(err error) *rpcError {
	if errors.Is(err, ErrForbidden) {
		return &rpcError{Code: -32003, Message: err.Error()}
	}
	return &rpcError{Code: -32000, Message: err.Error()}
}
func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func SortKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func Errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }
