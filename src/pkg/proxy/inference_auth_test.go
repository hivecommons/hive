package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startMockAuthedUpstream mimics a LiteLLM proxy: /v1/chat/completions
// requires "Authorization: Bearer <wantKey>" and returns 401 otherwise.
// The Authorization header seen on the last request is recorded in gotAuth.
func startMockAuthedUpstream(t *testing.T, wantKey string, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		if *gotAuth != "Bearer "+wantKey {
			http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "chatcmpl-authed",
			"choices": []map[string]interface{}{
				{
					"message":       map[string]string{"role": "assistant", "content": "authed response"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 2},
		})
	}))
}

func TestForwardToInference_BearerAuthReachesUpstream(t *testing.T) {
	var gotAuth string
	mock := startMockAuthedUpstream(t, "sk-litellm-test", &gotAuth)
	defer mock.Close()

	anthropicBody := `{
		"model": "claude-opus-4-6",
		"max_tokens": 128,
		"messages": [{"role": "user", "content": "hi"}],
		"stream": false
	}`

	route := &InferenceRoute{
		Backend:  "litellm",
		Endpoint: mock.URL,
		Model:    "gpt-4o",
		APIKey:   "sk-litellm-test",
	}

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(anthropicBody), w, route, "test-agent"); err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Bearer sk-litellm-test" {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer sk-litellm-test")
	}
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}
	if len(ar.Content) == 0 || !strings.Contains(ar.Content[0].Text, "authed response") {
		t.Errorf("response content = %+v, want authed response text", ar.Content)
	}
}

func TestForwardToInference_MissingKeyGets401(t *testing.T) {
	var gotAuth string
	mock := startMockAuthedUpstream(t, "sk-litellm-test", &gotAuth)
	defer mock.Close()

	anthropicBody := `{
		"model": "claude-opus-4-6",
		"max_tokens": 128,
		"messages": [{"role": "user", "content": "hi"}],
		"stream": false
	}`

	route := &InferenceRoute{
		Backend:  "litellm",
		Endpoint: mock.URL,
		Model:    "gpt-4o",
		// APIKey intentionally empty
	}

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(anthropicBody), w, route, "test-agent"); err != nil {
		t.Fatal(err)
	}

	if gotAuth != "" {
		t.Errorf("upstream Authorization = %q, want empty when route has no key", gotAuth)
	}
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through from upstream", w.Result().StatusCode)
	}
}

func TestForwardToInference_NoAuthHeaderForVLLM(t *testing.T) {
	var gotAuth string
	sawRequest := false
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "chatcmpl-open",
			"choices": []map[string]interface{}{
				{
					"message":       map[string]string{"role": "assistant", "content": "open response"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 2},
		})
	}))
	defer mock.Close()

	anthropicBody := `{
		"model": "claude-opus-4-6",
		"max_tokens": 128,
		"messages": [{"role": "user", "content": "hi"}],
		"stream": false
	}`

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test-model"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(anthropicBody), w, route, "test-agent"); err != nil {
		t.Fatal(err)
	}
	if !sawRequest {
		t.Fatal("mock upstream never received a request")
	}
	if gotAuth != "" {
		t.Errorf("upstream Authorization = %q, want none for keyless vllm route", gotAuth)
	}
}

func TestFindEndpointForModel_WithBearerKey(t *testing.T) {
	var gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer sk-models-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{{"id": "gpt-4o"}},
		})
	}))
	defer mock.Close()

	if got := FindEndpointForModel([]string{mock.URL}, "gpt-4o", "", ""); got != "" {
		t.Errorf("FindEndpointForModel without key = %q, want no match against authed endpoint", got)
	}
	if got := FindEndpointForModel([]string{mock.URL}, "gpt-4o", "sk-models-key", ""); got != mock.URL {
		t.Errorf("FindEndpointForModel with key = %q, want %q", got, mock.URL)
	}
}

func TestInferenceHTTPClient_BadCABundle(t *testing.T) {
	route := &InferenceRoute{Backend: "litellm", CABundle: "/nonexistent/ca.pem"}
	if _, err := inferenceHTTPClient(route); err == nil {
		t.Error("expected error for missing CA bundle, got nil")
	}
}

func TestInferenceHTTPClient_DefaultWithoutCABundle(t *testing.T) {
	route := &InferenceRoute{Backend: "litellm"}
	client, err := inferenceHTTPClient(route)
	if err != nil {
		t.Fatal(err)
	}
	if client != http.DefaultClient {
		t.Error("expected http.DefaultClient when no CA bundle is set")
	}
}
