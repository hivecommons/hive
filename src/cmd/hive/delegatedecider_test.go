package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

func TestDelegateDeciderRunner(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	var got delegateDeciderRequest
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"approve\"}"}}]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Governor.Trajectory.Endpoint = srv.URL
	cfg.Governor.Trajectory.Model = "reviewer-model"
	runner := newDelegateDeciderRunner(func() *config.Config { return cfg })

	reply, err := runner.RunDeciderTurn(context.Background(), delegatedecision.DeciderProfile, "sys", "usr")
	if err != nil || reply != `{"verdict":"approve"}` {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if got.Model != "reviewer-model" || len(got.Messages) != 2 || got.Messages[0].Content != "sys" || got.Messages[1].Content != "usr" || got.Temperature != 0 {
		t.Fatalf("request = %+v", got)
	}

	on := true
	cfg.Decisions.Delegate.LLM = &config.DelegateDecisionLLMConfig{Enabled: &on, Model: "decider-model"}
	if _, err := runner.RunDeciderTurn(context.Background(), delegatedecision.DeciderProfile, "sys", "usr"); err != nil || got.Model != "decider-model" {
		t.Fatalf("model override: model=%q err=%v", got.Model, err)
	}

	status = http.StatusBadGateway
	if _, err := runner.RunDeciderTurn(context.Background(), delegatedecision.DeciderProfile, "sys", "usr"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("non-200 must error, got %v", err)
	}

	empty := newDelegateDeciderRunner(func() *config.Config { return &config.Config{} })
	if _, err := empty.RunDeciderTurn(context.Background(), delegatedecision.DeciderProfile, "s", "u"); !errors.Is(err, errDelegateDeciderUnconfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
	none := newDelegateDeciderRunner(func() *config.Config { return nil })
	if _, err := none.RunDeciderTurn(context.Background(), delegatedecision.DeciderProfile, "s", "u"); !errors.Is(err, errDelegateDeciderUnconfigured) {
		t.Fatalf("nil config: %v", err)
	}
}
