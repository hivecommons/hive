package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// redirectGeminiEndpoint points the Gemini models URL at a test server and
// restores it on cleanup.
func redirectGeminiEndpoint(t *testing.T, url string) {
	t.Helper()
	orig := geminiModelsURL
	geminiModelsURL = url
	t.Cleanup(func() { geminiModelsURL = orig })
}

// clearGeminiEnv unsets all Gemini-related API key environment variables.
func clearGeminiEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_API_KEY"} {
		t.Setenv(v, "")
	}
}

// geminiModelsTestServer serves a canned Gemini models JSON response and
// captures the request for header and parameter assertions.
func geminiModelsTestServer(t *testing.T, status int, body string, gotReq **http.Request) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotReq != nil {
			*gotReq = r.Clone(r.Context())
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestDiscoverGeminiModels_LiveSuccess(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "test-gemini-secret-key")

	respBody := `{
		"models": [
			{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemini-2.5-pro", "supportedGenerationMethods": ["generateContent", "countTokens"]},
			{"name": "models/embedding-001", "supportedGenerationMethods": ["embedContent"]},
			{"name": "", "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": ["generateContent"]}
		]
	}`

	var req *http.Request
	ts := geminiModelsTestServer(t, http.StatusOK, respBody, &req)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGeminiModels()
	if r.fallback {
		t.Fatal("successful discovery probe must not be marked fallback")
	}
	wantModels := []string{"gemini-2.5-flash", "gemini-2.5-pro"}
	if !equalStrings(r.models, wantModels) {
		t.Fatalf("got models %v, want %v (expected prefix stripping, filtering, and deduplication)", r.models, wantModels)
	}

	// Verify header and query param contract
	if req == nil {
		t.Fatal("expected request to be recorded")
	}
	if gotKey := req.Header.Get("x-goog-api-key"); gotKey != "test-gemini-secret-key" {
		t.Errorf("x-goog-api-key = %q, want %q", gotKey, "test-gemini-secret-key")
	}
	if gotPageSize := req.URL.Query().Get("pageSize"); gotPageSize != "200" {
		t.Errorf("pageSize query param = %q, want 200", gotPageSize)
	}

	// Also verify end-to-end queryCLIModels returns the live discovered models,
	// followed by the always-offered Gemini CLI aliases.
	res := s.queryCLIModels("gemini")
	if res.fallback {
		t.Fatal("queryCLIModels must report fallback=false when discovery succeeds")
	}
	wantServed := append(append([]string(nil), wantModels...), geminiAlwaysIncludeModels...)
	if !equalStrings(res.models, wantServed) {
		t.Fatalf("queryCLIModels returned %v, want %v", res.models, wantServed)
	}
}

func TestDiscoverGeminiModels_FiltersProviderOnlyModels(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "test-gemini-secret-key")

	respBody := `{
		"models": [
			{"name": "models/gemini-2.5-pro", "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemini-future-ultra", "supportedGenerationMethods": ["generateContent"]}
		]
	}`
	ts := geminiModelsTestServer(t, http.StatusOK, respBody, nil)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGeminiModels()
	if r.fallback {
		t.Fatalf("supported intersection must remain live, got %+v", r)
	}
	if !equalStrings(r.models, []string{"gemini-2.5-pro"}) {
		t.Fatalf("got %v, want only the model accepted by the pinned CLI", r.models)
	}
}

func TestDiscoverGeminiModels_AlternativeEnvKeys(t *testing.T) {
	canned := `{"models":[{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}]}`

	cases := []struct {
		name   string
		setVar string
		val    string
	}{
		{"GOOGLE_API_KEY", "GOOGLE_API_KEY", "google-key-123"},
		{"GOOGLE_GENAI_API_KEY", "GOOGLE_GENAI_API_KEY", "genai-key-456"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGeminiEnv(t)
			t.Setenv(tc.setVar, tc.val)

			var req *http.Request
			ts := geminiModelsTestServer(t, http.StatusOK, canned, &req)
			redirectGeminiEndpoint(t, ts.URL)

			s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
			r := s.discoverGeminiModels()
			if r.fallback {
				t.Fatalf("discovery with %s must succeed", tc.setVar)
			}
			if !equalStrings(r.models, []string{"gemini-2.5-flash"}) {
				t.Fatalf("got models %v, want [gemini-2.5-flash]", r.models)
			}
			if req.Header.Get("x-goog-api-key") != tc.val {
				t.Errorf("header = %q, want %q", req.Header.Get("x-goog-api-key"), tc.val)
			}
		})
	}
}

func TestDiscoverGeminiModels_UpstreamError(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "valid-key")

	ts := geminiModelsTestServer(t, http.StatusForbidden, `{"error":{"message":"API key not valid"}}`, nil)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGeminiModels()
	if !r.fallback || len(r.models) != 0 {
		t.Fatalf("upstream error must yield empty fallback result, got %+v", r)
	}

	// queryCLIModels must fall back to the static catalog
	q := s.queryCLIModels("gemini")
	if !q.fallback {
		t.Fatal("queryCLIModels on upstream error must report fallback=true")
	}
	if !equalStrings(q.models, geminiStaticModels) {
		t.Fatalf("expected static fallback models %v, got %v", geminiStaticModels, q.models)
	}
}

func TestDiscoverGeminiModels_MalformedJSON(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "valid-key")

	ts := geminiModelsTestServer(t, http.StatusOK, `{"models": [not-valid-json`, nil)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGeminiModels()
	if !r.fallback || len(r.models) != 0 {
		t.Fatalf("malformed JSON must yield empty fallback result, got %+v", r)
	}
}

func TestDiscoverGeminiModels_EmptyCatalog(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "valid-key")

	ts := geminiModelsTestServer(t, http.StatusOK, `{"models": []}`, nil)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGeminiModels()
	if !r.fallback || len(r.models) != 0 {
		t.Fatalf("empty catalog must yield fallback result, got %+v", r)
	}
}

func TestDiscoverGeminiModels_NoGenerativeModels(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "valid-key")

	respBody := `{"models": [{"name": "models/text-embedding-004", "supportedGenerationMethods": ["embedContent"]}]}`
	ts := geminiModelsTestServer(t, http.StatusOK, respBody, nil)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGeminiModels()
	if !r.fallback || len(r.models) != 0 {
		t.Fatalf("catalog with no generateContent models must yield fallback result, got %+v", r)
	}
}

func TestFetchGeminiModels_Success(t *testing.T) {
	respBody := `{"models": [{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": ["generateContent"]}]}`
	ts := geminiModelsTestServer(t, http.StatusOK, respBody, nil)
	redirectGeminiEndpoint(t, ts.URL)

	models, err := fetchGeminiModels("direct-key")
	if err != nil {
		t.Fatalf("fetchGeminiModels failed: %v", err)
	}
	if !equalStrings(models, []string{"gemini-2.5-flash"}) {
		t.Fatalf("got models %v, want [gemini-2.5-flash]", models)
	}
}

func TestFetchGeminiModels_UpstreamError(t *testing.T) {
	ts := geminiModelsTestServer(t, http.StatusInternalServerError, `server error`, nil)
	redirectGeminiEndpoint(t, ts.URL)

	models, err := fetchGeminiModels("direct-key")
	if err == nil {
		t.Fatal("expected error on 500 upstream, got nil")
	}
	if len(models) != 0 {
		t.Fatalf("expected nil or empty models on error, got %v", models)
	}
}

func TestFetchGeminiModels_NetworkError(t *testing.T) {
	// Point at an address that refuses connections to exercise client.Do error path
	redirectGeminiEndpoint(t, "http://127.0.0.1:0")

	models, err := fetchGeminiModels("direct-key")
	if err == nil {
		t.Fatal("expected network error on unreachable endpoint, got nil")
	}
	if len(models) != 0 {
		t.Fatalf("expected nil or empty models on error, got %v", models)
	}
}

// --- Gemini CLI aliases (auto, pro, flash, flash-lite) ---
//
// The Gemini CLI defines its own aliases, like Claude's opus/sonnet/haiku/fable.
// The Google models API never lists them, so without geminiAlwaysIncludeModels
// a hive with a Gemini API key would serve a list without them and the
// dashboard's auto-heal would move an agent off an alias.

func TestGeminiAliasesAreTheCLIAliases(t *testing.T) {
	want := []string{"auto", "pro", "flash", "flash-lite"}
	if !equalStrings(geminiAlwaysIncludeModels, want) {
		t.Fatalf("geminiAlwaysIncludeModels = %v, want %v", geminiAlwaysIncludeModels, want)
	}
	for _, alias := range geminiAlwaysIncludeModels {
		if !contains(geminiStaticModels, alias) {
			t.Errorf("geminiStaticModels is missing alias %q: %v", alias, geminiStaticModels)
		}
		if !contains(geminiPinnedCLIModels, alias) {
			t.Errorf("geminiPinnedCLIModels is missing alias %q: %v", alias, geminiPinnedCLIModels)
		}
	}
}

func TestGeminiAliasesSurvivePinnedFilter(t *testing.T) {
	got := filterPinnedCLIModels("gemini", geminiAlwaysIncludeModels)
	if !equalStrings(got, geminiAlwaysIncludeModels) {
		t.Fatalf("filterPinnedCLIModels dropped Gemini aliases: got %v, want %v", got, geminiAlwaysIncludeModels)
	}
	got = filterPinnedCLIModels("gemini", geminiStaticModels)
	if !equalStrings(got, geminiStaticModels) {
		t.Fatalf("filterPinnedCLIModels changed the Gemini static list: got %v, want %v", got, geminiStaticModels)
	}
}

func TestQueryCLIModels_GeminiAliasesOnLivePath(t *testing.T) {
	clearGeminiEnv(t)
	t.Setenv("GEMINI_API_KEY", "test-gemini-secret-key")
	respBody := `{
		"models": [
			{"name": "models/gemini-2.5-pro", "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemini-3.6-flash", "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemini-3-flash-preview", "supportedGenerationMethods": ["generateContent"]}
		]
	}`
	ts := geminiModelsTestServer(t, http.StatusOK, respBody, nil)
	redirectGeminiEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("gemini")
	if r.fallback {
		t.Fatalf("live probe must not be fallback: %+v", r)
	}
	want := append([]string{"gemini-2.5-pro", "gemini-3-flash-preview"}, geminiAlwaysIncludeModels...)
	if !equalStrings(r.models, want) {
		t.Fatalf("served gemini list = %v, want %v (pinned filter still drops gemini-3.6-flash, aliases appended after it)", r.models, want)
	}
	// The cached authoritative list is what validation and the dashboard's
	// auto-heal read; the aliases are in it, so an agent on an alias is never
	// seen as "missing from discovery" and healed away.
	ids := s.modelIDsForBackend("gemini")
	for _, alias := range geminiAlwaysIncludeModels {
		if !contains(ids, alias) {
			t.Fatalf("alias %q missing from the authoritative gemini ids: %v", alias, ids)
		}
	}
	for _, alias := range geminiAlwaysIncludeModels {
		if w := s.unknownModelIDWarning("gemini", alias); w != "" {
			t.Errorf("unknownModelIDWarning(gemini, %q) = %q, want no warning", alias, w)
		}
	}
}

func TestQueryCLIModels_GeminiAliasesOnFallbackPath(t *testing.T) {
	clearGeminiEnv(t)
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("gemini")
	if !r.fallback {
		t.Fatalf("gemini with no key must be a fallback, got %+v", r)
	}
	for _, alias := range geminiAlwaysIncludeModels {
		if !contains(r.models, alias) {
			t.Fatalf("alias %q missing from the gemini fallback list: %v", alias, r.models)
		}
	}
	for _, alias := range geminiAlwaysIncludeModels {
		if w := s.unknownModelIDWarning("gemini", alias); w != "" {
			t.Errorf("unknownModelIDWarning(gemini, %q) = %q, want no warning", alias, w)
		}
	}
}

func TestStaticCLIModelResult_GeminiAliases(t *testing.T) {
	s := &Server{logger: testLogger()}
	r := s.staticCLIModelResult("gemini")
	for _, alias := range geminiAlwaysIncludeModels {
		if !contains(r.models, alias) {
			t.Fatalf("alias %q missing from the boot gemini list: %v", alias, r.models)
		}
	}
}
