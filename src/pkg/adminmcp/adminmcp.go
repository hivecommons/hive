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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	EndpointPath = "/api/admin/mcp"

	ProtocolVersion = "2025-03-26"
	ServerName      = "hive-admin-mcp"
	ServerVersion   = "0.1.0"

	ToolHiveStatus         = "hive_status"
	ToolFleetStatus        = "fleet_status"
	ToolAgentsList         = "agents_list"
	ToolRunsList           = "runs_list"
	ToolLeasesList         = "leases_list"
	ToolClaimsList         = "claims_list"
	ToolPlansList          = "plans_list"
	ToolAuditLog           = "audit_log"
	ToolSettingsRead       = "settings_read"
	ToolAutonomyReadiness  = "autonomy_readiness"
	ToolSpendRead          = "spend_read"
	ToolContributorsList   = "contributors_list"
	ToolKnowledgeRead      = "knowledge_read"
	ToolHiveAdvisor        = "hive_advisor"
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
	{Operation: "unregistered write operations", Kind: RefusalKindSwitchedOff, Reason: "Admin MCP writes are limited to registered WriteOp implementations; do not approximate an unavailable write by combining other operations.", Tracker: "https://github.com/hivecommons/hive/issues/8697"},
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
	// Exact, case-folded match only: a partial name ("knowledge", "api") must
	// not borrow the recorded refusal of a different operation.
	for _, ex := range exclusions {
		if strings.EqualFold(ex.Operation, operation) {
			return refusalData(ex), true
		}
	}
	return refusalData(Exclusion{Operation: operation, Kind: RefusalKindSwitchedOff, Reason: "This operation is not a registered admin MCP tool or write operation; the surface does not approximate unavailable writes.", Tracker: "https://github.com/hivecommons/hive/issues/8697"}), false
}

func refusalData(ex Exclusion) RefusalData {
	return RefusalData{Type: "refusal", Operation: ex.Operation, Kind: ex.Kind, Reason: ex.Reason, DoNotApproximate: ex.DoNotApproximate, Tracker: ex.Tracker, WhatWouldNeedToChange: ex.WhatWouldNeedToChange}
}

type Handler struct {
	Provider        Provider
	WritesEnabled   bool
	WriteClient     WriteClient
	PendingStore    PendingStore
	WriteRegistry   *WriteRegistry
	ConfirmationTTL time.Duration
	HiveID          string
	Now             func() time.Time
	// WritesUnavailableReason, when set, reports writes as disabled for this
	// caller even if the operator enabled them, because a confirmed write could
	// not authenticate. Preview fails fast instead of minting a confirmation
	// that can never be executed.
	WritesUnavailableReason string
}

type HandlerOption func(*Handler)

func WithWritesEnabled(enabled bool) HandlerOption {
	return func(h *Handler) { h.WritesEnabled = enabled }
}
func WithWriteClient(client WriteClient) HandlerOption {
	return func(h *Handler) { h.WriteClient = client }
}
func WithPendingStore(store PendingStore) HandlerOption {
	return func(h *Handler) { h.PendingStore = store }
}
func WithWriteRegistry(registry *WriteRegistry) HandlerOption {
	return func(h *Handler) { h.WriteRegistry = registry }
}
func WithHiveID(hive string) HandlerOption {
	return func(h *Handler) { h.HiveID = strings.TrimSpace(hive) }
}
func WithConfirmationTTL(ttl time.Duration) HandlerOption {
	return func(h *Handler) { h.ConfirmationTTL = ttl }
}
func WithClock(now func() time.Time) HandlerOption { return func(h *Handler) { h.Now = now } }
func WithWritesUnavailable(reason string) HandlerOption {
	return func(h *Handler) { h.WritesUnavailableReason = strings.TrimSpace(reason) }
}

func (h *Handler) writesUsable() bool {
	return h.WritesEnabled && h.WritesUnavailableReason == ""
}

func (h *Handler) writesDisabledErr() error {
	if h.WritesEnabled && h.WritesUnavailableReason != "" {
		return fmt.Errorf("%w for this caller: %s", ErrWritesDisabled, h.WritesUnavailableReason)
	}
	return ErrWritesDisabled
}

func NewHandler(provider Provider, opts ...HandlerOption) *Handler {
	h := &Handler{Provider: provider, WriteRegistry: DefaultWriteRegistry(), ConfirmationTTL: DefaultConfirmationTTL, HiveID: "local"}
	if writer, ok := provider.(WriteClient); ok {
		h.WriteClient = writer
	}
	for _, opt := range opts {
		if opt != nil {
			opt(h)
		}
	}
	if h.PendingStore == nil {
		h.PendingStore = NewMemoryPendingStore()
	}
	if h.WriteRegistry == nil {
		h.WriteRegistry = DefaultWriteRegistry()
	}
	if h.ConfirmationTTL <= 0 {
		h.ConfirmationTTL = DefaultConfirmationTTL
	}
	if h.HiveID == "" {
		h.HiveID = "local"
	}
	return h
}

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
		return map[string]any{"tools": toolDefs(h.writesUsable(), h.WritesUnavailableReason)}, nil
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
		data = catalogue(h.writesUsable(), h.WritesUnavailableReason, h.WriteRegistry)
	case ToolRefuseOperation:
		data, _ = RefusalFor(stringArg(p.Arguments, "operation"))
	case ToolWritePreview:
		result, err := h.PreviewWrite(ctx, p.Arguments)
		if err != nil {
			return nil, toolErr(err)
		}
		data = result
	case ToolWriteConfirm:
		result, err := h.ConfirmWrite(ctx, p.Arguments)
		if refusal, ok := HiveRefusalFromError(err); ok {
			data = refusal
			break
		}
		if err != nil {
			return nil, toolErr(err)
		}
		data = result
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
	b, err := EncodeResult(data)
	if err != nil {
		return nil, toolErr(err)
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}, nil
}

func Tools() []map[string]any { return ToolsWithWritesEnabled(false) }

func ToolsWithWritesEnabled(writesEnabled bool) []map[string]any {
	return toolDefs(writesEnabled, "")
}

func toolDefs(writesEnabled bool, unavailableReason string) []map[string]any {
	defs := []struct{ name, desc string }{
		{ToolHiveStatus, "Read this hive's dashboard status summary."},
		{ToolFleetStatus, "Read this hive's fleet status, including repo and agent health rollups."},
		{ToolAgentsList, "Read the capped list of agents known to this hive."},
		{ToolRunsList, "Read the capped list of runs known to this hive."},
		{ToolLeasesList, "Read active run leases through the existing runs surface."},
		{ToolClaimsList, "Read the capped list of issue claims known to this hive."},
		{ToolPlansList, "Read the capped list of saved plans."},
		{ToolAuditLog, "Read the capped audit log for this hive."},
		{ToolSettingsRead, "Read governor settings through the existing dashboard settings endpoint."},
		{ToolAutonomyReadiness, "Read autonomy/readiness status through the existing Nous status endpoint."},
		{ToolSpendRead, "Read spend and budget status through the existing cost endpoint."},
		{ToolContributorsList, "Read the capped list of contributors known to this hive."},
		{ToolKnowledgeRead, "Read knowledge-system health and statistics."},
		{ToolHiveAdvisor, "Read hive advisor recommendations."},
		{ToolAgentNudgeStatus, "Read the asynchronous delivery outcome for the latest nudge sent to one agent."},
		{ToolWritePreview, "Preview a registered write operation and create a durable pending confirmation."},
		{ToolWriteConfirm, "Confirm and execute a previously previewed write operation."},
		{ToolExclusionCatalogue, "Return the askable catalogue of operations deliberately excluded from admin MCP."},
		{ToolRefuseOperation, "Return the recorded refusal for an excluded or unavailable operation."},
	}
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		metadata := phaseOneMetadata()
		readOnly := true
		if d.name == ToolWritePreview || d.name == ToolWriteConfirm {
			metadata = writeMetadata(writesEnabled, unavailableReason)
			readOnly = false
		}
		out = append(out, map[string]any{"name": d.name, "description": d.desc, "inputSchema": inputSchema(d.name), "annotations": map[string]any{"readOnlyHint": readOnly}, "metadata": metadata})
	}
	return out
}

func AllowedTool(name string) bool {
	switch name {
	case ToolHiveStatus, ToolFleetStatus, ToolAgentsList, ToolRunsList, ToolLeasesList, ToolClaimsList,
		ToolPlansList, ToolAuditLog, ToolSettingsRead, ToolAutonomyReadiness, ToolSpendRead,
		ToolContributorsList, ToolKnowledgeRead, ToolHiveAdvisor, ToolAgentNudgeStatus, ToolWritePreview, ToolWriteConfirm,
		ToolExclusionCatalogue, ToolRefuseOperation:
		return true
	}
	return false
}

func phaseOneMetadata() ToolMetadata {
	return ToolMetadata{Preview: PreviewContract{Mode: "phase_3_write_contract", Enabled: false, Note: "Read tools require no confirmation; write tools use the phase 3 preview-and-confirm contract when explicitly enabled."}, Confirm: ConfirmContract{Required: false, Note: "Reads require no confirmation; write confirmations are handled by write_confirm."}, Writes: false}
}

func writeMetadata(enabled bool, unavailableReason string) ToolMetadata {
	note := "Writes are registered but disabled until the operator explicitly enables admin MCP writes."
	if enabled {
		note = "Writes require preview followed by durable confirmation."
	} else if unavailableReason != "" {
		note = "Writes are enabled on this hive but unavailable to this caller: " + unavailableReason
	}
	return ToolMetadata{Preview: PreviewContract{Mode: "preview_confirm", Enabled: enabled, Note: note}, Confirm: ConfirmContract{Required: true, Note: "Confirmations are one-use, hive-bound, action-bound, and expire."}, Writes: true}
}

func inputSchema(name string) map[string]any {
	props := map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResultLimit}}
	required := []string{}
	switch name {
	case ToolRefuseOperation:
		props = map[string]any{"operation": map[string]any{"type": "string"}}
		required = []string{"operation"}
	case ToolAgentNudgeStatus:
		props = map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}}
		required = []string{"agent"}
	case ToolWritePreview:
		props = map[string]any{"operation": map[string]any{"type": "string"}, "args": map[string]any{"type": "object"}}
		required = []string{"operation", "args"}
	case ToolWriteConfirm:
		props = map[string]any{"confirmation_id": map[string]any{"type": "string"}}
		required = []string{"confirmation_id"}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func (h *Handler) PreviewWrite(ctx context.Context, args map[string]any) (any, error) {
	if !h.writesUsable() {
		return nil, h.writesDisabledErr()
	}
	opName := cleanOperationName(stringArg(args, "operation"))
	op, ok := h.WriteRegistry.Get(opName)
	if !ok {
		return nil, fmt.Errorf("unknown write operation %q", opName)
	}
	opArgs, _ := args["args"].(map[string]any)
	if opArgs == nil {
		opArgs = map[string]any{}
	}
	preview, err := op.Preview(ctx, opArgs)
	if err != nil {
		return nil, err
	}
	if err := rejectUnpersistablePreview(op.Name(), opArgs, preview); err != nil {
		return nil, err
	}
	now := h.now()
	id, err := newConfirmationID()
	if err != nil {
		return nil, err
	}
	pending := PendingConfirmation{ID: id, Operation: op.Name(), Args: scrubPendingMap(opArgs).(map[string]any), Preview: persistedPendingPreview(preview), Hive: h.HiveID, CreatedAt: now, ExpiresAt: now.Add(h.ConfirmationTTL)}
	if err := h.PendingStore.Put(ctx, pending); err != nil {
		return nil, err
	}
	return map[string]any{"type": "write_preview", "confirmation_id": id, "expires_at": pending.ExpiresAt, "hive": pending.Hive, "preview": preview, "widening_disclosure": preview.WideningDisclosure}, nil
}

func (h *Handler) ConfirmWrite(ctx context.Context, args map[string]any) (any, error) {
	if !h.writesUsable() {
		return nil, h.writesDisabledErr()
	}
	if h.WriteClient == nil {
		return nil, fmt.Errorf("write client unavailable")
	}
	id := stringArg(args, "confirmation_id")
	if id == "" {
		return nil, fmt.Errorf("confirmation_id is required")
	}
	pending, err := h.PendingStore.Take(ctx, id)
	if err != nil {
		return nil, err
	}
	if pending.Hive != h.HiveID {
		return nil, fmt.Errorf("confirmation belongs to hive %q, not %q", pending.Hive, h.HiveID)
	}
	if !h.now().Before(pending.ExpiresAt) {
		return nil, ErrConfirmationExpired
	}
	op, ok := h.WriteRegistry.Get(pending.Operation)
	if !ok {
		return nil, fmt.Errorf("registered write operation %q is no longer available", pending.Operation)
	}
	preview, err := op.Preview(ctx, pending.Args)
	if err != nil {
		return nil, err
	}
	if preview.Request.Method != pending.Preview.Request.Method || preview.Request.Path != pending.Preview.Request.Path {
		return nil, fmt.Errorf("write preview changed since confirmation was issued")
	}
	result, err := h.WriteClient.ExecuteWrite(ctx, pending.Preview.Request)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "write_result", "confirmation_id": id, "operation": pending.Operation, "hive": pending.Hive, "result": result}, nil
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

func rejectUnpersistablePreview(op string, args map[string]any, preview WritePreview) error {
	if pendingValueNeedsFreshConfirm(args) || pendingValueNeedsFreshConfirm(preview.Request) || pendingValueNeedsFreshConfirm(preview.Details) {
		return fmt.Errorf("write preview for %s contains a confirmation value that cannot be stored; update that field outside admin MCP", op)
	}
	return nil
}

func pendingValueNeedsFreshConfirm(v any) bool {
	scrubbed := scrubPendingValue(v, "")
	orig, origErr := json.Marshal(v)
	next, nextErr := json.Marshal(scrubbed)
	return origErr == nil && nextErr == nil && !bytes.Equal(orig, next)
}

// persistedPendingPreview keeps only what ConfirmWrite reads back — the
// request it executes. Summary, effects, disclosure and details are for the
// caller of write_preview and would otherwise duplicate the args (up to a
// 10 000-char nudge prompt) in every stored entry (#9162).
func persistedPendingPreview(preview WritePreview) WritePreview {
	return WritePreview{Operation: preview.Operation, Target: scrubOutboundString(preview.Target), Request: scrubPendingPreview(preview).Request}
}

func scrubPendingPreview(preview WritePreview) WritePreview {
	out := preview
	out.Summary = scrubOutboundString(out.Summary)
	out.Target = scrubOutboundString(out.Target)
	out.Request = scrubPendingValue(out.Request, "request").(WriteRequest)
	if out.Effects != nil {
		out.Effects = scrubPendingValue(out.Effects, "effects").([]string)
	}
	out.WideningDisclosure = scrubOutboundString(out.WideningDisclosure)
	out.ConfirmationMessage = scrubOutboundString(out.ConfirmationMessage)
	if out.Details != nil {
		out.Details = scrubPendingMap(out.Details).(map[string]any)
	}
	return out
}

func scrubPendingMap(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	return scrubPendingValue(m, "")
}

func scrubPendingValue(v any, key string) any {
	switch x := v.(type) {
	case WriteRequest:
		out := x
		out.Path = scrubOutboundString(out.Path)
		out.Body = scrubPendingValue(out.Body, "body")
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if credentialBearingKey(k) && !maskExempt(k, val) {
				out[k] = "[masked:" + maskLabel(k) + "]"
				continue
			}
			out[k] = scrubPendingValue(val, k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = scrubPendingValue(val, key)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, val := range x {
			out[i] = scrubOutboundString(val)
		}
		return out
	case string:
		if credentialBearingKey(key) {
			return "[masked:" + maskLabel(key) + "]"
		}
		return scrubOutboundString(x)
	default:
		return v
	}
}

func credentialBearingKey(k string) bool {
	key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(k), "_", ""), "-", ""))
	if key == "" {
		return false
	}
	if key == "otelheaders" {
		return true
	}
	return strings.Contains(key, "secret") ||
		strings.Contains(key, "password") ||
		strings.Contains(key, "credential") ||
		strings.Contains(key, "authorization") ||
		strings.Contains(key, "token") ||
		strings.Contains(key, "apikey") ||
		strings.Contains(key, "header") ||
		strings.HasSuffix(key, "key")
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

func ReadPath(tool string, limit int) (string, bool) {
	values := url.Values{}
	if limit > 0 {
		values.Set("limit", fmt.Sprint(normalizeLimit(limit)))
	}
	suffix := ""
	if encoded := values.Encode(); encoded != "" {
		suffix = "?" + encoded
	}
	switch tool {
	case ToolHiveStatus:
		return "/api/status/summary", true
	case ToolFleetStatus:
		return "/api/status", true
	case ToolAgentsList:
		return "/api/agents" + suffix, true
	case ToolRunsList, ToolLeasesList:
		return "/api/runs" + suffix, true
	case ToolClaimsList:
		return "/api/claims" + suffix, true
	case ToolPlansList:
		return "/api/plans" + suffix, true
	case ToolAuditLog:
		return "/api/audit", true
	case ToolSettingsRead:
		return "/api/config/governor", true
	case ToolAutonomyReadiness:
		return "/api/nous/status", true
	case ToolSpendRead:
		return "/api/cost", true
	case ToolContributorsList:
		return "/api/contributors" + suffix, true
	case ToolKnowledgeRead:
		return "/api/knowledge/stats", true
	case ToolHiveAdvisor:
		return "/api/hive-advice", true
	default:
		return "", false
	}
}

func CapResult(v any, limit int) any {
	limit = normalizeLimit(limit)
	switch x := v.(type) {
	case []any:
		if len(x) > limit {
			return map[string]any{
				"items":     x[:limit],
				"truncated": true,
				"limit":     limit,
				"total":     len(x),
			}
		}
		return x
	case map[string]any:
		var truncatedFields []string
		out := make(map[string]any, len(x)+1)
		for k, value := range x {
			if items, ok := value.([]any); ok && cappedField(k) && len(items) > limit {
				out[k] = items[:limit]
				out[k+"_truncated"] = true
				truncatedFields = append(truncatedFields, k)
				continue
			}
			out[k] = value
		}
		if len(truncatedFields) > 0 {
			sort.Strings(truncatedFields)
			out["_admin_mcp"] = map[string]any{
				"truncated":        true,
				"limit":            limit,
				"truncated_fields": truncatedFields,
			}
		}
		return out
	default:
		return v
	}
}

func cappedField(key string) bool {
	switch strings.ToLower(key) {
	case "agents", "runs", "leases", "claims", "plans", "entries", "contributors", "items", "data":
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
				if sensitiveKey(k) && !maskExempt(k, val) {
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
			scrubbed := scrubOutboundString(x)
			if scrubbed != x {
				return scrubbed
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

// credentialTokenQualifiers mark a plural "*tokens" key as a collection of
// credentials (refresh_tokens, access_tokens) rather than a usage count.
var credentialTokenQualifiers = []string{
	"access", "refresh", "id", "bearer", "auth", "api", "registration",
	"session", "lease", "oauth", "bot", "webhook", "github", "csrf", "jwt", "device",
}

// tokenCountKey reports whether k names a token count or usage block
// (tokens, totalTokens, input_tokens, max_tokens) rather than a credential.
func tokenCountKey(k string) bool {
	key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
	stem, ok := strings.CutSuffix(key, "tokens")
	if !ok {
		return false
	}
	for _, q := range credentialTokenQualifiers {
		if strings.HasSuffix(stem, q) {
			return false
		}
	}
	return true
}

// maskExempt reports whether a value under a sensitive-looking key cannot be
// a credential and must stay visible (#9161). Booleans carry no secret; a
// token count is numeric, and a token usage block is an object that the
// caller keeps walking, so its fields are still scrubbed by key and shape.
func maskExempt(k string, v any) bool {
	switch v.(type) {
	case bool:
		return true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number, map[string]any:
		return tokenCountKey(k)
	default:
		return false
	}
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

// looksSecret matches credential prefixes. The only hive-issued prefixed
// token is the task MCP lease (taskmcp.LeaseTokenPrefix); a bare "hive_"
// prefix would also mask identifiers such as hive_id or tool names.
func looksSecret(s string) bool {
	return strings.HasPrefix(s, "Bearer ") || strings.HasPrefix(s, "ghp_") || strings.HasPrefix(s, "github_pat_") || strings.HasPrefix(s, "hive_mcp_v1.")
}
func scrubOutboundString(s string) string {
	out := logscrub.ScrubString(s, logscrub.WithMarkers())
	if out != s {
		return out
	}
	if looksSecret(s) {
		return "[masked:secret-like]"
	}
	return s
}
func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}
func toolErr(err error) *rpcError {
	if errors.Is(err, ErrForbidden) {
		return &rpcError{Code: -32003, Message: scrubOutboundString(err.Error())}
	}
	return &rpcError{Code: -32000, Message: scrubOutboundString(err.Error())}
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

func Catalogue(writesEnabled bool, registry *WriteRegistry) map[string]any {
	return catalogue(writesEnabled, "", registry)
}

func catalogue(writesEnabled bool, unavailableReason string, registry *WriteRegistry) map[string]any {
	meta := writeMetadata(writesEnabled, unavailableReason)
	out := map[string]any{"exclusions": Exclusions(), "writes_enabled": writesEnabled, "write_operations": WriteOperationDescriptions(registry), "preview": meta.Preview, "confirm": meta.Confirm}
	if unavailableReason != "" {
		out["writes_unavailable_reason"] = unavailableReason
	}
	return out
}

func WriteOperationDescriptions(registry *WriteRegistry) []map[string]any {
	if registry == nil {
		registry = DefaultWriteRegistry()
	}
	ops := registry.Ops()
	out := make([]map[string]any, 0, len(ops))
	for _, op := range ops {
		out = append(out, map[string]any{"name": op.Name(), "description": op.Description(), "input_schema": op.InputSchema()})
	}
	return out
}
