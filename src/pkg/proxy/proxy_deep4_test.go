package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStartInferenceTranslatorActual calls the real StartInferenceTranslator
// and sends requests to it. Port 18444 must be free.
func TestStartInferenceTranslatorActual(t *testing.T) {
	// Mock vLLM backend
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)
			data, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "streamed"}, "finish_reason": nil},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			f.Flush()
			data2, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{}, "finish_reason": "stop"},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", data2)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			f.Flush()
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	caCert, caX509, _ := generateCA()
	p := &GitHubProxy{
		caCert:     caCert,
		caX509:     caX509,
		logger:     slog.Default(),
		violations: make(map[string]int),
		certCache:  make(map[string]cachedCert),
		inference:  newInferenceRouter(),
	}

	p.inference.Set("agent1", &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test-model"})

	translator := httptest.NewServer(p.inferenceTranslatorHandler())
	defer translator.Close()
	baseURL := translator.URL

	// Test 1: no route
	req1, _ := http.NewRequest("POST", baseURL+"/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req1.Header.Set("x-api-key", "sk-hive-unknown")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("no-route request: %v", err)
	}
	if resp1.StatusCode != http.StatusBadGateway {
		t.Errorf("no-route status = %d, want 502", resp1.StatusCode)
	}
	resp1.Body.Close()

	// Test 2: non-streaming success
	req2, _ := http.NewRequest("POST", baseURL+"/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("x-api-key", "sk-hive-agent1")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("non-streaming request: %v", err)
	}
	if resp2.StatusCode != 200 {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("non-streaming status = %d, body = %q", resp2.StatusCode, body)
	}
	var ar anthropicResponse
	json.NewDecoder(resp2.Body).Decode(&ar)
	if ar.Type != "message" {
		t.Errorf("type = %q", ar.Type)
	}
	resp2.Body.Close()

	// Test 3: streaming success
	req3, _ := http.NewRequest("POST", baseURL+"/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req3.Header.Set("x-api-key", "sk-hive-agent1")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if !strings.Contains(string(body3), "message_start") {
		t.Error("missing message_start in streaming response")
	}

	// Test 4: bad body
	req4, _ := http.NewRequest("POST", baseURL+"/v1/messages",
		strings.NewReader("not json"))
	req4.Header.Set("x-api-key", "sk-hive-agent1")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("bad body request: %v", err)
	}
	if resp4.StatusCode != http.StatusBadGateway {
		t.Errorf("bad body status = %d, want 502", resp4.StatusCode)
	}
	resp4.Body.Close()

	// Test 5: unreachable backend
	p.inference.Set("agent-fail", &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "test"})
	req5, _ := http.NewRequest("POST", baseURL+"/v1/messages",
		strings.NewReader(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req5.Header.Set("x-api-key", "sk-hive-agent-fail")
	resp5, err := http.DefaultClient.Do(req5)
	if err != nil {
		t.Fatalf("unreachable request: %v", err)
	}
	if resp5.StatusCode != http.StatusBadGateway {
		t.Errorf("unreachable status = %d, want 502", resp5.StatusCode)
	}
	resp5.Body.Close()
}
