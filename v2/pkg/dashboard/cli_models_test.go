package dashboard

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseCopilotModelsResponse_FiltersChat(t *testing.T) {
	body := `{"data":[
		{"id":"gpt-4o","capabilities":{"type":"chat"}},
		{"id":"gpt-4o-mini-2024-07-18","capabilities":{"type":"chat"}},
		{"id":"text-embedding-3-small","capabilities":{"type":"embeddings"}},
		{"id":"","capabilities":{"type":"chat"}}
	]}`
	got, err := parseCopilotModelsResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"gpt-4o", "gpt-4o-mini-2024-07-18"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v (embeddings and empty id should be dropped)", got, want)
	}
}

func TestParseCopilotModelsResponse_NoTypeReturnsAll(t *testing.T) {
	// Older/edge responses may omit capabilities.type entirely — we then keep
	// every non-empty id rather than returning an empty list.
	body := `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`
	got, err := parseCopilotModelsResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !equalStrings(got, []string{"gpt-4o", "gpt-4o-mini"}) {
		t.Fatalf("got %v, want all ids", got)
	}
}

func TestParseCopilotModelsResponse_Malformed(t *testing.T) {
	if _, err := parseCopilotModelsResponse(strings.NewReader("not json")); err == nil {
		t.Fatal("expected error on malformed body")
	}
}

func TestParseCopilotAPIHost(t *testing.T) {
	body := `{"login":"x","endpoints":{"api":"https://api.enterprise.githubcopilot.com","proxy":"y"}}`
	host, ok := parseCopilotAPIHost(strings.NewReader(body))
	if !ok || host != "https://api.enterprise.githubcopilot.com" {
		t.Fatalf("got %q ok=%v, want enterprise api host", host, ok)
	}
}

func TestParseCopilotAPIHost_Missing(t *testing.T) {
	if _, ok := parseCopilotAPIHost(strings.NewReader(`{"login":"x"}`)); ok {
		t.Fatal("expected ok=false when endpoints.api absent")
	}
}

func TestParseGeminiModelsResponse_FiltersGenerateContent(t *testing.T) {
	body := `{"models":[
		{"name":"models/gemini-3-pro-preview","supportedGenerationMethods":["generateContent","countTokens"]},
		{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]},
		{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}
	]}`
	got, err := parseGeminiModelsResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"gemini-3-pro-preview", "gemini-2.5-flash"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v (embedding-only + models/ prefix should be handled)", got, want)
	}
}

func TestDedupeModels(t *testing.T) {
	got := dedupeModels([]string{"a", "b", "a", "", "c", "b"})
	if !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %v, want [a b c]", got)
	}
}

// TestQueryCLIModels_ClaudeStatic verifies claude returns a non-empty,
// non-fallback (authoritative static) list without any network call.
func TestQueryCLIModels_ClaudeStatic(t *testing.T) {
	s := &Server{cliModels: newCLIModelCache()}
	r := s.queryCLIModels("claude")
	if len(r.models) == 0 {
		t.Fatal("claude models should never be empty")
	}
	if r.fallback {
		t.Fatal("claude static list is authoritative, not a fallback")
	}
	if !contains(r.models, "claude-opus-4-8") {
		t.Fatalf("claude models missing current id: %v", r.models)
	}
}

// TestQueryCLIModels_CopilotFallbackNoToken verifies that without a Copilot
// token, discovery yields the static fallback (non-empty, fallback=true) and
// does not error.
func TestQueryCLIModels_CopilotFallbackNoToken(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("copilot")
	if len(r.models) == 0 {
		t.Fatal("copilot fallback must be non-empty")
	}
	if !r.fallback {
		t.Fatal("copilot with no token must be marked fallback")
	}
}

// TestQueryCLIModels_CopilotAlwaysIncludePinned verifies every id in
// copilotAlwaysIncludeModels is present in the served copilot list even when
// it is the static fallback (no token), so rollout-gated ids stay selectable
// and safe from model auto-heal regardless of what live discovery returns.
func TestQueryCLIModels_CopilotAlwaysIncludePinned(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("copilot")
	for _, m := range copilotAlwaysIncludeModels {
		if !contains(r.models, m) {
			t.Fatalf("always-include id %q missing from served copilot list: %v", m, r.models)
		}
	}
}

// TestQueryCLIModels_CopilotAutoSentinelServed verifies the Copilot auto-select
// sentinel ("auto") is always offered in the served copilot list, so an agent
// can be launched with `copilot --model auto` and the selection survives model
// auto-heal (the sentinel is not in the discovered catalog by design).
func TestQueryCLIModels_CopilotAutoSentinelServed(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("copilot")
	if !contains(r.models, copilotAutoModel) {
		t.Fatalf("copilot auto sentinel %q missing from served list: %v", copilotAutoModel, r.models)
	}
}

// TestQueryCLIModels_GeminiFallbackNoKey verifies gemini falls back when no
// API key is configured.
func TestQueryCLIModels_GeminiFallbackNoKey(t *testing.T) {
	for _, v := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_API_KEY"} {
		t.Setenv(v, "")
	}
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("gemini")
	if len(r.models) == 0 || !r.fallback {
		t.Fatalf("gemini with no key must be a non-empty fallback, got %+v", r)
	}
}

// TestQueryCLIModels_GooseUsesProvider verifies goose maps a configured
// provider to its static list.
func TestQueryCLIModels_GooseUsesProvider(t *testing.T) {
	t.Setenv("GOOSE_PROVIDER", "anthropic")
	t.Setenv("GOOSE_MODEL", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("goose")
	if !contains(r.models, "claude-opus-4-8") {
		t.Fatalf("goose+anthropic should include anthropic models, got %v", r.models)
	}
	if !r.fallback {
		t.Fatal("goose provider list is a curated fallback, not live discovery")
	}
}

func TestQueryCLIModels_GooseUnconfigured(t *testing.T) {
	t.Setenv("GOOSE_PROVIDER", "")
	t.Setenv("GOOSE_MODEL", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("goose")
	if !equalStrings(r.models, []string{"default"}) {
		t.Fatalf("unconfigured goose should be [default], got %v", r.models)
	}
}

func TestQueryCLIModels_GoosePinnedModelFirst(t *testing.T) {
	t.Setenv("GOOSE_PROVIDER", "ollama")
	t.Setenv("GOOSE_MODEL", "my-pinned-model")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("goose")
	if len(r.models) == 0 || r.models[0] != "my-pinned-model" {
		t.Fatalf("pinned GOOSE_MODEL should be first, got %v", r.models)
	}
}

// TestCLIModelCache_TTL verifies the cache returns a stored value within TTL
// and misses after it expires.
func TestCLIModelCache_TTL(t *testing.T) {
	c := newCLIModelCache()
	c.set("copilot", cliModelResult{models: []string{"x"}})
	if _, ok := c.get("copilot"); !ok {
		t.Fatal("expected cache hit within TTL")
	}
	// Force expiry by backdating the entry.
	c.mu.Lock()
	c.entries["copilot"] = cliModelCacheEntry{
		result: cliModelResult{models: []string{"x"}},
		at:     time.Now().Add(-cliModelCacheTTL - time.Second),
	}
	c.mu.Unlock()
	if _, ok := c.get("copilot"); ok {
		t.Fatal("expected cache miss after TTL")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- stabilize: live-list retention across nondeterministic upstream samples ---

// TestCLIModelCacheStabilize_TransientMissRetained verifies a model seen in a
// previous live probe survives samples that transiently omit it (the observed
// Copilot /models 31-vs-35 flapping), and reappearance resets its miss count.
func TestCLIModelCacheStabilize_TransientMissRetained(t *testing.T) {
	c := newCLIModelCache()
	full := []string{"claude-opus-4.7", "claude-opus-4.6", "gpt-5.4"}
	bad := []string{"claude-opus-4.7", "gpt-5.4"} // sample missing claude-opus-4.6

	if got := c.stabilize("copilot", full); !equalStrings(got, full) {
		t.Fatalf("first probe should pass through, got %v", got)
	}
	// Misses 1..cliModelDropAfterMisses-1: the model must be retained.
	for i := 1; i < cliModelDropAfterMisses; i++ {
		got := c.stabilize("copilot", bad)
		if !contains(got, "claude-opus-4.6") {
			t.Fatalf("miss %d: claude-opus-4.6 must be retained, got %v", i, got)
		}
	}
	// Reappearance resets the miss counter...
	if got := c.stabilize("copilot", full); !contains(got, "claude-opus-4.6") {
		t.Fatalf("reappearance: got %v", got)
	}
	// ...so the drop clock starts over.
	for i := 1; i < cliModelDropAfterMisses; i++ {
		if got := c.stabilize("copilot", bad); !contains(got, "claude-opus-4.6") {
			t.Fatalf("post-reset miss %d: claude-opus-4.6 must be retained, got %v", i, got)
		}
	}
}

// TestCLIModelCacheStabilize_DropsAfterConsecutiveMisses verifies a genuinely
// removed model is dropped once (and only once) it has been missing from
// cliModelDropAfterMisses consecutive live probes.
func TestCLIModelCacheStabilize_DropsAfterConsecutiveMisses(t *testing.T) {
	c := newCLIModelCache()
	full := []string{"claude-opus-4.7", "claude-opus-4.6"}
	bad := []string{"claude-opus-4.7"}

	c.stabilize("copilot", full)
	var got []string
	for i := 0; i < cliModelDropAfterMisses; i++ {
		got = c.stabilize("copilot", bad)
	}
	if contains(got, "claude-opus-4.6") {
		t.Fatalf("model must drop after %d consecutive misses, got %v", cliModelDropAfterMisses, got)
	}
	// And the served order/content now equals the fresh probe exactly.
	if !equalStrings(got, bad) {
		t.Fatalf("got %v, want %v", got, bad)
	}
}

// TestCLIModelCacheStabilize_PerBackendIsolation verifies retention state is
// keyed per backend.
func TestCLIModelCacheStabilize_PerBackendIsolation(t *testing.T) {
	c := newCLIModelCache()
	c.stabilize("copilot", []string{"a", "b"})
	if got := c.stabilize("gemini", []string{"x"}); !equalStrings(got, []string{"x"}) {
		t.Fatalf("gemini must not inherit copilot retention, got %v", got)
	}
	if got := c.stabilize("copilot", []string{"a"}); !contains(got, "b") {
		t.Fatalf("copilot retention lost, got %v", got)
	}
}

// TestQueryCLIModels_BobAutoOnly verifies bob is served EXACTLY one model, the
// auto sentinel, and that it is authoritative rather than a fallback. bob has
// no model catalog and no usable --model flag, so any additional option would
// invite a selection it cannot honor; marking it fallback would label it
// "(common alias, unverified)" in the dropdown and expose it to auto-heal.
func TestQueryCLIModels_BobAutoOnly(t *testing.T) {
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels(bobBackendID)
	if len(r.models) != 1 {
		t.Fatalf("bob must be served exactly one model, got %v", r.models)
	}
	if r.models[0] != bobAutoModel {
		t.Fatalf("bob's only model must be %q, got %q", bobAutoModel, r.models[0])
	}
	if r.fallback {
		t.Fatal("bob's single-option list is authoritative, not a fallback")
	}
}

// TestCliStaticFallback_BobAutoOnly verifies the static fallback path (used if
// the discovery switch is ever bypassed) also yields only the auto sentinel,
// so bob can never be handed the copilot catalog.
func TestCliStaticFallback_BobAutoOnly(t *testing.T) {
	got := cliStaticFallback(bobBackendID)
	if len(got) != 1 || got[0] != bobAutoModel {
		t.Fatalf("bob static fallback must be exactly [%q], got %v", bobAutoModel, got)
	}
}

// TestQueryCLIModels_BobDoesNotAffectOtherBackends guards against a regression
// where adding bob narrows or empties another CLI backend's catalog.
func TestQueryCLIModels_BobDoesNotAffectOtherBackends(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	for _, backend := range []string{"claude", "copilot", "gemini", "goose", "codex"} {
		r := s.queryCLIModels(backend)
		if len(r.models) == 0 {
			t.Errorf("%s models must not be empty", backend)
		}
		if len(r.models) == 1 && r.models[0] == bobAutoModel {
			t.Errorf("%s must not inherit bob's single auto option: %v", backend, r.models)
		}
	}
}
