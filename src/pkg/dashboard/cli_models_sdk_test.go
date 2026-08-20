package dashboard

import (
	"context"
	"errors"
	"testing"
)

// swapSDKHelper substitutes the SDK-helper seam for one test and restores it.
func swapSDKHelper(t *testing.T, fn func(ctx context.Context, token string) ([]byte, error)) {
	t.Helper()
	prev := runCopilotSDKHelper
	runCopilotSDKHelper = fn
	t.Cleanup(func() { runCopilotSDKHelper = prev })
}

func TestParseCopilotSDKModels_FiltersPolicyAndAuto(t *testing.T) {
	body := []byte(`{"models":[
		{"id":"gpt-5.4","name":"GPT-5.4","policyState":"enabled"},
		{"id":"claude-fable-5","name":"Claude Fable 5","policyState":"enabled","efforts":["low","medium","high"],"defaultEffort":"medium"},
		{"id":"grok-code-fast-1","name":"Grok","policyState":"unconfigured"},
		{"id":"gemini-2.5-pro","name":"Gemini"},
		{"id":"auto","name":"Auto"},
		{"id":"","name":"nameless"}
	]}`)
	got, err := parseCopilotSDKModels(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// enabled ids kept; absent policy kept; non-enabled policy dropped; the
	// "auto" pseudo-entry and empty ids dropped.
	want := []string{"gpt-5.4", "claude-fable-5", "gemini-2.5-pro"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseCopilotSDKModels_Malformed(t *testing.T) {
	if _, err := parseCopilotSDKModels([]byte("not json")); err == nil {
		t.Fatal("expected error on malformed helper output")
	}
}

func TestParseCopilotSDKModels_Empty(t *testing.T) {
	got, err := parseCopilotSDKModels([]byte(`{"models":[]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no models, got %v", got)
	}
}

// TestDiscoverCopilotModels_SDKSuccess verifies the SDK probe is consulted
// FIRST and that a successful helper run yields a live (non-fallback) result
// even with no token available to the raw-HTTP path — the hardening this
// probe adds.
func TestDiscoverCopilotModels_SDKSuccess(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return []byte(`{"models":[{"id":"gpt-5.4","policyState":"enabled"},{"id":"claude-fable-5","policyState":"enabled"}]}`), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverCopilotModels()
	if r.fallback {
		t.Fatalf("SDK success must be a live result, got fallback: %+v", r)
	}
	if !equalStrings(r.models, []string{"gpt-5.4", "claude-fable-5"}) {
		t.Fatalf("got %v, want SDK-discovered ids", r.models)
	}
}

// TestDiscoverCopilotModels_SDKFailureFallsBack verifies a nonzero-exit
// helper falls through to the raw-HTTP probe path; with no token that path
// resolves to the static fallback, so the chain ends at fallback=true rather
// than an error or empty list.
func TestDiscoverCopilotModels_SDKFailureFallsBack(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverCopilotModels()
	if !r.fallback {
		t.Fatalf("SDK failure with no token must fall back, got %+v", r)
	}
	// The full pipeline still serves a non-empty static list.
	q := s.queryCLIModels("copilot")
	if len(q.models) == 0 || !q.fallback {
		t.Fatalf("pipeline must serve non-empty static fallback, got %+v", q)
	}
}

// TestDiscoverCopilotModels_SDKMalformedFallsBack verifies helper stdout that
// fails to parse is treated exactly like a failed probe.
func TestDiscoverCopilotModels_SDKMalformedFallsBack(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return []byte("garbage"), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.discoverCopilotModels(); !r.fallback {
		t.Fatalf("malformed SDK output must fall back, got %+v", r)
	}
}

// TestDiscoverCopilotModels_SDKEmptyFallsBack verifies a well-formed helper
// response with zero usable models is not served as a live (empty) list.
func TestDiscoverCopilotModels_SDKEmptyFallsBack(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return []byte(`{"models":[{"id":"auto"}]}`), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.discoverCopilotModels(); !r.fallback {
		t.Fatalf("empty SDK result must fall back, got %+v", r)
	}
}

// TestQueryCLIModels_SDKResultsFeedRetention verifies SDK-discovered models
// ride the same stabilize/retention machinery as the HTTP probe: a model seen
// in one live SDK sample survives its absence from the next sample.
func TestQueryCLIModels_SDKResultsFeedRetention(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	sample := []byte(`{"models":[{"id":"gpt-5.4","policyState":"enabled"},{"id":"claude-fable-5","policyState":"enabled"}]}`)
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		out := sample
		return out, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.queryCLIModels("copilot"); !contains(r.models, "claude-fable-5") {
		t.Fatalf("first live sample missing id: %v", r.models)
	}
	// Next probe omits claude-fable-5; expire the cache and re-query.
	sample = []byte(`{"models":[{"id":"gpt-5.4","policyState":"enabled"}]}`)
	s.cliModels.entries = map[string]cliModelCacheEntry{}
	r := s.queryCLIModels("copilot")
	if r.fallback {
		t.Fatalf("live SDK result must not be fallback: %+v", r)
	}
	if !contains(r.models, "claude-fable-5") {
		t.Fatalf("retention should keep recently-seen id through one miss: %v", r.models)
	}
}

// TestProbeCopilotModelsSDK_HelperNotInstalled verifies the real exec path
// degrades instantly (no hang, no panic) when the helper script is absent —
// the situation on dev machines and CI runners.
func TestProbeCopilotModelsSDK_HelperNotInstalled(t *testing.T) {
	s := &Server{logger: testLogger()}
	if _, err := s.probeCopilotModelsSDK(""); err == nil {
		// The helper is installed only inside the hive image; if this machine
		// actually has it, the probe may legitimately succeed — skip then.
		t.Skip("copilot SDK helper present on this machine; skipping absence check")
	}
}

// TestDiscoverCopilotModels_CanonicalizesNomenclatureDrift verifies discovery
// normalizes catalog ids whose version separator drifts from the copilot
// CLI's --model nomenclature (#4262): a catalog sample carrying the dotted
// claude-fable.5 is served as the CLI-accepted claude-fable-5, canonical
// dotted ids (claude-opus-4.6, gpt-5.5, gemini-2.5-pro) are untouched, and
// unknown ids pass through verbatim. Because the served list is canonical,
// the stabilize/auto-heal retention keys can never see the same model under
// two spellings (false "model revoked" churn).
func TestDiscoverCopilotModels_CanonicalizesNomenclatureDrift(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return []byte(`{"models":[
			{"id":"claude-fable.5","policyState":"enabled"},
			{"id":"claude-opus-4.6","policyState":"enabled"},
			{"id":"gpt-5.5","policyState":"enabled"},
			{"id":"gemini-2.5-pro","policyState":"enabled"},
			{"id":"some-future-model.9","policyState":"enabled"}
		]}`), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverCopilotModels()
	if r.fallback {
		t.Fatalf("SDK success must be a live result, got %+v", r)
	}
	want := []string{"claude-fable-5", "claude-opus-4.6", "gpt-5.5", "gemini-2.5-pro", "some-future-model.9"}
	if !equalStrings(r.models, want) {
		t.Fatalf("got %v, want canonicalized %v", r.models, want)
	}
}

// TestStabilize_NomenclatureDriftNoChurn verifies alternating catalog samples
// that flip a model's separator spelling do not accumulate duplicate retained
// entries once discovery canonicalizes ids: both spellings collapse to one
// retention key, so the same model can never be counted absent while present.
func TestStabilize_NomenclatureDriftNoChurn(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	samples := [][]byte{
		[]byte(`{"models":[{"id":"claude-fable.5","policyState":"enabled"}]}`),
		[]byte(`{"models":[{"id":"claude-fable-5","policyState":"enabled"}]}`),
		[]byte(`{"models":[{"id":"claude-fable.5","policyState":"enabled"}]}`),
	}
	i := 0
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		out := samples[i%len(samples)]
		i++
		return out, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	for n := 0; n < 3; n++ {
		s.cliModels.entries = map[string]cliModelCacheEntry{}
		r := s.queryCLIModels("copilot")
		if r.fallback {
			t.Fatalf("sample %d: live result expected, got %+v", n, r)
		}
		count := 0
		for _, m := range r.models {
			if m == "claude-fable-5" {
				count++
			}
			if m == "claude-fable.5" {
				t.Fatalf("sample %d: non-canonical spelling served: %v", n, r.models)
			}
		}
		if count != 1 {
			t.Fatalf("sample %d: want exactly one claude-fable-5, got %v", n, r.models)
		}
	}
}
