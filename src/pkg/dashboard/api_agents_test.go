package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ---------- agentDefinition JSON serialization ----------

func TestAgentDefinitionJSONTags(t *testing.T) {
	def := agentDefinition{
		APIVersion: "hive.kubestellar.io/v1",
		Kind:       "AgentDefinition",
		Metadata: agentDefinitionMeta{
			Name:        "scanner",
			DisplayName: "Scanner Agent",
			Description: "Scans for issues",
			Emoji:       "🔍",
			Color:       "#ff0000",
		},
		Spec: agentDefinitionSpec{
			Backend:         "claude",
			Model:           "claude-sonnet-4-6",
			Role:            "scanner",
			Mode:            "autonomous",
			SortOrder:       1,
			BeadRole:        "worker",
			StaleTimeout:    300,
			RestartStrategy: "immediate",
			ClearOnKick:     true,
			IncludeRepos:    true,
			LaneKeywords:    []string{"bug", "fix"},
			DetectKeywords:  []string{"error"},
			Aliases:         []string{"scan"},
			Cadences:        map[string]config.Cadence{"busy": "5m"},
			PromptTemplate:  "do the thing",
		},
	}

	data, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Verify lowercase JSON keys (the recently-added json tags)
	checks := map[string]string{
		`"apiVersion"`:      "apiVersion field",
		`"kind"`:            "kind field",
		`"metadata"`:        "metadata field",
		`"spec"`:            "spec field",
		`"name"`:            "metadata.name field",
		`"displayName"`:     "metadata.displayName field",
		`"description"`:     "metadata.description field",
		`"emoji"`:           "metadata.emoji field",
		`"color"`:           "metadata.color field",
		`"backend"`:         "spec.backend field",
		`"model"`:           "spec.model field",
		`"role"`:            "spec.role field",
		`"mode"`:            "spec.mode field",
		`"sortOrder"`:       "spec.sortOrder field",
		`"beadRole"`:        "spec.beadRole field",
		`"staleTimeout"`:    "spec.staleTimeout field",
		`"restartStrategy"`: "spec.restartStrategy field",
		`"clearOnKick"`:     "spec.clearOnKick field",
		`"includeRepos"`:    "spec.includeRepos field",
		`"laneKeywords"`:    "spec.laneKeywords field",
		`"detectKeywords"`:  "spec.detectKeywords field",
		`"aliases"`:         "spec.aliases field",
		`"cadences"`:        "spec.cadences field",
		`"promptTemplate"`:  "spec.promptTemplate field",
	}

	body := string(data)
	for needle, desc := range checks {
		if !strings.Contains(body, needle) {
			t.Errorf("JSON output missing %s (looked for %s)", desc, needle)
		}
	}

	// Roundtrip: unmarshal back and compare
	var decoded agentDefinition
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.Metadata.Name != "scanner" {
		t.Errorf("roundtrip metadata.name = %q, want %q", decoded.Metadata.Name, "scanner")
	}
	if decoded.Spec.Backend != "claude" {
		t.Errorf("roundtrip spec.backend = %q, want %q", decoded.Spec.Backend, "claude")
	}
	if decoded.Spec.SortOrder != 1 {
		t.Errorf("roundtrip spec.sortOrder = %d, want 1", decoded.Spec.SortOrder)
	}
}

func TestAgentDefinitionJSONOmitsEmpty(t *testing.T) {
	def := agentDefinition{
		APIVersion: "v1",
		Kind:       "AgentDefinition",
		Metadata:   agentDefinitionMeta{Name: "worker"},
		Spec:       agentDefinitionSpec{},
	}

	data, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	body := string(data)
	// Omitempty fields should not appear when zero-valued
	shouldBeAbsent := []string{
		`"displayName"`, `"description"`, `"emoji"`, `"color"`,
		`"laneKeywords"`, `"detectKeywords"`, `"aliases"`, `"cadences"`,
		`"promptTemplate"`, `"role"`, `"mode"`, `"backend"`, `"model"`,
	}
	for _, needle := range shouldBeAbsent {
		if strings.Contains(body, needle) {
			t.Errorf("JSON output should omit empty field %s", needle)
		}
	}
}

// ---------- GHCR tag check ----------

func TestGhcrTagExists_MockTokenEndpoint(t *testing.T) {
	// We test with a fake GHCR server to cover both success and failure paths.
	// The real ghcrTagExists uses hardcoded URLs, so we test the cached wrapper instead.

	// Clear cache before test
	ghcrCacheMu.Lock()
	ghcrCacheResult = map[string]bool{}
	ghcrCacheExpiry = map[string]time.Time{}
	ghcrCacheMu.Unlock()

	// Cache a "true" result
	ghcrCacheMu.Lock()
	ghcrCacheResult["v2-abc1234"] = true
	ghcrCacheExpiry["v2-abc1234"] = time.Now().Add(ghcrCacheTTL)
	ghcrCacheMu.Unlock()

	if !ghcrTagExistsCached("v2-abc1234") {
		t.Error("expected cached result to be true for v2-abc1234")
	}

	// Cache a "false" result
	ghcrCacheMu.Lock()
	ghcrCacheResult["v2-missing"] = false
	ghcrCacheExpiry["v2-missing"] = time.Now().Add(ghcrCacheTTL)
	ghcrCacheMu.Unlock()

	if ghcrTagExistsCached("v2-missing") {
		t.Error("expected cached result to be false for v2-missing")
	}

	// Cleanup
	ghcrCacheMu.Lock()
	ghcrCacheResult = map[string]bool{}
	ghcrCacheExpiry = map[string]time.Time{}
	ghcrCacheMu.Unlock()
}

func TestGhcrTagExists_ExpiredCacheFallsThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	ghcrCacheMu.Lock()
	ghcrCacheResult["v2-expired"] = true
	ghcrCacheExpiry["v2-expired"] = time.Now().Add(-1) // already expired
	ghcrCacheMu.Unlock()

	// This will try the real endpoint and likely fail (tag doesn't exist),
	// but it exercises the cache-miss + network path
	_ = ghcrTagExistsCached("v2-expired")

	// Cleanup
	ghcrCacheMu.Lock()
	delete(ghcrCacheResult, "v2-expired")
	delete(ghcrCacheExpiry, "v2-expired")
	ghcrCacheMu.Unlock()
}

// ---------- Agent CRUD handlers ----------

func TestHandleAgentsList(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var agents []agentListEntry
	if err := json.NewDecoder(rec.Body).Decode(&agents); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// testDeps creates one agent: "scanner"
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name != "scanner" {
		t.Errorf("expected agent name %q, got %q", "scanner", agents[0].Name)
	}
	if agents[0].Backend != "claude" {
		t.Errorf("expected backend %q, got %q", "claude", agents[0].Backend)
	}
}

func TestHandleAgentCreate_MissingName(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents", map[string]interface{}{
		"agent": map[string]interface{}{"backend": "claude"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentCreate_InvalidName(t *testing.T) {
	s, _ := apiServer(t)

	cases := []string{"has space", "../path-traversal", "with/slash", "with\\backslash"}
	for _, name := range cases {
		rec := doPost(s, "/api/agents", map[string]interface{}{
			"name":  name,
			"agent": map[string]interface{}{"backend": "claude"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q: expected 400, got %d", name, rec.Code)
		}
	}
}

func TestHandleAgentCreate_NameTooLong(t *testing.T) {
	s, _ := apiServer(t)
	const maxAgentNameLen = 64
	longName := strings.Repeat("a", maxAgentNameLen+1)
	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  longName,
		"agent": map[string]interface{}{"backend": "claude"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentCreate_Duplicate(t *testing.T) {
	s, _ := apiServer(t)
	// "scanner" already exists in testDeps
	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  "scanner",
		"agent": map[string]interface{}{"backend": "copilot"},
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict, got %d", rec.Code)
	}
}

func TestHandleAgentCreate_NoAgentsDir(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = "" // ensure no agents_dir
	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  "new-agent",
		"agent": map[string]interface{}{"backend": "copilot"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestHandleAgentCreate_Success(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir

	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  "new-worker",
		"agent": map[string]interface{}{"backend": "copilot", "model": "gpt-4o"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify agent was added to config
	if _, ok := deps.Config.Agents["new-worker"]; !ok {
		t.Error("new-worker should be in config after creation")
	}

	// Verify agent file was written
	agentFile := filepath.Join(dir, "new-worker.yaml")
	if _, err := os.Stat(agentFile); os.IsNotExist(err) {
		t.Errorf("expected agent file at %s", agentFile)
	}
}

func TestHandleAgentDelete_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doDelete(s, "/api/agents/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleAgentDelete_NotManaged(t *testing.T) {
	s, _ := apiServer(t)
	// "scanner" from testDeps is not managed
	rec := doDelete(s, "/api/agents/scanner", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestHandleAgentDelete_Success(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir

	// First create a managed agent
	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  "temp-agent",
		"agent": map[string]interface{}{"backend": "copilot"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Delete it
	rec = doDelete(s, "/api/agents/temp-agent", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, ok := deps.Config.Agents["temp-agent"]; ok {
		t.Error("temp-agent should be removed from config after deletion")
	}
}

// ---------- Agent Import handlers ----------

func TestHandleAgentImport_InvalidBody(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/import",
		strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	// Import is owner-only (audit F16). This test is about body validation, not
	// authorization, so give it credentials and keep asserting 400.
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_InvalidSource(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source": "invalid",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_PasteEmptyContent(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_PasteInvalidYAML(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": "not: valid: yaml: [",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_PasteWrongKind(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": "apiVersion: v1\nkind: WrongKind\nmetadata:\n  name: test\nspec:\n  backend: claude\n",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_PasteMissingName(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": fmt.Sprintf("apiVersion: v1\nkind: %s\nmetadata:\n  name: \"\"\nspec:\n  backend: claude\n", exportKind),
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_Preview(t *testing.T) {
	s, _ := apiServer(t)
	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: imported-agent
  displayName: Imported Agent
spec:
  backend: claude
  model: claude-sonnet-4-6
  role: scanner
`, exportKind)

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": yamlContent,
		"preview": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	result := decodeJSON(t, rec)
	if result["ok"] != true {
		t.Error("expected ok=true in preview response")
	}
	parsed, ok := result["parsed"].(map[string]interface{})
	if !ok {
		t.Fatal("expected parsed to be a map")
	}
	meta, ok := parsed["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("expected metadata to be a map")
	}
	if meta["name"] != "imported-agent" {
		t.Errorf("expected name=%q, got %q", "imported-agent", meta["name"])
	}
}

func TestHandleAgentImport_Duplicate(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: scanner
spec:
  backend: claude
`, exportKind)

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": yamlContent,
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentImport_InvalidAgentName(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: "../path-traversal"
spec:
  backend: claude
`, exportKind)

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": yamlContent,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentImport_NoAgentsDir(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = ""

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: fresh-agent
spec:
  backend: claude
`, exportKind)

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": yamlContent,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentImport_Success(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: imported-worker
  displayName: Imported Worker
  emoji: "🤖"
spec:
  backend: copilot
  model: gpt-4o
  role: worker
  staleTimeout: 600
  restartStrategy: immediate
`, exportKind)

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":  "paste",
		"content": yamlContent,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, ok := deps.Config.Agents["imported-worker"]; !ok {
		t.Error("imported-worker should be in config after import")
	}

	agentFile := filepath.Join(dir, "imported-worker.yaml")
	if _, err := os.Stat(agentFile); os.IsNotExist(err) {
		t.Errorf("expected agent file at %s", agentFile)
	}
}

func TestHandleAgentImport_URLEmptyURL(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source": "url",
		"url":    "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_URLTooLong(t *testing.T) {
	s, _ := apiServer(t)
	longURL := "https://example.com/" + strings.Repeat("a", importMaxURLLen)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source": "url",
		"url":    longURL,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentImport_URLFetchFromServer(t *testing.T) {
	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: url-imported
spec:
  backend: claude
`, exportKind)

	// Start a test server that serves YAML
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.Write([]byte(yamlContent))
	}))
	defer ts.Close()
	routeImportFetchesToServer(t, ts)

	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source": "url",
		"url":    "https://raw.githubusercontent.com/hivecommons/hive/main/agent.yaml",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, ok := deps.Config.Agents["url-imported"]; !ok {
		t.Error("url-imported should be in config after URL import")
	}
}

func TestHandleAgentImport_URLFetchFails(t *testing.T) {
	// Server that returns 500
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	routeImportFetchesToServer(t, ts)

	s, _ := apiServer(t)
	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source": "url",
		"url":    "https://raw.githubusercontent.com/hivecommons/hive/main/agent.yaml",
	})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------- Agent Export handler ----------

func TestHandleAgentExport_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/nonexistent/export")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleAgentExport_JSON(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/agent/scanner/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	result := decodeJSON(t, rec)
	if result["name"] != "scanner" {
		t.Errorf("expected name=scanner, got %v", result["name"])
	}
	yamlStr, ok := result["yaml"].(string)
	if !ok || yamlStr == "" {
		t.Error("expected non-empty yaml field")
	}
	if !strings.Contains(yamlStr, "kind: AgentDefinition") {
		t.Error("yaml should contain 'kind: AgentDefinition'")
	}
}

func TestHandleAgentExport_YAML(t *testing.T) {
	s, _ := apiServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config/agent/scanner/export", nil)
	req.Header.Set("Accept", "text/yaml")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/yaml") {
		t.Errorf("expected text/yaml content-type, got %s", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "kind: AgentDefinition") {
		t.Error("YAML response should contain 'kind: AgentDefinition'")
	}
}

// ---------- reInitSubsystems ----------

func TestReInitSubsystems_NilDeps(t *testing.T) {
	s := &Server{}
	// Should not panic when deps is nil
	s.reInitSubsystems()
}

func TestReInitSubsystems_NilFunc(t *testing.T) {
	s := &Server{deps: &Dependencies{}}
	// Should not panic when ReInitFunc is nil
	s.reInitSubsystems()
}

func TestReInitSubsystems_Called(t *testing.T) {
	called := false
	s := &Server{deps: &Dependencies{
		ReInitFunc: func() { called = true },
	}}
	s.reInitSubsystems()
	if !called {
		t.Error("expected ReInitFunc to be called")
	}
}

// ---------- valueOrDefault ----------

func TestValueOrDefault(t *testing.T) {
	if got := valueOrDefault("hello", "default"); got != "hello" {
		t.Errorf("expected %q, got %q", "hello", got)
	}
	if got := valueOrDefault("", "fallback"); got != "fallback" {
		t.Errorf("expected %q, got %q", "fallback", got)
	}
}
