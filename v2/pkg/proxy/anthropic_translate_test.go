package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranslateAnthropicToOpenAI_StringSystem(t *testing.T) {
	body := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "Hello"}
		],
		"stream": true
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "Qwen/Qwen2.5-1.5B-Instruct", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if req.Model != "Qwen/Qwen2.5-1.5B-Instruct" {
		t.Errorf("model = %q, want Qwen/Qwen2.5-1.5B-Instruct", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", req.MaxTokens)
	}
	if !req.Stream {
		t.Error("stream should be true")
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", req.Messages[0].Role)
	}
	if req.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("system content = %q", req.Messages[0].Content)
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "Hello" {
		t.Errorf("user message = %+v", req.Messages[1])
	}
}

// TestTranslateAnthropicToOpenAI_ModelPassthrough asserts that the outbound
// OpenAI "model" is exactly the target (route) model, byte-for-byte, and that
// the model claude --bare puts in the Anthropic request body is ignored. The
// gateway checks this field for entitlement; any rewrite (dropping the
// "Azure/" prefix, dot/hyphen swaps, case changes) makes an entitled model
// 403 with "team not allowed to access model".
func TestTranslateAnthropicToOpenAI_ModelPassthrough(t *testing.T) {
	// claude --bare sends a claude-ish model in the body; it must be ignored.
	body := `{
		"model": "claude-opus-4-6",
		"max_tokens": 256,
		"messages": [{"role": "user", "content": "Hi"}]
	}`

	for _, target := range []string{
		"Azure/gpt-4o",
		"Azure/gpt-4.1",
		"Azure/gpt-4",
		"gpt-4o-2024-08-06",
		"aws/anthropic.claude-3-5-sonnet",
		"Qwen/Qwen2.5-1.5B-Instruct",
	} {
		result, err := translateAnthropicToOpenAI([]byte(body), target, 0, "")
		if err != nil {
			t.Fatalf("target %q: %v", target, err)
		}
		var req openaiRequest
		if err := json.Unmarshal(result, &req); err != nil {
			t.Fatalf("target %q: %v", target, err)
		}
		if req.Model != target {
			t.Errorf("outbound model = %q, want verbatim %q", req.Model, target)
		}
	}
}

func TestTranslateAnthropicToOpenAI_BlockSystem(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 512,
		"system": [{"type": "text", "text": "Be concise."}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hi"}]}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Content != "Be concise." {
		t.Errorf("system = %q, want 'Be concise.'", req.Messages[0].Content)
	}
	if req.Messages[1].Content != "Hi" {
		t.Errorf("user = %q, want 'Hi'", req.Messages[1].Content)
	}
}

func TestTranslateAnthropicToOpenAI_WithTools(t *testing.T) {
	body := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "What's the weather?"}],
		"tools": [
			{
				"name": "get_weather",
				"description": "Get weather for a location",
				"input_schema": {"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}
			}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Type != "function" {
		t.Errorf("tool type = %q, want function", req.Tools[0].Type)
	}
	if req.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", req.Tools[0].Function.Name)
	}
	if req.Tools[0].Function.Description != "Get weather for a location" {
		t.Errorf("tool description = %q", req.Tools[0].Function.Description)
	}
}

func TestTranslateAnthropicToOpenAI_ToolUseInAssistantMessage(t *testing.T) {
	body := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "What's the weather in SF?"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me check."},
				{"type": "tool_use", "id": "toolu_123", "name": "get_weather", "input": {"location": "San Francisco"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_123", "content": "72°F and sunny"}
			]}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("messages count = %d, want 3", len(req.Messages))
	}

	// First: user message
	if req.Messages[0].Role != "user" {
		t.Errorf("msg[0] role = %q, want user", req.Messages[0].Role)
	}

	// Second: assistant with tool_calls
	assistantMsg := req.Messages[1]
	if assistantMsg.Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", assistantMsg.Role)
	}
	if assistantMsg.Content != "Let me check." {
		t.Errorf("msg[1] content = %q, want 'Let me check.'", assistantMsg.Content)
	}
	if len(assistantMsg.ToolCalls) != 1 {
		t.Fatalf("msg[1] tool_calls count = %d, want 1", len(assistantMsg.ToolCalls))
	}
	if assistantMsg.ToolCalls[0].ID != "toolu_123" {
		t.Errorf("tool_call id = %q, want toolu_123", assistantMsg.ToolCalls[0].ID)
	}
	if assistantMsg.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call name = %q, want get_weather", assistantMsg.ToolCalls[0].Function.Name)
	}

	// Third: tool result
	toolMsg := req.Messages[2]
	if toolMsg.Role != "tool" {
		t.Errorf("msg[2] role = %q, want tool", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "toolu_123" {
		t.Errorf("msg[2] tool_call_id = %q, want toolu_123", toolMsg.ToolCallID)
	}
	if toolMsg.Content != "72°F and sunny" {
		t.Errorf("msg[2] content = %q, want '72°F and sunny'", toolMsg.Content)
	}
}

func TestTranslateAnthropicToOpenAI_ToolResultWithBlocks(t *testing.T) {
	body := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_456", "content": [{"type": "text", "text": "result data"}]}
			]}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "tool" {
		t.Errorf("role = %q, want tool", req.Messages[0].Role)
	}
	if req.Messages[0].Content != "result data" {
		t.Errorf("content = %q, want 'result data'", req.Messages[0].Content)
	}
}

func TestTranslateOpenAIResponseToAnthropic(t *testing.T) {
	resp := `{
		"id": "chatcmpl-001",
		"choices": [{
			"message": {"content": "Hello there!"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 3}
	}`

	result, err := translateOpenAIResponseToAnthropic([]byte(resp), "test-model")
	if err != nil {
		t.Fatal(err)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(result, &ar); err != nil {
		t.Fatal(err)
	}

	if ar.Type != "message" {
		t.Errorf("type = %q, want message", ar.Type)
	}
	if ar.Role != "assistant" {
		t.Errorf("role = %q, want assistant", ar.Role)
	}
	if ar.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", ar.StopReason)
	}
	if len(ar.Content) != 1 || ar.Content[0].Text != "Hello there!" {
		t.Errorf("content = %+v", ar.Content)
	}
	if ar.Usage == nil || ar.Usage.InputTokens != 10 || ar.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", ar.Usage)
	}
}

func TestTranslateOpenAIResponseToAnthropic_WithToolCalls(t *testing.T) {
	resp := `{
		"id": "chatcmpl-002",
		"choices": [{
			"message": {
				"content": "Let me look that up.",
				"tool_calls": [
					{
						"id": "call_abc",
						"type": "function",
						"function": {
							"name": "get_weather",
							"arguments": "{\"location\":\"SF\"}"
						}
					}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 15, "completion_tokens": 8}
	}`

	result, err := translateOpenAIResponseToAnthropic([]byte(resp), "test-model")
	if err != nil {
		t.Fatal(err)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(result, &ar); err != nil {
		t.Fatal(err)
	}

	if ar.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", ar.StopReason)
	}
	if len(ar.Content) != 2 {
		t.Fatalf("content count = %d, want 2 (text + tool_use)", len(ar.Content))
	}
	if ar.Content[0].Type != "text" || ar.Content[0].Text != "Let me look that up." {
		t.Errorf("content[0] = %+v", ar.Content[0])
	}
	if ar.Content[1].Type != "tool_use" {
		t.Errorf("content[1].type = %q, want tool_use", ar.Content[1].Type)
	}
	if ar.Content[1].ID != "call_abc" {
		t.Errorf("content[1].id = %q, want call_abc", ar.Content[1].ID)
	}
	if ar.Content[1].Name != "get_weather" {
		t.Errorf("content[1].name = %q, want get_weather", ar.Content[1].Name)
	}

	var input map[string]string
	if err := json.Unmarshal(ar.Content[1].Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["location"] != "SF" {
		t.Errorf("input.location = %q, want SF", input["location"])
	}
}

func TestTranslateOpenAISSEToAnthropic(t *testing.T) {
	sseInput := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var buf strings.Builder
	_, _, err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	output := buf.String()

	if !strings.Contains(output, "event: message_start") {
		t.Error("missing message_start event")
	}
	if !strings.Contains(output, "event: content_block_start") {
		t.Error("missing content_block_start event")
	}
	if !strings.Contains(output, `"text_delta"`) {
		t.Error("missing text_delta in output")
	}
	if !strings.Contains(output, `"Hello"`) {
		t.Error("missing 'Hello' text delta")
	}
	if !strings.Contains(output, `" world"`) {
		t.Error("missing ' world' text delta")
	}
	if !strings.Contains(output, "event: content_block_stop") {
		t.Error("missing content_block_stop event")
	}
	if !strings.Contains(output, "event: message_delta") {
		t.Error("missing message_delta event")
	}
	if !strings.Contains(output, `"end_turn"`) {
		t.Error("missing end_turn stop reason")
	}
	if !strings.Contains(output, "event: message_stop") {
		t.Error("missing message_stop event")
	}
}

func TestTranslateOpenAISSEToAnthropic_ToolCalls(t *testing.T) {
	sseInput := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Checking"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_xyz","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loc"}}]},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ation\":\"NYC\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var buf strings.Builder
	_, _, err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	output := buf.String()

	if !strings.Contains(output, `"tool_use"`) {
		t.Error("missing tool_use content block")
	}
	if !strings.Contains(output, `"get_weather"`) {
		t.Error("missing tool name get_weather")
	}
	if !strings.Contains(output, `"call_xyz"`) {
		t.Error("missing tool call id")
	}
	if !strings.Contains(output, `"input_json_delta"`) {
		t.Error("missing input_json_delta events")
	}
	if !strings.Contains(output, `"tool_use"`) {
		t.Error("missing tool_use stop reason")
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"unknown", "end_turn"},
		{"", "end_turn"},
	}
	for _, tt := range tests {
		got := mapFinishReason(tt.input)
		if got != tt.want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractTextFromContent_String(t *testing.T) {
	got := extractTextFromContent(json.RawMessage(`"plain text"`))
	if got != "plain text" {
		t.Errorf("got %q, want 'plain text'", got)
	}
}

func TestExtractTextFromContent_Blocks(t *testing.T) {
	got := extractTextFromContent(json.RawMessage(`[{"type":"text","text":"hello"},{"type":"text","text":" world"}]`))
	if got != "hello world" {
		t.Errorf("got %q, want 'hello world'", got)
	}
}

func TestTranslateAnthropicToOpenAI_MixedTextAndToolResultInUser(t *testing.T) {
	body := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "Here's the result:"},
				{"type": "tool_result", "tool_use_id": "toolu_789", "content": "42"}
			]}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2 (text user + tool)", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("msg[0] role = %q, want user", req.Messages[0].Role)
	}
	if req.Messages[0].Content != "Here's the result:" {
		t.Errorf("msg[0] content = %q", req.Messages[0].Content)
	}
	if req.Messages[1].Role != "tool" {
		t.Errorf("msg[1] role = %q, want tool", req.Messages[1].Role)
	}
	if req.Messages[1].Content != "42" {
		t.Errorf("msg[1] content = %q, want '42'", req.Messages[1].Content)
	}
}

func TestTranslateAnthropicToOpenAI_InferencePreamble(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"system": "You are Claude Code, Anthropic's official CLI for Claude.CWD: /data/agents/scanner\nDate: 2026-06-23",
		"messages": [
			{"role": "user", "content": "Check CI status"}
		]
	}`

	preamble := "You are an autonomous agent.\n\n"

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, preamble)
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(req.Messages))
	}
	sys := req.Messages[0]
	if sys.Role != "system" {
		t.Errorf("first message role = %q, want system", sys.Role)
	}
	if !strings.HasPrefix(sys.Content, preamble) {
		t.Errorf("system prompt should start with preamble, got: %q", sys.Content[:80])
	}
	if !strings.Contains(sys.Content, "CWD: /data/agents/scanner") {
		t.Error("original system content (CWD) should be preserved after preamble")
	}
}

func TestTranslateAnthropicToOpenAI_NoPreamble(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"system": "You are Claude Code.",
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if req.Messages[0].Content != "You are Claude Code." {
		t.Errorf("system prompt should be unchanged when preamble is empty, got: %q", req.Messages[0].Content)
	}
}

func TestTranslateAnthropicToOpenAI_PreambleNoSystem(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`

	preamble := "You are an autonomous agent.\n"

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model", 0, preamble)
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2 (system + user)", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", req.Messages[0].Role)
	}
	if req.Messages[0].Content != preamble {
		t.Errorf("system content = %q, want preamble", req.Messages[0].Content)
	}
}

func TestTranslateAnthropicToOpenAI_DefaultToolInjection(t *testing.T) {
	// When preamble is set and CLI sends no tools, default tools should be injected.
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 200,
		"messages": [{"role": "user", "content": "List files"}]
	}`
	preamble := "You are an agent.\n\n"

	result, err := translateAnthropicToOpenAI([]byte(body), "qwen-72b", 0, preamble)
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Tools) != len(DefaultInferenceTools) {
		t.Fatalf("tools count = %d, want %d (default tools injected)", len(req.Tools), len(DefaultInferenceTools))
	}

	// Verify specific tools are present.
	toolNames := make(map[string]bool)
	for _, tool := range req.Tools {
		toolNames[tool.Function.Name] = true
	}
	for _, expected := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep"} {
		if !toolNames[expected] {
			t.Errorf("missing expected tool %q in injected tools", expected)
		}
	}

	// tool_choice should be "auto".
	var tc string
	if err := json.Unmarshal(req.ToolChoice, &tc); err != nil {
		t.Fatal(err)
	}
	if tc != "auto" {
		t.Errorf("tool_choice = %q, want auto", tc)
	}
}

func TestTranslateAnthropicToOpenAI_NoToolInjectionWithoutPreamble(t *testing.T) {
	// When preamble is empty (non-inference path), no tools should be injected.
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 200,
		"messages": [{"role": "user", "content": "Hello"}]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "claude-sonnet-4-6", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Tools) != 0 {
		t.Errorf("tools count = %d, want 0 (no injection without preamble)", len(req.Tools))
	}
}

func TestTranslateAnthropicToOpenAI_CLIToolsPreserved(t *testing.T) {
	// When CLI sends its own tools, they should be used (not replaced with defaults).
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 200,
		"messages": [{"role": "user", "content": "Hello"}],
		"tools": [{"name": "MyCustomTool", "description": "Custom tool", "input_schema": {"type": "object"}}]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "qwen-72b", 0, "preamble\n")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	if err := json.Unmarshal(result, &req); err != nil {
		t.Fatal(err)
	}

	if len(req.Tools) != 1 {
		t.Fatalf("tools count = %d, want 1 (CLI tools preserved)", len(req.Tools))
	}
	if req.Tools[0].Function.Name != "MyCustomTool" {
		t.Errorf("tool name = %q, want MyCustomTool", req.Tools[0].Function.Name)
	}
}
