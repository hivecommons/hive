package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInferenceContextErrorResponse(t *testing.T) {
	for _, entry := range []string{"translator", "mitm"} {
		for _, streaming := range []bool{false, true} {
			name := entry
			if streaming {
				name += "/stream"
			} else {
				name += "/nonstream"
			}
			t.Run(name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = io.WriteString(w, `{"error":{"message":"`+strings.Repeat("x", 250)+`","code":"context_length_exceeded"}}`)
				}))
				defer upstream.Close()
				p := newInferenceTestProxy()
				route := &InferenceRoute{Backend: "litellm", Endpoint: upstream.URL, Model: "local"}
				body := `{"model":"local","max_tokens":8,"messages":[{"role":"user","content":"hi"}]`
				if streaming {
					body += `,"stream":true`
				}
				body += `}`
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
					go func() {
						defer server.Close()
						req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(body))
						p.handleInferenceRequest(server, req, "scanner", route)
					}()
					var err error
					resp, err = http.ReadResponse(bufio.NewReader(client), nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				defer resp.Body.Close()
				responseBody, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				status := http.StatusBadRequest
				if streaming {
					status = http.StatusOK
				}
				if resp.StatusCode != status || !strings.Contains(string(responseBody), `"type":"invalid_request_error"`) || !strings.Contains(string(responseBody), "prompt is too long") {
					t.Fatalf("unexpected response: %d %s", resp.StatusCode, responseBody)
				}
			})
		}
	}
}
