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

// Both entry points must keep blank models away from the provider, expose the
// configuration remedy, and recover when a gateway default is supplied.
func TestInferenceMissingModel(t *testing.T) {
	for _, entry := range []string{"translator", "mitm"} {
		for _, tc := range []struct {
			name         string
			model        string
			defaultModel string
			wantModel    string
		}{
			{name: "empty"},
			{name: "whitespace", model: " \t", defaultModel: " \n"},
			{name: "gateway default", defaultModel: "Azure/gpt-4o", wantModel: "Azure/gpt-4o"},
			{name: "whitespace inherits default", model: " \t", defaultModel: "Azure/gpt-4o", wantModel: "Azure/gpt-4o"},
			{name: "explicit selection", model: "aws/claude-sonnet-4-6", defaultModel: "Azure/gpt-4o", wantModel: "aws/claude-sonnet-4-6"},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				var hits atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					var req openaiRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Errorf("decode upstream request: %v", err)
					}
					if req.Model == "" || req.Model != tc.wantModel {
						t.Errorf("upstream model = %q, want %q", req.Model, tc.wantModel)
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
				}))
				defer upstream.Close()
				p := newInferenceTestProxy()
				p.inferenceLastErrors = newInferenceLastErrorState()
				route := &InferenceRoute{Backend: "litellm", Endpoint: upstream.URL, Model: tc.model, DefaultModel: tc.defaultModel}
				const body = `{"model":"claude-cli-alias","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
				var resp *http.Response
				if entry == "translator" {
					p.inference.Set("scanner", route)
					req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
					req.Header.Set("x-api-key", "sk-hive-scanner")
					w := httptest.NewRecorder()
					p.inferenceTranslatorHandler().ServeHTTP(w, req)
					resp = w.Result()
				} else {
					client, server := net.Pipe()
					defer client.Close()
					done := make(chan struct{})
					go func() {
						defer close(done)
						defer server.Close()
						req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(body))
						p.handleInferenceRequest(server, req, "scanner", route)
					}()
					var err error
					resp, err = http.ReadResponse(bufio.NewReader(client), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { client.Close(); <-done }()
				}
				defer resp.Body.Close()
				responseBody, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid(responseBody) {
					t.Fatalf("invalid response JSON: %s", responseBody)
				}
				if tc.wantModel == "" {
					if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(responseBody), "no inference model configured") || !strings.Contains(string(responseBody), "default_model") {
						t.Fatalf("missing configuration response: %d %s", resp.StatusCode, responseBody)
					}
					if hits.Load() != 0 {
						t.Fatal("missing model was forwarded upstream")
					}
					msg, status, _, ok := p.LastInferenceError("scanner")
					if !ok || status != 0 || !strings.Contains(msg, "default_model") {
						t.Fatalf("missing dashboard error: %q, %d, %v", msg, status, ok)
					}
				} else if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
					t.Fatalf("configured request: status=%d hits=%d body=%s", resp.StatusCode, hits.Load(), responseBody)
				}
				if route.Model != tc.model {
					t.Fatalf("stored model mutated: %q, want %q", route.Model, tc.model)
				}
			})
		}
	}
}
