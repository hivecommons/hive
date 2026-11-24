package ioscan

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLLMClassifierAcceptsV1Endpoint(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	t.Cleanup(srv.Close)

	for _, endpoint := range []string{srv.URL, srv.URL + "/v1", srv.URL + "/v1/"} {
		lastPath = ""
		c, err := NewLLMClassifier(LLMClassifierConfig{Endpoint: endpoint, Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.client.Complete(context.Background(), nil, 1)
		if lastPath != "/v1/chat/completions" {
			t.Errorf("endpoint %q: request path %q, want /v1/chat/completions", endpoint, lastPath)
		}
	}
}
