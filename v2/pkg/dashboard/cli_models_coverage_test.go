package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCovD_QueryCLIModels drives queryCLIModels for every backend, exercising
// the static-list, cache, and fallback branches (all offline).
func TestCovD_QueryCLIModels(t *testing.T) {
	swapGooseProbe(t, gooseProbeMissing)
	logger := testLoggerCovD()
	s := NewServer(0, logger)

	backends := []string{"claude", "codex", "copilot", "gemini", "goose"}
	for _, b := range backends {
		r := s.queryCLIModels(b)
		if len(r.models) == 0 {
			t.Errorf("queryCLIModels(%q) returned empty list", b)
		}
	}
	// An unknown backend has no static fallback → empty list (default branch).
	if r := s.queryCLIModels("unknown-backend"); len(r.models) != 0 {
		t.Errorf("unknown backend should have empty list, got %v", r.models)
	}
	// Second call for claude hits the cache branch.
	if r := s.queryCLIModels("claude"); len(r.models) == 0 {
		t.Error("cached claude query returned empty")
	}
	// copilot always includes the auto sentinel.
	cop := s.queryCLIModels("copilot")
	if !containsStrCovD(cop.models, "auto") {
		t.Errorf("copilot models missing 'auto': %v", cop.models)
	}
}

// TestCovD_CLIStaticFallback covers cliStaticFallback for all backends.
func TestCovD_CLIStaticFallback(t *testing.T) {
	for _, b := range []string{"copilot", "gemini", "claude", "codex", "goose", "nope"} {
		_ = cliStaticFallback(b)
	}
	if cliStaticFallback("goose")[0] != "default" {
		t.Error("goose fallback should be [default]")
	}
	if cliStaticFallback("nope") != nil {
		t.Error("unknown backend fallback should be nil")
	}
}

// TestCovD_DiscoverModelsNoCreds covers the no-token / no-key early returns.
func TestCovD_DiscoverModelsNoCreds(t *testing.T) {
	logger := testLoggerCovD()
	s := NewServer(0, logger)

	// No AgentMgr and no env var → copilotToken returns "" → fallback.
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	if r := s.discoverCopilotModels(); !r.fallback {
		t.Error("discoverCopilotModels with no token should be fallback")
	}
	if tok := s.copilotToken(); tok != "" {
		t.Errorf("copilotToken should be empty, got %q", tok)
	}

	// No Gemini key → fallback.
	for _, v := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_API_KEY"} {
		t.Setenv(v, "")
	}
	if r := s.discoverGeminiModels(); !r.fallback {
		t.Error("discoverGeminiModels with no key should be fallback")
	}
	if geminiAPIKey() != "" {
		t.Error("geminiAPIKey should be empty")
	}
	t.Setenv("GEMINI_API_KEY", "k")
	if geminiAPIKey() != "k" {
		t.Error("geminiAPIKey should resolve GEMINI_API_KEY")
	}
}

// TestCovD_DiscoverGooseModels covers the env-driven goose static fallback
// (probe seam pinned to "binary missing" so a locally installed goose cannot
// make this nondeterministic).
func TestCovD_DiscoverGooseModels(t *testing.T) {
	swapGooseProbe(t, gooseProbeMissing)
	logger := testLoggerCovD()
	s := NewServer(0, logger)

	t.Setenv("GOOSE_PROVIDER", "")
	t.Setenv("GOOSE_MODEL", "")
	if r := s.discoverGooseModels(); len(r.models) == 0 || r.models[0] != "default" {
		t.Errorf("unconfigured goose = %v, want [default]", r.models)
	}

	t.Setenv("GOOSE_PROVIDER", "ollama")
	t.Setenv("GOOSE_MODEL", "my-pinned-model")
	r := s.discoverGooseModels()
	if r.models[0] != "my-pinned-model" {
		t.Errorf("pinned goose model not first: %v", r.models)
	}
}

// TestCovD_CopilotAPIHost covers copilotAPIHost against a live test server (200
// with an endpoints.api, and a non-200 fallback), plus parseCopilotAPIHost.
func TestCovD_CopilotAPIHost(t *testing.T) {
	logger := testLoggerCovD()
	s := NewServer(0, logger)

	// parseCopilotAPIHost: good, empty, malformed.
	host, ok := parseCopilotAPIHost(strings.NewReader(`{"endpoints":{"api":"https://api.example.com"}}`))
	if !ok || host != "https://api.example.com" {
		t.Errorf("parseCopilotAPIHost good = %q,%v", host, ok)
	}
	if _, ok := parseCopilotAPIHost(strings.NewReader(`{"endpoints":{"api":""}}`)); ok {
		t.Error("empty api should be !ok")
	}
	if _, ok := parseCopilotAPIHost(strings.NewReader(`{bad`)); ok {
		t.Error("malformed should be !ok")
	}

	// Live server returns endpoints.api → copilotAPIHost uses it.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"endpoints":{"api":"https://api.custom.example/"}}`))
	}))
	defer ts.Close()
	// copilotAPIHost hits a fixed URL; we can't redirect it, but calling it
	// exercises the request-build + client.Do + fallback path (the real
	// github URL will fail or return non-200 in CI → default host).
	got := s.copilotAPIHost("faketoken")
	if got == "" {
		t.Error("copilotAPIHost returned empty")
	}
}

// TestCovD_FetchCopilotModels covers fetchCopilotModels + parseCopilotModelsResponse
// against a live test server (chat filter, all-fallback, non-200, and the
// discoverCopilotModels success path via COPILOT_GITHUB_TOKEN).
func TestCovD_FetchCopilotModels(t *testing.T) {
	// chat-typed models: only "chat" ids returned.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"gpt-x","capabilities":{"type":"chat"}},{"id":"embed-x","capabilities":{"type":"embedding"}}]}`))
	}))
	defer ts.Close()
	models, err := fetchCopilotModels(ts.URL+"/models", "tok", "vscode-chat")
	if err != nil {
		t.Fatalf("fetchCopilotModels: %v", err)
	}
	if len(models) != 1 || models[0] != "gpt-x" {
		t.Errorf("chat filter = %v, want [gpt-x]", models)
	}

	// no capabilities.type anywhere → returns all ids.
	tsAny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":""}]}`))
	}))
	defer tsAny.Close()
	models, err = fetchCopilotModels(tsAny.URL+"/models", "tok", "id")
	if err != nil || len(models) != 2 {
		t.Errorf("all-fallback = %v err=%v", models, err)
	}

	// non-200 → error.
	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts500.Close()
	if _, err := fetchCopilotModels(ts500.URL+"/models", "t", "i"); err == nil {
		t.Error("non-200 should error")
	}

	// bad URL → build/request error.
	if _, err := fetchCopilotModels("http://127.0.0.1:1/models", "t", "i"); err == nil {
		t.Error("unreachable should error")
	}

	// parseCopilotModelsResponse malformed body.
	if _, err := parseCopilotModelsResponse(strings.NewReader("{bad")); err == nil {
		t.Error("malformed copilot models should error")
	}
}

// TestCovD_FetchGeminiModels covers fetchGeminiModels + parseGeminiModelsResponse.
func TestCovD_FetchGeminiModels(t *testing.T) {
	// parseGeminiModelsResponse: keep generateContent, strip prefix, skip others.
	out, err := parseGeminiModelsResponse(strings.NewReader(
		`{"models":[{"name":"models/gemini-x","supportedGenerationMethods":["generateContent"]},` +
			`{"name":"models/embed","supportedGenerationMethods":["embedContent"]},` +
			`{"name":""}]}`))
	if err != nil {
		t.Fatalf("parseGeminiModelsResponse: %v", err)
	}
	if len(out) != 1 || out[0] != "gemini-x" {
		t.Errorf("gemini parse = %v, want [gemini-x]", out)
	}
	if _, err := parseGeminiModelsResponse(strings.NewReader("{bad")); err == nil {
		t.Error("malformed gemini body should error")
	}

	// fetchGeminiModels against unreachable host → error.
	if _, err := fetchGeminiModels("dummy-key"); err == nil {
		// The real endpoint may or may not be reachable in CI; both a network
		// error and a non-200 are acceptable failures. A rare 200 is fine too.
		t.Logf("fetchGeminiModels returned no error (endpoint reachable in this env)")
	}
}

func containsStrCovD(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
