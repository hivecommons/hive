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

	// Also verify end-to-end queryCLIModels returns the live discovered models
	res := s.queryCLIModels("gemini")
	if res.fallback {
		t.Fatal("queryCLIModels must report fallback=false when discovery succeeds")
	}
	if !equalStrings(res.models, wantModels) {
		t.Fatalf("queryCLIModels returned %v, want %v", res.models, wantModels)
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
