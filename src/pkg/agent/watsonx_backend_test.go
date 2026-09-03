package agent

import (
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// The bug this file locks down:
//
// An operator set `backend: watsonx` on a real spoke. The config layer accepted
// it (watsonx had shipped as a `gateways:` KIND and as an onboarding UI option)
// but the agent launcher had no case for it, so the agent never started:
//
//	HIVE LAUNCH FAILED: backend watsonx did not launch: unknown backend: watsonx.
//	{"level":"WARN","msg":"failed to send kick","agent":"ci-maintainer",
//	 "error":"agent ci-maintainer cannot be kicked: it failed to start: unknown backend: watsonx"}
//
// watsonx.ai is a MODEL GATEWAY (OpenAI-compatible endpoint + IAM-minted
// bearer), not an agentic CLI, so it belongs with litellm/vllm/llm-d.

// TestWatsonxIsRecognizedBackend is the direct regression test: the launcher
// must not reject "watsonx" with "unknown backend".
func TestWatsonxIsRecognizedBackend(t *testing.T) {
	if !config.IsInferenceBackend("watsonx") {
		t.Fatal("watsonx must be an inference backend so the launcher can dispatch it")
	}
	if !IsInferenceBackend("watsonx") {
		t.Fatal("agent.IsInferenceBackend(watsonx) = false, want true")
	}
	// backendBinary is the exact site that produced "unknown backend: watsonx".
	// It may still fail with "not found in PATH" in CI (no claude binary
	// installed) — that is a DIFFERENT and acceptable error. The one thing that
	// must never come back is "unknown backend".
	if _, err := backendBinary("watsonx"); err != nil && strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("backendBinary(watsonx) returned %v; watsonx must be a known backend", err)
	}
}

// TestBackendBinary_UnknownStillRejected guards the other direction: making
// watsonx known must not turn backendBinary into a function that accepts
// anything.
func TestBackendBinary_UnknownStillRejected(t *testing.T) {
	_, err := backendBinary("totally-made-up")
	if err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("backendBinary(totally-made-up) err = %v, want 'unknown backend'", err)
	}
}

// TestEveryInferenceBackendHasABinary is the anti-drift test. Every backend the
// config layer will ACCEPT must be one the launcher can dispatch — that gap is
// precisely what let `backend: watsonx` be saved and then fail at kick time.
func TestEveryInferenceBackendHasABinary(t *testing.T) {
	for _, b := range config.InferenceBackends {
		if _, err := backendBinary(b); err != nil && strings.Contains(err.Error(), "unknown backend") {
			t.Errorf("backend %q is accepted by config but unknown to the launcher", b)
		}
	}
}

// TestWatsonxNeverRequiresInteractiveAuth: watsonx authenticates with an IBM
// Cloud API key exchanged for an IAM bearer — there is no interactive login for
// an operator to perform, so a 🔑 badge on a watsonx agent is always wrong.
func TestWatsonxNeverRequiresInteractiveAuth(t *testing.T) {
	if BackendRequiresInteractiveAuth("watsonx") {
		t.Fatal("BackendRequiresInteractiveAuth(watsonx) = true, want false (API-key/IAM auth, no login)")
	}
	// Hold the whole gateway family to the same rule.
	for _, b := range config.InferenceBackends {
		if BackendRequiresInteractiveAuth(b) {
			t.Errorf("BackendRequiresInteractiveAuth(%q) = true, want false", b)
		}
	}
	// The interactive CLIs must still require it, or the badge dies everywhere.
	for _, b := range []string{"claude", "copilot", "codex", "gemini"} {
		if !BackendRequiresInteractiveAuth(b) {
			t.Errorf("BackendRequiresInteractiveAuth(%q) = false, want true", b)
		}
	}
}

// TestWatsonxAuthStateNeverNeedsLogin exercises the badge path end to end: even
// with no credential files and a pane that looks like a login prompt, a watsonx
// agent must read as authenticated+known (no badge).
func TestWatsonxAuthStateNeverNeedsLogin(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{}
	for _, running := range []bool{true, false} {
		for _, needsLogin := range []bool{true, false} {
			avail, known := m.AgentAuthState("ci-maintainer", 2007, "watsonx", running, needsLogin)
			if !known || !avail {
				t.Fatalf("watsonx running=%v needsLogin=%v: got (avail=%v, known=%v), want authenticated+known",
					running, needsLogin, avail, known)
			}
		}
	}
}

// TestWatsonxModelNamePassedThroughVerbatim: gateway backends must never have
// their model id rewritten — the id has to match what the gateway serves.
// TestValidateBackendName covers the manager-side set-time gate that stops the
// /switch/{backend} path from accepting a backend the launcher cannot start.
func TestValidateBackendName(t *testing.T) {
	m := &Manager{}
	for _, ok := range []string{"", "claude", "copilot", "litellm", "vllm", "llm-d", "watsonx"} {
		if err := m.validateBackendName(ok); err != nil {
			t.Errorf("validateBackendName(%q) = %v, want nil", ok, err)
		}
	}
	err := m.validateBackendName("totally-made-up")
	if err == nil {
		t.Fatal("validateBackendName(totally-made-up) = nil, want an error")
	}
	// The message must tell the operator what IS valid, not just what is not.
	for _, want := range []string{"totally-made-up", "claude", "watsonx"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

// TestGatewayBackendNameStillValidates: a backend naming a configured gateway
// is legal, and that must survive the new validation.
func TestGatewayBackendNameStillValidates(t *testing.T) {
	m := &Manager{}
	checker := func(backend string) bool { return backend == "ibm-granite-prod" }
	m.SetGatewayBackendChecker(checker)
	if err := m.validateBackendName("ibm-granite-prod"); err != nil {
		t.Fatalf("a configured gateway name must be a valid backend, got %v", err)
	}
	if err := m.validateBackendName("some-other-gateway"); err == nil {
		t.Fatal("an unconfigured gateway name must not validate")
	}
}
