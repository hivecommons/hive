package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranslatorCanceledRequestDoesNotRecordBackendFailure(t *testing.T) {
	p := newInferenceTestProxy()
	p.inferenceLastErrors = newInferenceLastErrorState()
	p.inference.Set("scanner", &InferenceRoute{Backend: "litellm", Endpoint: "http://127.0.0.1:1", Model: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("x-api-key", "sk-hive-scanner")
	p.inferenceTranslatorHandler().ServeHTTP(httptest.NewRecorder(), req)
	if msg, _, _, ok := p.LastInferenceError("scanner"); ok {
		t.Fatalf("canceled request recorded backend failure: %s", msg)
	}
}
