package config

import (
	"strings"
	"testing"
)

// These tests cover the "silent accept-then-fail" half of the watsonx bug: the
// config layer accepted `backend: watsonx` while the launcher rejected it at
// kick time. Validation must now happen at SET time, against the same canonical
// lists the launcher dispatches on.

func TestValidateBackend_AcceptsWatsonx(t *testing.T) {
	var g GovernorConfig
	if err := g.ValidateBackend("watsonx"); err != nil {
		t.Fatalf("ValidateBackend(watsonx) = %v, want nil", err)
	}
}

// Ranging over SupportedBackends() alone is tautological (#5388):
// SupportedBackends() is exactly CLIBackends ++ InferenceBackends, and
// ValidateBackend returns nil early iff IsCLIBackend(b) || IsInferenceBackend(b).
// Every element it yields satisfies that disjunction by construction, so the
// gateway branch and the error-formatting branch below it are unreachable for
// these inputs and no edit to either list can fail the loop.
//
// The loop is kept — it is cheap and it does pin the early-return path — but
// the assertions that can actually fail are the ones after it: a rejection
// that must happen, and the gateway acceptance path the loop never reaches.
func TestValidateBackend_AcceptsAllSupported(t *testing.T) {
	var g GovernorConfig
	// Empty means "hive default" and must stay valid.
	if err := g.ValidateBackend(""); err != nil {
		t.Errorf("ValidateBackend(\"\") = %v, want nil", err)
	}
	for _, b := range SupportedBackends() {
		if err := g.ValidateBackend(b); err != nil {
			t.Errorf("ValidateBackend(%q) = %v, want nil", b, err)
		}
	}

	// A name in NEITHER list and matching no gateway must be rejected. Without
	// this, a ValidateBackend rewritten to `return nil` unconditionally would
	// pass every assertion above.
	if err := g.ValidateBackend("definitely-not-a-backend"); err == nil {
		t.Error("ValidateBackend(definitely-not-a-backend) = nil, want an error — validation accepts anything")
	}
}

func TestValidateBackend_RejectsUnknownWithHelpfulMessage(t *testing.T) {
	var g GovernorConfig
	err := g.ValidateBackend("totally-made-up")
	if err == nil {
		t.Fatal("ValidateBackend(totally-made-up) = nil, want an error")
	}
	msg := err.Error()
	// The message must name the offender AND enumerate what is valid — the
	// whole point is that the operator learns this at set time.
	if !strings.Contains(msg, "totally-made-up") {
		t.Errorf("error %q should name the rejected backend", msg)
	}
	for _, want := range []string{"claude", "litellm", "watsonx"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should list supported backend %q", msg, want)
		}
	}
}

// A gateway NAME is a valid backend, and the rejection message should point at
// the configured gateways when there are any.
func TestValidateBackend_AcceptsConfiguredGatewayName(t *testing.T) {
	g := GovernorConfig{
		Gateways: []GatewayConfig{
			{Name: "ibm-granite-prod", Kind: GatewayKindWatsonx, Endpoint: "https://us-south.ml.cloud.ibm.com/ml/gateway"},
		},
	}
	if err := g.ValidateBackend("ibm-granite-prod"); err != nil {
		t.Fatalf("a configured gateway name must be a valid backend, got %v", err)
	}
	// Case-insensitive, mirroring ResolveGateway.
	if err := g.ValidateBackend("IBM-Granite-Prod"); err != nil {
		t.Fatalf("gateway name match must be case-insensitive, got %v", err)
	}
	err := g.ValidateBackend("not-a-gateway")
	if err == nil {
		t.Fatal("an unconfigured gateway name must be rejected")
	}
	if !strings.Contains(err.Error(), "ibm-granite-prod") {
		t.Errorf("error %q should list the configured gateway names", err.Error())
	}
}

// The canonical lists must not overlap: a backend is either an agentic CLI or a
// model gateway, never both, or callers branching on one will mis-handle it.
func TestBackendListsAreDisjoint(t *testing.T) {
	for _, c := range CLIBackends {
		if IsInferenceBackend(c) {
			t.Errorf("backend %q is in both CLIBackends and InferenceBackends", c)
		}
	}
	for _, i := range InferenceBackends {
		if IsCLIBackend(i) {
			t.Errorf("backend %q is in both InferenceBackends and CLIBackends", i)
		}
	}
}

// validate() must reject a bogus agent backend and accept watsonx — this is the
// hive.yaml load path, the same gate the dashboard write path now uses.
func TestValidate_AgentBackendGate(t *testing.T) {
	base := func(backend string) *Config {
		return &Config{
			Project: ProjectConfig{Org: "my-org"},
			GitHub:  GitHubConfig{Token: "t"},
			Agents:  map[string]AgentConfig{"ci-maintainer": {Backend: backend}},
		}
	}
	if err := base("watsonx").validate(); err != nil {
		t.Fatalf("validate() with backend watsonx = %v, want nil", err)
	}
	err := base("totally-made-up").validate()
	if err == nil {
		t.Fatal("validate() with a bogus backend = nil, want an error")
	}
	if !strings.Contains(err.Error(), "ci-maintainer") {
		t.Errorf("error %q should name the offending agent", err.Error())
	}
	if !strings.Contains(err.Error(), "watsonx") {
		t.Errorf("error %q should enumerate supported backends", err.Error())
	}
}
