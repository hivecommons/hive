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

	result, err := translateAnthropicToOpenAI([]byte(body), "Qwen/Qwen2.5-1.5B-Instruct")
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

func TestTranslateAnthropicToOpenAI_BlockSystem(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 512,
		"system": [{"type": "text", "text": "Be concise."}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hi"}]}
		]
	}`

	result, err := translateAnthropicToOpenAI([]byte(body), "test-model")
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
	err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "test-model")
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

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
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
