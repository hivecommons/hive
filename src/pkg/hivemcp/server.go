package hivemcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

const ProtocolVersion = "2025-06-18"

type Handler func(ctx context.Context, name string, arguments map[string]any) (any, error)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func Serve(ctx context.Context, input io.Reader, output io.Writer, handler Handler) error {
	if handler == nil {
		return fmt.Errorf("MCP handler is required")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var message request
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			if err := encoder.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "invalid JSON"}}); err != nil {
				return err
			}
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		result, rpcErr := handle(ctx, message, handler)
		item := response{JSONRPC: "2.0", ID: message.ID, Result: result, Error: rpcErr}
		if err := encoder.Encode(item); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handle(ctx context.Context, message request, handler Handler) (any, *rpcError) {
	switch message.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "hive", "title": "Hive Production Automation", "version": "2"},
			"instructions":    "Use hive_setup_plan before hive_setup_apply. Coverage depth and automation authority are separate. Mutating tools enforce persistent authorization and append-only audit logs.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": Tools()}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil || params.Name == "" {
			return nil, &rpcError{Code: -32602, Message: "invalid tools/call parameters"}
		}
		tool, found := findTool(params.Name)
		if !found {
			return nil, &rpcError{Code: -32602, Message: "unknown Hive tool: " + params.Name}
		}
		if err := validateToolArguments(tool, params.Arguments); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid " + params.Name + " arguments: " + err.Error()}
		}
		value, err := handler(ctx, params.Name, params.Arguments)
		if err != nil {
			return toolResult(map[string]any{"error": err.Error()}, true), nil
		}
		return toolResult(value, false), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func toolResult(value any, isError bool) map[string]any {
	structured := normalizeObject(value)
	data, _ := json.Marshal(structured)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(data)}},
		"structuredContent": structured,
		"isError":           isError,
	}
}

func normalizeObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"result": fmt.Sprint(value)}
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return map[string]any{"result": value}
	}
	return object
}

func Tools() []Tool {
	tools := []Tool{
		tool("hive_setup_plan", "Plan Hive setup", "Read-only. Inspects a repository and returns the exact coverage, workflow, authorization, and file-change plan. Makes no GitHub mutations.", true, false, setupSchema()),
		tool("hive_setup_apply", "Apply Hive setup", "Requires explicit repository setup authority. Creates or updates one setup branch and PR, initializes persistent state, and records every allowed or denied write.", false, false, setupSchema()),
		tool("hive_doctor", "Check Hive readiness", "Read-only. Verifies config, checkout, immutable Visual Hive pin, provider authentication, and production gates.", true, false, stateSchema()),
		tool("hive_status", "Read Hive status", "Read-only. Returns persistent repository, queue, finding, issue, PR, policy, and pause state.", true, false, stateSchema()),
		tool("hive_run", "Run Hive now", "Requires configured run authority. Launches a complete Visual Hive scan and processes trusted evidence through Hive lifecycle policy.", false, false, runSchema()),
		tool("hive_start", "Start Hive scheduler", "Requires operator authority. Starts the persistent scheduler using the configured immutable runtime.", false, false, startSchema()),
		tool("hive_stop", "Stop Hive scheduler", "Requires operator authority. Stops the persistent scheduler while preserving state.", false, false, stateSchema()),
		tool("hive_plan_merge_approval", "Plan an exact held-repair approval", "Read-only. Returns the live base SHA and raw diff digest that the accountable apply call must repeat.", true, false, planMergeApprovalSchema()),
		tool("hive_approve_merge", "Approve an exact held repair", "Requires accountable operator authority. Applies only when the caller repeats the reviewed PR, base, head, diff digest, and reason; Hive rechecks all gates and performs the merge on the next run.", false, false, approveMergeSchema()),
		tool("hive_plan_baseline_approval", "Plan hosted baseline approval", "Read-only. Revalidates and returns every exact repository, hosted run/artifact, candidate set, PR/ref/diff, and numeric setup-authorizer binding required for approval.", true, false, stateSchema()),
		tool("hive_approve_baseline", "Approve hosted setup baselines", "Requires the recorded numeric setup authorizer to repeat every exact plan binding and an accountable reason. Hive durably records authority, removes its hold, rechecks specialized gates, and is the sole merger.", false, false, approveBaselineSchema()),
		tool("hive_revoke_merge_approval", "Revoke an exact merge approval", "Requires accountable operator authority. Durably revokes the active approval and records actor and reason.", false, false, reasonSchema()),
		tool("hive_retry_repair", "Resume an exact failed repair checkpoint", "Requires accountable operator authority. Resumes only a matching infrastructure or patch-engine failure without consuming another model attempt; all decisions are durable and audited.", false, false, retryRepairSchema()),
		tool("hive_plan_dispatch_recovery", "Plan ambiguous dispatch recovery", "Read-only. Exhaustively searches for the exact durable workflow correlation and returns actor-, request-, action-, and time-bound apply values only when no matching run exists.", true, false, planDispatchRecoverySchema()),
		tool("hive_recover_dispatch", "Recover an ambiguous workflow dispatch", "Requires accountable operator authority. Repeats a fresh exhaustive run search, then either authorizes one same-correlation retry or revokes the old intent so Hive can create a fresh correlation. All decisions are fail-closed and audited.", false, false, dispatchRecoverySchema()),
		tool("hive_transfer_setup_authorizer", "Transfer setup authorizer", "Requires the currently recorded human setup authorizer. Opens one exact reviewed transfer PR authorized by the old numeric actor and changes durable authority only after exact merge and default-branch verification.", false, false, authorizerTransferSchema()),
		tool("hive_set_coverage", "Set coverage depth", "Requires configuration authority. Changes testing depth without changing GitHub write authority.", false, false, valueSchema([]string{"essential", "standard", "comprehensive", "custom"})),
		tool("hive_set_automation", "Set automation authority", "Requires configuration authority. Changes issue/PR/merge authority without changing testing coverage.", false, false, valueSchema([]string{"advisory", "issues", "repair-pr", "auto-merge"})),
		tool("hive_set_issue_limit", "Set active issue limit", "Requires configuration authority. Changes the repository work-in-progress limit without changing coverage or write authority.", false, false, integerValueSchema(1, 100)),
		tool("hive_set_retry_limit", "Set repair retry limit", "Requires configuration authority. Changes the bounded per-finding model repair limit and records the change in the audit log.", false, false, integerValueSchema(1, 10)),
		tool("hive_pause", "Pause repository automation", "Requires operator authority. Signals active production contexts to cancel, waits for their exclusive lease, durably denies lifecycle writes, and stops scheduling while preserving state.", false, false, stateSchema()),
		tool("hive_resume", "Resume repository automation", "Requires operator authority. Re-enables only the previously configured automation level and restarts the persistent scheduler.", false, false, stateSchema()),
		tool("hive_upgrade", "Upgrade immutable components", "Requires setup authority. Opens a reviewed upgrade PR and preserves rollback metadata; never changes a mutable tag in place.", false, false, immutableValueSchema()),
		tool("hive_rollback", "Rollback immutable components", "Requires setup authority. Opens a reviewed rollback PR to a previously known immutable Visual Hive commit.", false, false, optionalValueSchema()),
		tool("hive_uninstall", "Uninstall Hive", "Destructive and requires explicit uninstall authority. Opens a cleanup PR, stops automation, and preserves or deletes state according to the request.", false, true, uninstallSchema()),
	}
	return tools
}

func tool(name, title, description string, readOnly, destructive bool, schema map[string]any) Tool {
	return Tool{Name: name, Title: title, Description: description, InputSchema: schema, Annotations: map[string]any{
		"readOnlyHint": readOnly, "destructiveHint": destructive, "idempotentHint": true, "openWorldHint": !readOnly,
	}}
}

func setupSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "allOf": []any{
		map[string]any{
			"if":   map[string]any{"required": []string{"runtime"}, "properties": map[string]any{"runtime": map[string]any{"const": "local"}}},
			"then": map[string]any{"properties": map[string]any{"run_interval_seconds": map[string]any{"minimum": 60, "maximum": 86400}}},
			"else": map[string]any{"properties": map[string]any{"run_interval_seconds": map[string]any{"minimum": 300, "maximum": 86400, "multipleOf": 60}}},
		},
	}, "properties": map[string]any{
		"repo":                 map[string]any{"type": "string", "pattern": `^[^/]+/[^/]+$`},
		"coverage":             map[string]any{"type": "string", "enum": []string{"essential", "standard", "comprehensive", "custom"}},
		"automation":           map[string]any{"type": "string", "enum": []string{"advisory", "issues", "repair-pr", "auto-merge"}},
		"provider":             map[string]any{"type": "string"},
		"runtime":              map[string]any{"type": "string", "enum": []string{"hosted", "local"}, "default": "hosted"},
		"visual_hive":          map[string]any{"type": "boolean", "const": true, "default": true},
		"max_active_issues":    map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		"max_repair_attempts":  map[string]any{"type": "integer", "minimum": 1, "maximum": 10},
		"auto_merge_paths":     map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}},
		"auto_merge_risks":     map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"automatic", "low", "medium", "restricted"}}},
		"start":                map[string]any{"type": "boolean", "default": true},
		"run_interval_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 86400, "default": 900},
		"state_dir":            map[string]any{"type": "string"},
	}}
}

func stateSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"state_dir": map[string]any{"type": "string"}}}
}

func runSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"state_dir":       map[string]any{"type": "string"},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 86400, "default": 2700},
	}}
}

func valueSchema(values []string) map[string]any {
	value := map[string]any{"type": "string"}
	if len(values) > 0 {
		value["enum"] = values
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"value"}, "properties": map[string]any{"state_dir": map[string]any{"type": "string"}, "value": value}}
}

func optionalValueSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"state_dir": map[string]any{"type": "string"},
		"value":     map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{40}$`},
	}}
}

func immutableValueSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"value"}, "properties": map[string]any{
		"state_dir": map[string]any{"type": "string"},
		"value":     map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{40}$`},
	}}
}

func uninstallSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "oneOf": []any{
		map[string]any{"properties": map[string]any{"cancel": map[string]any{"const": false}}},
		map[string]any{"required": []string{"cancel"}, "properties": map[string]any{"cancel": map[string]any{"const": true}, "delete_state": map[string]any{"const": false}}},
	}, "properties": map[string]any{
		"state_dir":    map[string]any{"type": "string"},
		"delete_state": map[string]any{"type": "boolean", "default": false},
		"cancel":       map[string]any{"type": "boolean", "default": false},
	}}
}

func authorizerTransferSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "oneOf": []any{
		map[string]any{"required": []string{"new_authorizer", "reason"}, "properties": map[string]any{"cancel": map[string]any{"const": false}}},
		map[string]any{"required": []string{"cancel"}, "properties": map[string]any{"cancel": map[string]any{"const": true}}},
	}, "properties": map[string]any{
		"state_dir":      map[string]any{"type": "string"},
		"new_authorizer": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"reason":         map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
		"cancel":         map[string]any{"type": "boolean", "default": false},
	}}
}

func integerValueSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"value"}, "properties": map[string]any{
		"state_dir": map[string]any{"type": "string"},
		"value":     map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum},
	}}
}

func startSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"state_dir":        map[string]any{"type": "string"},
		"interval_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 86400, "default": 900},
	}}
}

func approveMergeSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"pr_number", "head_sha", "base_sha", "diff_digest", "reason"}, "properties": map[string]any{
		"state_dir":   map[string]any{"type": "string"},
		"pr_number":   map[string]any{"type": "integer", "minimum": 1},
		"head_sha":    map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{40}$`},
		"base_sha":    map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{40}$`},
		"diff_digest": map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{64}$`},
		"reason":      map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
	}}
}

func planMergeApprovalSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"pr_number", "head_sha"}, "properties": map[string]any{
		"state_dir": map[string]any{"type": "string"},
		"pr_number": map[string]any{"type": "integer", "minimum": 1},
		"head_sha":  map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{40}$`},
	}}
}

func approveBaselineSchema() map[string]any {
	sha := map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{40}$`}
	digest := map[string]any{"type": "string", "pattern": `^[a-fA-F0-9]{64}$`}
	return map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"repository_id", "capture_run_id", "artifact_id", "pr_number", "head_sha", "base_sha", "diff_digest", "candidate_digest", "actor_id", "plan_digest", "reason"},
		"properties": map[string]any{
			"state_dir": map[string]any{"type": "string"}, "repository_id": map[string]any{"type": "string", "pattern": `^[0-9]+$`},
			"capture_run_id": map[string]any{"type": "integer", "minimum": 1}, "artifact_id": map[string]any{"type": "integer", "minimum": 1},
			"pr_number": map[string]any{"type": "integer", "minimum": 1}, "head_sha": sha, "base_sha": sha,
			"diff_digest": digest, "candidate_digest": digest, "actor_id": map[string]any{"type": "integer", "minimum": 1},
			"plan_digest": digest, "reason": map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
		},
	}
}

func reasonSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"reason"}, "properties": map[string]any{
		"state_dir": map[string]any{"type": "string"},
		"reason":    map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
	}}
}

func retryRepairSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"finding", "recurrence", "attempt", "failure_class", "failure_id", "reason"}, "properties": map[string]any{
		"state_dir":     map[string]any{"type": "string"},
		"finding":       map[string]any{"type": "string", "minLength": 1},
		"recurrence":    map[string]any{"type": "integer", "minimum": 0},
		"attempt":       map[string]any{"type": "integer", "minimum": 1},
		"failure_class": map[string]any{"type": "string", "enum": []string{"infrastructure", "patch_engine"}},
		"failure_id":    map[string]any{"type": "string", "minLength": 1},
		"reason":        map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
	}}
}

func planDispatchRecoverySchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"action", "correlation"}, "properties": map[string]any{
		"state_dir":   map[string]any{"type": "string"},
		"action":      map[string]any{"type": "string", "enum": []string{"retry", "revoke"}},
		"correlation": map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`},
	}}
}

func dispatchRecoverySchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"action", "correlation", "request_digest", "plan_digest", "planned_at", "reason"}, "properties": map[string]any{
		"state_dir":      map[string]any{"type": "string"},
		"action":         map[string]any{"type": "string", "enum": []string{"retry", "revoke"}},
		"correlation":    map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`},
		"request_digest": map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`},
		"plan_digest":    map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`},
		"planned_at":     map[string]any{"type": "string", "format": "date-time"},
		"reason":         map[string]any{"type": "string", "minLength": 1, "maxLength": 2048},
	}}
}

func knownTool(name string) bool {
	_, found := findTool(name)
	return found
}

func findTool(name string) (Tool, bool) {
	for _, tool := range Tools() {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

func validateToolArguments(tool Tool, arguments map[string]any) error {
	if arguments == nil {
		arguments = map[string]any{}
	}
	properties, _ := tool.InputSchema["properties"].(map[string]any)
	for _, required := range schemaStringSlice(tool.InputSchema["required"]) {
		if _, exists := arguments[required]; !exists {
			return fmt.Errorf("missing required argument %q", required)
		}
	}
	for name, value := range arguments {
		raw, exists := properties[name]
		if !exists {
			return fmt.Errorf("unknown argument %q", name)
		}
		property, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("argument %q has an invalid server schema", name)
		}
		if err := validateToolArgument(name, value, property); err != nil {
			return err
		}
	}
	return nil
}

func validateToolArgument(name string, value any, schema map[string]any) error {
	switch schema["type"] {
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("argument %q must be a string", name)
		}
		if minimum, ok := schemaNumber(schema["minLength"]); ok && float64(len([]rune(text))) < minimum {
			return fmt.Errorf("argument %q is shorter than %.0f characters", name, minimum)
		}
		if maximum, ok := schemaNumber(schema["maxLength"]); ok && float64(len([]rune(text))) > maximum {
			return fmt.Errorf("argument %q is longer than %.0f characters", name, maximum)
		}
		if pattern, ok := schema["pattern"].(string); ok {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("argument %q has an invalid server pattern", name)
			}
			if !compiled.MatchString(text) {
				return fmt.Errorf("argument %q does not match %s", name, pattern)
			}
		}
		if values := schemaStringSlice(schema["enum"]); len(values) > 0 && !containsSchemaString(values, text) {
			return fmt.Errorf("argument %q must be one of %s", name, strings.Join(values, ", "))
		}
		if schema["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
				return fmt.Errorf("argument %q must be an RFC3339 timestamp", name)
			}
		}
	case "integer":
		number, ok := schemaNumber(value)
		if !ok || math.Trunc(number) != number {
			return fmt.Errorf("argument %q must be an integer", name)
		}
		if minimum, ok := schemaNumber(schema["minimum"]); ok && number < minimum {
			return fmt.Errorf("argument %q must be at least %.0f", name, minimum)
		}
		if maximum, ok := schemaNumber(schema["maximum"]); ok && number > maximum {
			return fmt.Errorf("argument %q must be at most %.0f", name, maximum)
		}
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return fmt.Errorf("argument %q must be a boolean", name)
		}
		if expected, hasConst := schema["const"].(bool); hasConst && boolean != expected {
			return fmt.Errorf("argument %q must be %t", name, expected)
		}
	case "array":
		values, ok := schemaArrayValues(value)
		if !ok {
			return fmt.Errorf("argument %q must be an array", name)
		}
		if minimum, ok := schemaNumber(schema["minItems"]); ok && float64(len(values)) < minimum {
			return fmt.Errorf("argument %q must contain at least %.0f items", name, minimum)
		}
		if maximum, ok := schemaNumber(schema["maxItems"]); ok && float64(len(values)) > maximum {
			return fmt.Errorf("argument %q must contain at most %.0f items", name, maximum)
		}
		itemSchema, ok := schema["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("argument %q has no valid item schema", name)
		}
		seen := map[string]bool{}
		for index, item := range values {
			if err := validateToolArgument(fmt.Sprintf("%s[%d]", name, index), item, itemSchema); err != nil {
				return err
			}
			if schema["uniqueItems"] == true {
				key := fmt.Sprintf("%T:%v", item, item)
				if seen[key] {
					return fmt.Errorf("argument %q must contain unique items", name)
				}
				seen[key] = true
			}
		}
	default:
		return fmt.Errorf("argument %q has unsupported server schema type %q", name, schema["type"])
	}
	return nil
}

func schemaArrayValues(value any) ([]any, bool) {
	switch values := value.(type) {
	case []any:
		return values, true
	case []string:
		result := make([]any, len(values))
		for index, item := range values {
			result[index] = item
		}
		return result, true
	default:
		return nil, false
	}
}

func schemaNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

func schemaStringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func containsSchemaString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
