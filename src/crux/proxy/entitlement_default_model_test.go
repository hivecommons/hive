package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
)

// fmaTeam403 is the LiteLLM team-scope denial verbatim from the FMA hive
// (hivecommons/hive#9945): the team may use exactly two models, neither of
// which is the one the agent asked for.
const fmaTeam403 = `{"error":{"message":"team not allowed to access model. This team can only access ` +
	`models=['rits/zai-org/glm-5-3', 'rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4']. ` +
	`Tried to access aws/claude-sonnet-4-6","type":"team_model_access_denied","code":"403"}}`

// Reproduces hivecommons/hive#9945 as seen after #11794: the gateway's
// default_model (X) is the only model the team can use, the agent entry
// still carries a `model:` written for its earlier copilot backend, and the
// per-agent value won — every call went out for aws/claude-sonnet-4-6 and
// 403'd. Once the gateway has told us the entitled set, a per-agent model
// that is not in it must give way to the gateway default, and a request must
// never go out for the stale model (or the Claude CLI's own built-in default).
func TestRouteWithEntitledModel_FallsBackToGatewayDefault(t *testing.T) {
	const gatewayDefault = "rits/zai-org/glm-5-3"
	p := &GitHubProxy{
		logger:       slog.Default(),
		inference:    newInferenceRouter(),
		entitlements: newEntitlementStore(),
	}

	// Case 1: agent config WITHOUT a model → the route resolver already
	// filled Model with the gateway default; it must pass through verbatim.
	noModel := &InferenceRoute{Backend: "litellm", Endpoint: "https://litellm.cb.example.com",
		Model: gatewayDefault, DefaultModel: gatewayDefault, APIKey: "sk-team"}
	p.inference.Set("scanner", noModel)
	if got := p.routeWithEntitledModel(noModel, "scanner"); got.Model != gatewayDefault {
		t.Fatalf("no per-agent model: forwarded model = %q, want gateway default %q", got.Model, gatewayDefault)
	}

	// Case 2: agent config WITH a stale model the team is not entitled to.
	stale := &InferenceRoute{Backend: "litellm", Endpoint: "https://litellm.cb.example.com",
		Model: "claude-sonnet-4-6", DefaultModel: gatewayDefault, APIKey: "sk-team"}
	p.inference.Set("quality", stale)

	// Before any entitlement signal the explicit model is honoured (the
	// gateway has not said it is unusable yet); the first 403 then teaches
	// the entitled set.
	if got := p.routeWithEntitledModel(stale, "quality"); got.Model != "claude-sonnet-4-6" {
		t.Fatalf("unknown entitlements must honour the explicit model, got %q", got.Model)
	}
	p.recordInferenceError(stale, "quality", http.StatusForbidden, []byte(fmaTeam403))

	got := p.routeWithEntitledModel(stale, "quality")
	if got == stale {
		t.Fatalf("non-entitled per-agent model must return a rewritten copy")
	}
	if got.Model != gatewayDefault {
		t.Fatalf("forwarded model = %q, want gateway default %q", got.Model, gatewayDefault)
	}
	if got.Endpoint != stale.Endpoint || got.APIKey != stale.APIKey || got.DefaultModel != gatewayDefault {
		t.Fatalf("fallback must only touch Model: %+v", got)
	}
	if p.inference.Get("quality").Model != "claude-sonnet-4-6" {
		t.Fatalf("stored route must not be mutated")
	}

	// The outbound OpenAI body names the forwarded route's model — never the
	// model the Claude CLI put in its own request (its built-in default).
	cliBody := []byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateAnthropicToOpenAI(cliBody, got.Model, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != gatewayDefault {
		t.Fatalf("request model = %q, want %q (never the CLI default or the stale per-agent model)", sent.Model, gatewayDefault)
	}

	// Case 3: the explicit per-agent model IS entitled → it wins over the default.
	explicit := &InferenceRoute{Backend: "litellm", Endpoint: "https://litellm.cb.example.com",
		Model: "rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4", DefaultModel: gatewayDefault, APIKey: "sk-team"}
	if got := p.routeWithEntitledModel(explicit, "architect"); got != explicit {
		t.Fatalf("entitled explicit model must pass through unchanged, got %q", got.Model)
	}

	// Case 4: drift in the DEFAULT resolves too (dotted default vs entitled id).
	driftDefault := &InferenceRoute{Backend: "litellm", Endpoint: "https://litellm.cb.example.com",
		Model: "claude-sonnet-4-6", DefaultModel: "zai-org/glm-5.3", APIKey: "sk-team"}
	if got := p.routeWithEntitledModel(driftDefault, "quality"); got.Model != gatewayDefault {
		t.Fatalf("drifted default must resolve to %q, got %q", gatewayDefault, got.Model)
	}

	// Case 5: neither the model nor the default is entitled → unchanged, so
	// the gateway's own 403 (naming the entitled set) reaches the operator.
	neither := &InferenceRoute{Backend: "litellm", Endpoint: "https://litellm.cb.example.com",
		Model: "claude-sonnet-4-6", DefaultModel: "gpt-4o", APIKey: "sk-team"}
	if got := p.routeWithEntitledModel(neither, "quality"); got != neither {
		t.Fatalf("un-entitled default must not be substituted, got %q", got.Model)
	}

	// Case 6: no default configured → unchanged (today's behaviour).
	noDefault := &InferenceRoute{Backend: "litellm", Endpoint: "https://litellm.cb.example.com",
		Model: "claude-sonnet-4-6", APIKey: "sk-team"}
	if got := p.routeWithEntitledModel(noDefault, "quality"); got != noDefault {
		t.Fatalf("no default must pass through unchanged")
	}
}
