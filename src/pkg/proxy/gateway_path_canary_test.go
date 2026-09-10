package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// This file is the hermetic canary for #6515: the OpenAI-compatible gateway
// path (vllm/llm-d/litellm/watsonx/named Model Gateways) has no CI coverage
// that proves the Anthropic->OpenAI translator round-trips correctly for a
// NAMED CUSTOM gateway authenticated via api_key_file — the credential shape
// operators actually use for a private/self-hosted endpoint (docs/inference-
// backends.md), as opposed to the litellm/vllm-only cases already covered by
// inference_integration_test.go and inference_auth_test.go. It drives the
// real translator (forwardToInference) against an httptest fake
// OpenAI-compatible endpoint — no network, no sleeps.

// gatewayCanaryUpstreamRequest captures the exact shape of the request the
// translator sends upstream, so assertions can check model passthrough and
// message-role mapping instead of just the response.
type gatewayCanaryUpstreamRequest struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// startGatewayCanaryUpstream mimics a named, api_key_file-authenticated custom
// gateway (e.g. a corp LiteLLM/vLLM deployment configured as `kind: custom` in
// governor.gateways). It records the last request it saw and the Authorization
// header presented, then returns a canned completion.
func startGatewayCanaryUpstream(t *testing.T, wantAuth string, gotReq *gatewayCanaryUpstreamRequest, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		*gotAuth = r.Header.Get("Authorization")
		if wantAuth != "" && *gotAuth != wantAuth {
			http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "chatcmpl-gateway-canary",
			"choices": []map[string]interface{}{
				{
					"message":       map[string]string{"role": "assistant", "content": "canary response from custom gateway"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 4},
		})
	}))
}

// TestGatewayPathCanary_NamedGatewayAPIKeyFileRequestShape is the hermetic
// canary for issue #6515: it resolves a NAMED custom gateway's key from
// api_key_file exactly the way cmd/hive/main.go's inference-route callback
// does (config.GatewayConfig.ResolveAPIKey), builds the InferenceRoute the
// same shape production code builds it, and drives it through the real
// translator against a fake OpenAI-compatible endpoint. It asserts: the
// resolved file key reaches the upstream Authorization header, the model is
// passed through verbatim, multi-turn messages (system + user + assistant +
// user) map to the correct OpenAI roles/content in order, and the upstream
// response maps back to the Anthropic response shape.
func TestGatewayPathCanary_NamedGatewayAPIKeyFileRequestShape(t *testing.T) {
	secretsDir := t.TempDir()
	restore := config.SetSecretFileRootsForTest(secretsDir)
	defer restore()

	keyPath := filepath.Join(secretsDir, "corp_gateway_api_key")
	const wantKey = "sk-corp-gateway-canary-key"
	if err := os.WriteFile(keyPath, []byte(wantKey+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	// A named custom gateway, as an adopter would configure under
	// governor.gateways for a self-hosted endpoint — kind "custom", auth via
	// api_key_file rather than api_key_env.
	gw := config.GatewayConfig{
		Name:         "corp-gateway",
		Kind:         config.GatewayKindCustom,
		APIKeyFile:   keyPath,
		DefaultModel: "corp-llama-70b",
	}

	resolvedKey := gw.ResolveAPIKey()
	if resolvedKey != wantKey {
		t.Fatalf("gw.ResolveAPIKey() = %q, want %q (api_key_file resolution is broken)", resolvedKey, wantKey)
	}

	var gotReq gatewayCanaryUpstreamRequest
	var gotAuth string
	mock := startGatewayCanaryUpstream(t, "Bearer "+wantKey, &gotReq, &gotAuth)
	defer mock.Close()

	// Mirrors the InferenceRoute shape cmd/hive/main.go builds for a named
	// gateway resolved via ResolveGateway(backend) in the inference callback.
	route := &InferenceRoute{
		Backend:  gw.Name,
		Endpoint: mock.URL,
		Model:    gw.DefaultModel,
		APIKey:   resolvedKey,
	}

	anthropicBody := `{
		"model": "claude-opus-4-6",
		"max_tokens": 512,
		"system": "You are a helpful coding assistant.",
		"messages": [
			{"role": "user", "content": "List the files in this repo."},
			{"role": "assistant", "content": "Sure, running ls now."},
			{"role": "user", "content": "Thanks, now summarize them."}
		],
		"stream": false
	}`

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(anthropicBody), w, route, "test-agent"); err != nil {
		t.Fatalf("forwardToInference: %v", err)
	}

	// --- Auth header from the resolved api_key_file value. ---
	if gotAuth != "Bearer "+wantKey {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer "+wantKey)
	}

	// --- Model passthrough: the gateway's own model id, not the Anthropic one. ---
	if gotReq.Model != "corp-llama-70b" {
		t.Errorf("upstream model = %q, want %q (model passthrough broken)", gotReq.Model, "corp-llama-70b")
	}

	// --- Message mapping: system + 3 turns, in order, roles preserved. Named
	// gateway backends (anything but the literal "litellm" backend name) get
	// the agentic DefaultInferencePreamble prepended to the system message
	// (resolveInferencePreamble) — the CLI's own system text must still
	// survive intact as a suffix of that combined message.
	wantMessages := []struct {
		role           string
		contentSuffix  string
		exactNonSystem string
	}{
		{role: "system", contentSuffix: "You are a helpful coding assistant."},
		{role: "user", exactNonSystem: "List the files in this repo."},
		{role: "assistant", exactNonSystem: "Sure, running ls now."},
		{role: "user", exactNonSystem: "Thanks, now summarize them."},
	}
	if len(gotReq.Messages) != len(wantMessages) {
		t.Fatalf("upstream messages = %d, want %d: %+v", len(gotReq.Messages), len(wantMessages), gotReq.Messages)
	}
	for i, want := range wantMessages {
		got := gotReq.Messages[i]
		if got.Role != want.role {
			t.Errorf("message[%d].Role = %q, want %q", i, got.Role, want.role)
		}
		if want.role == "system" {
			if !strings.HasSuffix(got.Content, want.contentSuffix) {
				t.Errorf("message[%d].Content = %q, want suffix %q", i, got.Content, want.contentSuffix)
			}
		} else if got.Content != want.exactNonSystem {
			t.Errorf("message[%d].Content = %q, want %q", i, got.Content, want.exactNonSystem)
		}
	}

	// --- Response mapping back to the Anthropic shape. ---
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if ar.Type != "message" || ar.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", ar.Type, ar.Role)
	}
	if len(ar.Content) == 0 || !strings.Contains(ar.Content[0].Text, "canary response from custom gateway") {
		t.Errorf("content = %+v, want canary response text", ar.Content)
	}
	if ar.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", ar.StopReason)
	}
	if ar.Usage == nil || ar.Usage.InputTokens != 7 || ar.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want {7 4}", ar.Usage)
	}
}

// TestGatewayPathCanary_NamedGatewayWrongKeyFails404sTheGatewayRoute proves the
// canary is not vacuously green: an unresolved/incorrect key must actually be
// rejected end-to-end (401 surfaced back to the Anthropic-shaped caller),
// not silently swallowed by the translator.
func TestGatewayPathCanary_NamedGatewayWrongKeyFails404sTheGatewayRoute(t *testing.T) {
	secretsDir := t.TempDir()
	restore := config.SetSecretFileRootsForTest(secretsDir)
	defer restore()

	keyPath := filepath.Join(secretsDir, "corp_gateway_api_key")
	if err := os.WriteFile(keyPath, []byte("sk-corp-gateway-canary-key"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	gw := config.GatewayConfig{Name: "corp-gateway", Kind: config.GatewayKindCustom, APIKeyFile: keyPath}

	var gotReq gatewayCanaryUpstreamRequest
	var gotAuth string
	mock := startGatewayCanaryUpstream(t, "Bearer sk-corp-gateway-canary-key", &gotReq, &gotAuth)
	defer mock.Close()

	route := &InferenceRoute{
		Backend:  gw.Name,
		Endpoint: mock.URL,
		Model:    "corp-llama-70b",
		APIKey:   "sk-wrong-key", // deliberately does not match the file
	}

	anthropicBody := `{"model":"claude-opus-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(anthropicBody), w, route, "test-agent"); err != nil {
		t.Fatalf("forwardToInference: %v", err)
	}
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through from the gateway", w.Result().StatusCode)
	}
}

// TestGatewayPathCanary_StreamingNamedGatewayMapsUsageAndDeltas covers the
// streaming half of the gap: a named custom gateway that streams SSE chunks
// must still map deltas and the terminal usage block back into Anthropic SSE
// events, with the model passed straight through in the request.
func TestGatewayPathCanary_StreamingNamedGatewayMapsUsageAndDeltas(t *testing.T) {
	secretsDir := t.TempDir()
	restore := config.SetSecretFileRootsForTest(secretsDir)
	defer restore()

	keyPath := filepath.Join(secretsDir, "corp_gateway_api_key")
	const wantKey = "sk-corp-gateway-stream-key"
	if err := os.WriteFile(keyPath, []byte(wantKey), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	gw := config.GatewayConfig{Name: "corp-gateway-stream", Kind: config.GatewayKindCustom, APIKeyFile: keyPath}
	resolvedKey := gw.ResolveAPIKey()

	var gotModel string
	var gotStream bool
	var gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotModel, gotStream = req.Model, req.Stream

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, c := range []string{"Streaming ", "from ", "gateway"} {
			data, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": c}, "finish_reason": nil},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
		final, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{{"delta": map[string]string{}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 3},
		})
		fmt.Fprintf(w, "data: %s\n\n", final)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer mock.Close()

	route := &InferenceRoute{
		Backend:  gw.Name,
		Endpoint: mock.URL,
		Model:    "corp-llama-70b-stream",
		APIKey:   resolvedKey,
	}

	anthropicBody := `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"stream please"}],"stream":true}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(anthropicBody), w, route, "test-agent"); err != nil {
		t.Fatalf("forwardToInference: %v", err)
	}

	if gotAuth != "Bearer "+wantKey {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer "+wantKey)
	}
	if gotModel != "corp-llama-70b-stream" {
		t.Errorf("upstream model = %q, want corp-llama-70b-stream", gotModel)
	}
	if !gotStream {
		t.Errorf("upstream stream flag = false, want true")
	}

	body := w.Body.String()
	for _, evt := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, evt) {
			t.Errorf("missing SSE event %q in output:\n%s", evt, body)
		}
	}
	if !strings.Contains(body, "Streaming ") || !strings.Contains(body, "gateway") {
		t.Errorf("missing streamed delta text in output:\n%s", body)
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Errorf("missing end_turn stop reason in output:\n%s", body)
	}
}
