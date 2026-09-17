package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// The inference route and gateway resolvers moved to pkg/inference (#7238
// stage 3) and their tests moved with them. Those tests now call the package
// directly, which means nothing exercises the wrappers left behind here.
//
// That gap is not theoretical: a wrapper that forwards the wrong argument, or
// drops one, compiles perfectly and every relocated test still passes. Both
// mutations below survived the first mutation run of this change and are the
// reason this file exists.

// TestResolveLiteLLMRouteWrapperForwardsBackendAndModel pins that the wrapper
// passes through the two arguments that actually select a route.
//
// requestedModel becomes the route's model verbatim, and backend names the
// gateway the endpoint is read from when the legacy governor.litellm block is
// empty — the Model-Gateways-tab-only configuration that produced "502 no
// inference route" for every agent call in #5393. A wrapper that dropped
// either would resolve a route that looks fine and points at the wrong place.
func TestResolveLiteLLMRouteWrapperForwardsBackendAndModel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governor.Gateways = []config.GatewayConfig{{
		Name:     "acme-gw",
		Kind:     "openai",
		Endpoint: "https://acme.example/v1",
	}}

	endpoint, model, ok := resolveLiteLLMInferenceRoute(cfg, "acme-gw", "gpt-test-model")
	if !ok {
		t.Fatal("a gateway named by the backend must yield a route (#5393)")
	}
	if endpoint != "https://acme.example/v1" {
		t.Errorf("endpoint = %q, want the backend-named gateway's endpoint — "+
			"the wrapper must forward `backend`", endpoint)
	}
	if model != "gpt-test-model" {
		t.Errorf("model = %q, want %q — the wrapper must forward `requestedModel`",
			model, "gpt-test-model")
	}

	// A backend naming no configured gateway must yield NO route rather than
	// silently borrowing the one above, which also proves the assertion on
	// `backend` above is not passing by accident.
	if _, _, ok := resolveLiteLLMInferenceRoute(cfg, "not-configured", "gpt-test-model"); ok {
		t.Error("an unknown backend must not resolve a route; an invented endpoint is the 502 this path prevents")
	}
}

// TestResolveGatewayAuthWrapperForwardsAgentAndBackend pins the wrapper's other
// two arguments.
//
// agentName and backend are observable in exactly one place: the warning logged
// when the watsonx IAM mint fails. That makes them easy to swap or drop without
// any test noticing — and they are the only attribution an operator gets for
// "which agent, through which gateway, could not authenticate". A swapped pair
// sends them to the wrong agent.
func TestResolveGatewayAuthWrapperForwardsAgentAndBackend(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errorMessage":"key rejected"}`, http.StatusBadRequest)
	}))
	defer iam.Close()
	orig := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(iam.URL+"/identity/token", nil)
	t.Cleanup(func() { watsonx.DefaultMinter = orig })

	const keyEnv = "HIVE_TEST_WRAPPER_WATSONX_KEY"
	t.Setenv(keyEnv, "raw-rejected-key")
	gw := &config.GatewayConfig{
		Name:      "watsonx",
		Kind:      config.GatewayKindWatsonx,
		APIKeyEnv: keyEnv,
		ProjectID: "proj-wrapper",
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	key, headers := resolveGatewayAuth(gw, "scanner", "watsonx-gw", logger)

	// The documented failure contract still holds through the wrapper.
	if key != "raw-rejected-key" {
		t.Errorf("key = %q, want the raw key passed through on mint failure", key)
	}
	if got := headers[watsonx.ProjectIDHeader]; got != "proj-wrapper" {
		t.Errorf("project header = %q, want %q", got, "proj-wrapper")
	}

	logged := buf.String()
	if !strings.Contains(logged, `agent=scanner`) {
		t.Errorf("mint-failure warning did not attribute the agent; the wrapper must "+
			"forward `agentName`. log: %s", logged)
	}
	if !strings.Contains(logged, `gateway=watsonx-gw`) {
		t.Errorf("mint-failure warning did not name the gateway; the wrapper must "+
			"forward `backend`. log: %s", logged)
	}
	// Whatever else happens, the raw IBM Cloud key must never reach the log.
	if strings.Contains(logged, "raw-rejected-key") {
		t.Error("the raw API key leaked into the mint-failure warning")
	}
}
