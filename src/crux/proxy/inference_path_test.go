package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// inferencePathCases is the method/path table shared by the MITM reroute and
// the HTTP translator tests: which requests reach the backend, and how the
// rest are answered locally.
var inferencePathCases = []struct {
	name      string
	method    string
	target    string
	body      string
	forwarded bool
	status    int
	check     func(t *testing.T, body []byte)
}{
	{
		name:      "messages is translated and forwarded",
		method:    "POST",
		target:    "/v1/messages",
		body:      `{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		forwarded: true,
		status:    http.StatusOK,
		check: func(t *testing.T, body []byte) {
			var ar anthropicResponse
			if err := json.Unmarshal(body, &ar); err != nil || ar.Type != "message" {
				t.Fatalf("expected translated message response, got %s (err=%v)", body, err)
			}
		},
	},
	{
		name:      "messages with query string is still forwarded",
		method:    "POST",
		target:    "/v1/messages?beta=true",
		body:      `{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		forwarded: true,
		status:    http.StatusOK,
	},
	{
		name:      "count_tokens is answered locally with an estimate",
		method:    "POST",
		target:    "/v1/messages/count_tokens",
		body:      `{"model":"claude","system":"You are terse.","messages":[{"role":"user","content":"` + strings.Repeat("word ", 200) + `"}]}`,
		forwarded: false,
		status:    http.StatusOK,
		check: func(t *testing.T, body []byte) {
			var ct struct {
				InputTokens int `json:"input_tokens"`
			}
			if err := json.Unmarshal(body, &ct); err != nil {
				t.Fatalf("count_tokens body %s: %v", body, err)
			}
			if ct.InputTokens <= 0 {
				t.Fatalf("input_tokens = %d, want > 0", ct.InputTokens)
			}
		},
	},
	{
		name:      "event logging telemetry is swallowed",
		method:    "POST",
		target:    "/api/event_logging/batch",
		body:      `{"events":[{"event_type":"tengu_api_success"}]}`,
		forwarded: false,
		status:    http.StatusOK,
		check: func(t *testing.T, body []byte) {
			if strings.TrimSpace(string(body)) != "{}" {
				t.Fatalf("telemetry body = %s, want {}", body)
			}
		},
	},
	{
		name:      "cli feedback is swallowed",
		method:    "POST",
		target:    "/api/claude_cli_feedback",
		body:      `{"feedback":"..."}`,
		forwarded: false,
		status:    http.StatusOK,
	},
	{
		name:      "unknown path is a local not_found_error",
		method:    "GET",
		target:    "/v1/organizations/me",
		body:      "",
		forwarded: false,
		status:    http.StatusNotFound,
		check: func(t *testing.T, body []byte) {
			var e struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &e); err != nil {
				t.Fatalf("404 body %s: %v", body, err)
			}
			if e.Type != "error" || e.Error.Type != "not_found_error" || !strings.Contains(e.Error.Message, "/v1/organizations/me") {
				t.Fatalf("unexpected 404 envelope: %s", body)
			}
		},
	},
	{
		name:      "GET on messages is not an inference call",
		method:    "GET",
		target:    "/v1/messages",
		body:      "",
		forwarded: false,
		status:    http.StatusNotFound,
	},
}

// countingVLLM wraps the mock vLLM so tests can assert whether a request
// reached the backend at all.
func countingVLLM(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	inner := startMockVLLM(t)
	t.Cleanup(inner.Close)
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		req, _ := http.NewRequest(r.Method, inner.URL+r.URL.RequestURI(), r.Body)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(outer.Close)
	return outer, &hits
}

// The MITM reroute (handleInferenceRequest) forwards only POST /v1/messages;
// telemetry, count_tokens, and unknown paths are answered locally and never
// reach the OpenAI-compatible backend.
func TestHandleInferenceRequest_PathAware(t *testing.T) {
	upstream, hits := countingVLLM(t)

	for _, tc := range inferencePathCases {
		t.Run(tc.name, func(t *testing.T) {
			p := newInferenceTestProxy()
			route := &InferenceRoute{Backend: "vllm", Endpoint: upstream.URL, Model: "test-model"}
			before := hits.Load()

			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			go func() {
				req, _ := http.NewRequest(tc.method, "https://api.anthropic.com"+tc.target, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				p.handleInferenceRequest(serverConn, req, "agent", route)
				serverConn.Close()
			}()

			resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.status, body)
			}
			if got := hits.Load()-before > 0; got != tc.forwarded {
				t.Fatalf("forwarded to backend = %v, want %v", got, tc.forwarded)
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}
}

// The plain-HTTP translator (ANTHROPIC_BASE_URL=http://127.0.0.1:18444)
// applies the same gate.
func TestInferenceTranslatorHandler_PathAware(t *testing.T) {
	upstream, hits := countingVLLM(t)

	for _, tc := range inferencePathCases {
		t.Run(tc.name, func(t *testing.T) {
			p := newInferenceTestProxy()
			p.inference.Set("agent", &InferenceRoute{Backend: "vllm", Endpoint: upstream.URL, Model: "test-model"})
			before := hits.Load()

			req := httptest.NewRequest(tc.method, "http://127.0.0.1:18444"+tc.target, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", "sk-hive-agent")
			w := httptest.NewRecorder()
			p.inferenceTranslatorHandler().ServeHTTP(w, req)

			body := w.Body.Bytes()
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.status, body)
			}
			if got := hits.Load()-before > 0; got != tc.forwarded {
				t.Fatalf("forwarded to backend = %v, want %v", got, tc.forwarded)
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}
}

func TestEstimateAnthropicInputTokens(t *testing.T) {
	if n := estimateAnthropicInputTokens([]byte(`{}`)); n != 1 {
		t.Fatalf("empty request = %d, want 1", n)
	}
	small := estimateAnthropicInputTokens([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	big := estimateAnthropicInputTokens([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}]}`))
	if big <= small {
		t.Fatalf("estimate not monotonic: small=%d big=%d", small, big)
	}
	if n := estimateAnthropicInputTokens([]byte("not json")); n < 1 {
		t.Fatalf("malformed body = %d, want >= 1", n)
	}
}
