package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func startMockVLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string          `json:"model"`
			Messages json.RawMessage `json:"messages"`
			Stream   bool            `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)

			chunks := []string{"Hello", " from", " vLLM!"}
			for _, c := range chunks {
				data, _ := json.Marshal(map[string]interface{}{
					"choices": []map[string]interface{}{
						{"delta": map[string]string{"content": c}, "finish_reason": nil},
					},
				})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			finalData, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{}, "finish_reason": "stop"},
				},
				"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 3},
			})
			fmt.Fprintf(w, "data: %s\n\n", finalData)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "chatcmpl-test",
			"choices": []map[string]interface{}{
				{
					"message":       map[string]string{"role": "assistant", "content": "Mock response for " + req.Model},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
}

func TestForwardToInference_NonStreaming(t *testing.T) {
	mock := startMockVLLM(t)
	defer mock.Close()

	anthropicBody := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"system": "You are a test assistant.",
		"messages": [{"role": "user", "content": "Say hello"}],
		"stream": false
	}`

	route := &InferenceRoute{
		Backend:  "vllm",
		Endpoint: mock.URL,
		Model:    "test-model",
	}

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte(anthropicBody), w, route)
	if err != nil {
		t.Fatal(err)
	}

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}

	if ar.Type != "message" {
		t.Errorf("type = %q, want message", ar.Type)
	}
	if ar.Role != "assistant" {
		t.Errorf("role = %q, want assistant", ar.Role)
	}
	if len(ar.Content) == 0 {
		t.Fatal("no content blocks")
	}
	if !strings.Contains(ar.Content[0].Text, "test-model") {
		t.Errorf("response text = %q, expected to contain 'test-model'", ar.Content[0].Text)
	}
	if ar.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", ar.StopReason)
	}
	if ar.Usage == nil {
		t.Fatal("usage is nil")
	}
	if ar.Usage.InputTokens != 10 {
		t.Errorf("input_tokens = %d, want 10", ar.Usage.InputTokens)
	}
}

func TestForwardToInference_Streaming(t *testing.T) {
	mock := startMockVLLM(t)
	defer mock.Close()

	anthropicBody := `{
		"model": "claude-opus-4-6",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hello"}],
		"stream": true
	}`

	route := &InferenceRoute{
		Backend:  "vllm",
		Endpoint: mock.URL,
		Model:    "test-model",
	}

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte(anthropicBody), w, route)
	if err != nil {
		t.Fatal(err)
	}

	body := w.Body.String()

	expectedEvents := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	for _, evt := range expectedEvents {
		if !strings.Contains(body, evt) {
			t.Errorf("missing event: %s", evt)
		}
	}

	if !strings.Contains(body, "Hello") {
		t.Error("missing 'Hello' in SSE output")
	}
	if !strings.Contains(body, "vLLM!") {
		t.Error("missing 'vLLM!' in SSE output")
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Error("missing end_turn stop reason")
	}
}

func TestInferenceRouter(t *testing.T) {
	router := newInferenceRouter()

	if got := router.Get("agent-1"); got != nil {
		t.Error("expected nil for unknown agent")
	}

	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://localhost:8000", Model: "test"}
	router.Set("agent-1", route)

	got := router.Get("agent-1")
	if got == nil {
		t.Fatal("expected route for agent-1")
	}
	if got.Backend != "vllm" {
		t.Errorf("backend = %q, want vllm", got.Backend)
	}

	router.Clear("agent-1")
	if got := router.Get("agent-1"); got != nil {
		t.Error("expected nil after clear")
	}
}

func TestIsAnthropicHost(t *testing.T) {
	if !IsAnthropicHost("api.anthropic.com") {
		t.Error("api.anthropic.com should be anthropic host")
	}
	if IsAnthropicHost("api.openai.com") {
		t.Error("api.openai.com should not be anthropic host")
	}
}

func TestIsInferenceBackend(t *testing.T) {
	_ = time.Now() // suppress unused import
	if !IsInferenceBackend("vllm") {
		t.Error("vllm should be inference backend")
	}
	if !IsInferenceBackend("llm-d") {
		t.Error("llm-d should be inference backend")
	}
	if IsInferenceBackend("claude") {
		t.Error("claude should not be inference backend")
	}
}
