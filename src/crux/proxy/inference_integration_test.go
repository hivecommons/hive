package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if !IsInferenceBackend("litellm") {
		t.Error("litellm should be inference backend")
	}
	if IsInferenceBackend("claude") {
		t.Error("claude should not be inference backend")
	}
}
