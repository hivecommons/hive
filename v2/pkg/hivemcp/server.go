package hivemcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		if !knownTool(params.Name) {
			return nil, &rpcError{Code: -32602, Message: "unknown Hive tool: " + params.Name}
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
		tool("hive_run", "Run Hive now", "Requires configured run authority. Launches a complete Visual Hive scan and processes trusted evidence through Hive lifecycle policy.", false, false, stateSchema()),
		tool("hive_set_coverage", "Set coverage depth", "Requires configuration authority. Changes testing depth without changing GitHub write authority.", false, false, valueSchema([]string{"essential", "standard", "comprehensive", "custom"})),
		tool("hive_set_automation", "Set automation authority", "Requires configuration authority. Changes issue/PR/merge authority without changing testing coverage.", false, false, valueSchema([]string{"advisory", "issues", "repair-pr", "auto-merge"})),
		tool("hive_pause", "Pause repository automation", "Requires operator authority. Immediately denies repository lifecycle writes while preserving durable state.", false, false, stateSchema()),
		tool("hive_resume", "Resume repository automation", "Requires operator authority. Re-enables only the previously configured automation level.", false, false, stateSchema()),
		tool("hive_upgrade", "Upgrade immutable components", "Requires setup authority. Opens a reviewed upgrade PR and preserves rollback metadata; never changes a mutable tag in place.", false, false, valueSchema(nil)),
		tool("hive_rollback", "Rollback immutable components", "Requires setup authority. Opens a reviewed rollback PR to a previously known immutable Visual Hive commit.", false, false, valueSchema(nil)),
		tool("hive_uninstall", "Uninstall Hive", "Destructive and requires explicit uninstall authority. Opens a cleanup PR, stops automation, and preserves or deletes state according to the request.", false, true, stateSchema()),
	}
	return tools
}

func tool(name, title, description string, readOnly, destructive bool, schema map[string]any) Tool {
	return Tool{Name: name, Title: title, Description: description, InputSchema: schema, Annotations: map[string]any{
		"readOnlyHint": readOnly, "destructiveHint": destructive, "idempotentHint": true, "openWorldHint": !readOnly,
	}}
}

func setupSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"repo", "coverage", "automation"}, "properties": map[string]any{
		"repo":              map[string]any{"type": "string", "pattern": `^[^/]+/[^/]+$`},
		"coverage":          map[string]any{"type": "string", "enum": []string{"essential", "standard", "comprehensive", "custom"}},
		"automation":        map[string]any{"type": "string", "enum": []string{"advisory", "issues", "repair-pr", "auto-merge"}},
		"provider":          map[string]any{"type": "string", "default": "codex"},
		"visual_hive":       map[string]any{"type": "boolean", "default": true},
		"max_active_issues": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 5},
		"state_dir":         map[string]any{"type": "string"},
	}}
}

func stateSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"state_dir": map[string]any{"type": "string"}}}
}

func valueSchema(values []string) map[string]any {
	value := map[string]any{"type": "string"}
	if len(values) > 0 {
		value["enum"] = values
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"value"}, "properties": map[string]any{"state_dir": map[string]any{"type": "string"}, "value": value}}
}

func knownTool(name string) bool {
	for _, tool := range Tools() {
		if tool.Name == name {
			return true
		}
	}
	return false
}
