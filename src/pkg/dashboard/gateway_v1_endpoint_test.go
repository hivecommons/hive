package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewayProbesAcceptV1Endpoint(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	for _, endpoint := range []string{srv.URL, srv.URL + "/v1", srv.URL + "/v1/"} {
		if err := probeLiteLLMInference(endpoint, "k", "m"); err != nil || lastPath != "/v1/chat/completions" {
			t.Errorf("probeLiteLLMInference(%q): err %v, path %q, want /v1/chat/completions", endpoint, err, lastPath)
		}
		if n, err := probeLiteLLMModels(endpoint, "k"); err != nil || n != 1 || lastPath != "/v1/models" {
			t.Errorf("probeLiteLLMModels(%q): %d models, err %v, path %q, want 1 from /v1/models", endpoint, n, err, lastPath)
		}
		if models, err := fetchModelsFromEndpoint(endpoint, "k"); err != nil || len(models) != 1 || lastPath != "/v1/models" {
			t.Errorf("fetchModelsFromEndpoint(%q): %v, err %v, path %q, want [m] from /v1/models", endpoint, models, err, lastPath)
		}
	}
}
