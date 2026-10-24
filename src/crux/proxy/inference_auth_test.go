package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
