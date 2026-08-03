package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func doPutRaw(s *Server, path, raw string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doPostRaw(s *Server, path, raw string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	return rec
}

// ---- handleAuthorizedUsersList ----

func TestCovBR_AuthorizedUsersList(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"owner1", "reader1:read", "  ", ":onlyrole", "namedowner:owner"}
	rec := doGet(s, "/api/config/authorized-users")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["enforced"] != true {
		t.Fatalf("expected enforced=true")
	}
	users, _ := body["users"].([]any)
	if len(users) == 0 {
		t.Fatalf("expected some users")
	}
}

// ---- handleVariableDelete round-trip ----

func TestCovBR_VariableDelete_RoundTrip(t *testing.T) {
	s := covApiServer(t)
	// Upsert a static var, then delete it (found path).
	if rec := doPut(s, "/api/config/variables/OKVAR", map[string]any{"type": "static", "value": "v"}); rec.Code != http.StatusOK {
		t.Fatalf("seed static: %d", rec.Code)
	}
	if rec := doDelete(s, "/api/config/variables/OKVAR", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete static: want 200, got %d", rec.Code)
	}
}

// ---- handleConfigGitHub ----

func TestCovBR_ConfigGitHub_NoFields(t *testing.T) {
	s := covApiServer(t)
	if rec := doPut(s, "/api/config/github", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no fields: want 400, got %d", rec.Code)
	}
}

func TestCovBR_ConfigGitHub_BadBody(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutRaw(s, "/api/config/github", "not-json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: want 400, got %d", rec.Code)
	}
}

func TestCovBR_ConfigGitHub_KeyFileAndIDs(t *testing.T) {
	s := covApiServer(t)
	// The handler now refuses updates it cannot persist (#2459), so the
	// config needs a real source path.
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	appID := int64(123)
	instID := int64(456)
	rec := doPut(s, "/api/config/github", map[string]any{
		"app_id":          appID,
		"installation_id": instID,
		"key_file":        "/tmp/does-not-need-to-exist.pem",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("key_file + ids: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["app_id"] != float64(123) {
		t.Fatalf("app_id not applied: %v", body["app_id"])
	}
}

func TestCovBR_ConfigGitHub_PrivateKeyWrite(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	keyPath := filepath.Join(t.TempDir(), "sub", "gh-app-key.pem")
	rec := doPut(s, "/api/config/github", map[string]any{
		"private_key": "-----BEGIN KEY-----\nabc\n-----END KEY-----",
		"key_file":    keyPath,
		"app_id":      int64(1),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("private key write: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// ---- handleInferenceModels entitled path ----

func TestCovBR_InferenceModels_BackendRequired(t *testing.T) {
	s := covApiServer(t)
	// The route requires a backend path value; hitting an unknown backend → 404.
	if rec := doGet(s, "/api/inference/models/unknownbackend"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown backend: want 404, got %d", rec.Code)
	}
}

func TestCovBR_InferenceModels_Entitled(t *testing.T) {
	s := covApiServer(t)

	// Mock a litellm /v1/models endpoint advertising a superset.
	modelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "gpt-4o"}, {"id": "claude-opus"}, {"id": "unentitled-model"},
			},
		})
	}))
	defer modelSrv.Close()

	s.SetInferenceEndpoints(map[string][]string{"litellm": {modelSrv.URL}})

	// Inject an entitled-models provider narrowing to a subset.
	SetEntitledModelsProvider(func(endpoint string) ([]string, string, bool) {
		return []string{"gpt-4o", "claude-opus"}, "key-info", true
	})
	defer SetEntitledModelsProvider(nil)

	rec := doGet(s, "/api/inference/models/litellm")
	if rec.Code != http.StatusOK {
		t.Fatalf("litellm models: want 200, got %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["entitled"] != true {
		t.Fatalf("expected entitled=true, got %v (%s)", body["entitled"], rec.Body.String())
	}
}

// ---- inferenceAPIKey / entitledModelsFor direct ----

func TestCovBR_InferenceAPIKey_Backends(t *testing.T) {
	s := covApiServer(t)
	// vllm / llm-d / default all resolve without panicking (empty is fine).
	_ = s.inferenceAPIKey("vllm")
	_ = s.inferenceAPIKey("llm-d")
	_ = s.inferenceAPIKey("litellm")
	// nil deps guard.
	var s2 Server
	if k := s2.inferenceAPIKey("vllm"); k != "" {
		t.Fatalf("nil-deps key should be empty")
	}
}

func TestCovBR_EntitledModelsFor(t *testing.T) {
	s := covApiServer(t)
	// Non-litellm backend → not known.
	if _, _, ok := s.entitledModelsFor("vllm", []string{"http://x"}); ok {
		t.Fatalf("vllm should not be entitled-known")
	}
	// litellm with no provider fn → not known.
	SetEntitledModelsProvider(nil)
	if _, _, ok := s.entitledModelsFor("litellm", []string{"http://x"}); ok {
		t.Fatalf("no provider → not known")
	}
	// litellm with provider returning known.
	SetEntitledModelsProvider(func(string) ([]string, string, bool) { return []string{"m1"}, "src", true })
	defer SetEntitledModelsProvider(nil)
	if m, src, ok := s.entitledModelsFor("litellm", []string{"http://x"}); !ok || len(m) != 1 || src != "src" {
		t.Fatalf("expected entitled known, got %v %v %v", m, src, ok)
	}
}

func TestCovBR_IntersectEntitled(t *testing.T) {
	got := intersectEntitled(
		[]string{"gpt-4o", "azure/claude", "orphan"},
		[]string{"gpt-4o", "vendor/claude"},
	)
	// gpt-4o full match; azure/claude tail-matches vendor/claude; orphan excluded.
	if len(got) != 2 {
		t.Fatalf("intersect: expected 2, got %v", got)
	}
}

// ---- handleBudgetIgnoreSet variants ----

func TestCovBR_BudgetIgnoreSet(t *testing.T) {
	s := covApiServer(t)
	// bool form
	if rec := doPost(s, "/api/budget-ignore", map[string]any{"ignored": true}); rec.Code != http.StatusOK {
		t.Fatalf("bool form: %d", rec.Code)
	}
	// list form
	if rec := doPost(s, "/api/budget-ignore", map[string]any{"ignored": []string{"scanner"}}); rec.Code != http.StatusOK {
		t.Fatalf("list form: %d", rec.Code)
	}
	// invalid inner value (number) → 400
	if rec := doPostRaw(s, "/api/budget-ignore", `{"ignored": 5}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid inner: want 400, got %d", rec.Code)
	}
	// invalid body → 400
	if rec := doPostRaw(s, "/api/budget-ignore", `nope`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body: want 400, got %d", rec.Code)
	}
}

// ---- handleTokens / handleTokenAccess / handleModelAdvisor / handleIssueCosts ----

func TestCovBR_TokensAndAdvisor(t *testing.T) {
	s := covApiServer(t)
	if rec := doGet(s, "/api/tokens"); rec.Code != http.StatusOK {
		t.Fatalf("tokens: %d", rec.Code)
	}
	if rec := doGet(s, "/api/token-access"); rec.Code != http.StatusOK {
		t.Fatalf("token-access: %d", rec.Code)
	}
	if rec := doGet(s, "/api/model-advisor"); rec.Code != http.StatusOK {
		t.Fatalf("model-advisor: %d", rec.Code)
	}
	if rec := doGet(s, "/api/issue-costs"); rec.Code != http.StatusOK {
		t.Fatalf("issue-costs: %d", rec.Code)
	}
	if rec := doGet(s, "/api/budget-ignore"); rec.Code != http.StatusOK {
		t.Fatalf("budget-ignore get: %d", rec.Code)
	}
}

// ---- handleRestart / handleResetRestarts (agent manager errors) ----

func TestCovBR_RestartUnknownAgent(t *testing.T) {
	s := covApiServer(t)
	// Unknown agent → manager.Restart errors → 400.
	if rec := doPost(s, "/api/restart/ghostagent", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("restart unknown: want 400, got %d", rec.Code)
	}
	if rec := doPost(s, "/api/reset-restarts/ghostagent", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("reset unknown: want 400, got %d", rec.Code)
	}
}
