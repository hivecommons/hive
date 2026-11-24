package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

func TestDelegateDeciderRunnerAcceptsV1Endpoint(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
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
		cfg := &config.Config{}
		cfg.Governor.Trajectory.Endpoint = endpoint
		cfg.Governor.Trajectory.Model = "reviewer-model"
		runner := newDelegateDeciderRunner(func() *config.Config { return cfg })
		if _, err := runner.RunDeciderTurn(context.Background(), delegatedecision.DeciderProfile, "sys", "usr"); err != nil || lastPath != "/v1/chat/completions" {
			t.Errorf("endpoint %q: err %v, request path %q, want /v1/chat/completions", endpoint, err, lastPath)
		}
	}
}
