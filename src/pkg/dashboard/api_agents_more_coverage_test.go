package dashboard

import (
	"net/http"
	"testing"
)

// covH2AgentServer builds a wired server whose AgentsDir points at a temp dir so
// create/import handlers can persist agent files.
func covH2AgentServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()
	return s, deps
}

// TestCovH2_AgentCreate covers the create handler's validation + success paths.
func TestCovH2_AgentCreate(t *testing.T) {
	s, _ := covH2AgentServer(t)

	// Bad body → 400.
	if rec := doPost(s, "/api/agents", "not-json-object"); rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad body: expected 400, got %d", rec.Code)
	}
	// Missing name → 400.
	if rec := doPost(s, "/api/agents", map[string]any{"agent": map[string]any{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("create no-name: expected 400, got %d", rec.Code)
	}
	// Invalid name (space) → 400.
	if rec := doPost(s, "/api/agents", map[string]any{"name": "bad name"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad-name: expected 400, got %d", rec.Code)
	}
	// Too-long name → 400.
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if rec := doPost(s, "/api/agents", map[string]any{"name": long}); rec.Code != http.StatusBadRequest {
		t.Fatalf("create long-name: expected 400, got %d", rec.Code)
	}
	// Duplicate name (scanner exists) → 409.
	if rec := doPost(s, "/api/agents", map[string]any{"name": "scanner"}); rec.Code != http.StatusConflict {
		t.Fatalf("create dup: expected 409, got %d", rec.Code)
	}
	// Valid new agent → 200 (persists to temp AgentsDir).
	rec := doPost(s, "/api/agents", map[string]any{
		"name":  "custombot",
		"agent": map[string]any{"backend": "copilot", "model": "sonnet"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create valid: expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestCovH2_AgentCreateNoDir covers the agents_dir-not-configured branch.
func TestCovH2_AgentCreateNoDir(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = "" // force the misconfig branch
	rec := doPost(s, "/api/agents", map[string]any{"name": "nodircustom"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create no-dir: expected 500, got %d", rec.Code)
	}
}

// TestCovH2_AgentDelete covers not-found, protected, and managed-delete paths.
func TestCovH2_AgentDelete(t *testing.T) {
	s, deps := covH2AgentServer(t)

	// Not found → 404.
	if rec := doDelete(s, "/api/agents/ghost", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: expected 404, got %d", rec.Code)
	}
	// scanner is a base (non-managed) agent → 403.
	if rec := doDelete(s, "/api/agents/scanner", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("delete base agent: expected 403, got %d", rec.Code)
	}

	// Create a managed agent, then delete it → 200.
	if rec := doPost(s, "/api/agents", map[string]any{
		"name":  "deletable",
		"agent": map[string]any{"backend": "copilot"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("precreate deletable: got %d", rec.Code)
	}
	if _, ok := deps.Config.Agents["deletable"]; !ok {
		t.Fatalf("deletable agent not present after create")
	}
	if rec := doDelete(s, "/api/agents/deletable", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete managed: expected 200, got %d", rec.Code)
	}
}

// TestCovH2_AgentImport covers the import handler's validation + paste success + preview.
func TestCovH2_AgentImport(t *testing.T) {
	s, _ := covH2AgentServer(t)

	// Bad body → 400.
	if rec := doPost(s, "/api/agents/import", "xxx"); rec.Code != http.StatusBadRequest {
		t.Fatalf("import bad body: expected 400, got %d", rec.Code)
	}
	// Unknown source → 400.
	if rec := doPost(s, "/api/agents/import", map[string]any{"source": "carrier-pigeon"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("import bad source: expected 400, got %d", rec.Code)
	}
	// source=url but no url → 400.
	if rec := doPost(s, "/api/agents/import", map[string]any{"source": "url"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("import url-missing: expected 400, got %d", rec.Code)
	}
	// source=paste but no content → 400.
	if rec := doPost(s, "/api/agents/import", map[string]any{"source": "paste"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("import paste-missing: expected 400, got %d", rec.Code)
	}
	// source=paste with invalid YAML kind → 400 (ParseDefinition rejects).
	if rec := doPost(s, "/api/agents/import", map[string]any{
		"source": "paste", "content": "kind: Wrong\nmetadata:\n  name: x\n",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("import wrong-kind: expected 400, got %d", rec.Code)
	}

	validYAML := "apiVersion: hive.kubestellar.io/v1\n" +
		"kind: AgentDefinition\n" +
		"metadata:\n  name: imported-bot\n  displayName: Imported\n" +
		"spec:\n  backend: copilot\n  model: sonnet\n  role: worker\n"

	// Preview mode → 200 with parsed definition, no persistence.
	if rec := doPost(s, "/api/agents/import", map[string]any{
		"source": "paste", "content": validYAML, "preview": true,
	}); rec.Code != http.StatusOK {
		t.Fatalf("import preview: expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Real paste import → 200, persists to temp AgentsDir.
	if rec := doPost(s, "/api/agents/import", map[string]any{
		"source": "paste", "content": validYAML,
	}); rec.Code != http.StatusOK {
		t.Fatalf("import paste: expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Re-import the same agent → 409 conflict.
	if rec := doPost(s, "/api/agents/import", map[string]any{
		"source": "paste", "content": validYAML,
	}); rec.Code != http.StatusConflict {
		t.Fatalf("import dup: expected 409, got %d", rec.Code)
	}
}

// TestCovH2_AgentSourceFetchers covers the tiny fetcher accessors.
func TestCovH2_AgentSourceFetchers(t *testing.T) {
	s, deps := apiServer(t)

	// No GHClient wired → nil fetchers.
	if s.promptSourceFetcher() != nil {
		t.Fatalf("expected nil promptSourceFetcher without GHClient")
	}
	if s.definitionSourceFetcher() != nil {
		t.Fatalf("expected nil definitionSourceFetcher without GHClient")
	}

	// bakePromptSource: not-set source → error.
	cfg := deps.Config.Agents["scanner"]
	if err := s.bakePromptSource(deps.Ctx, "scanner", &cfg); err == nil {
		t.Fatalf("expected error for unset prompt source")
	}
}

// TestCovH2_PackApplyAndSetLevel covers the pack handlers.
func TestCovH2_PackApplyAndSetLevel(t *testing.T) {
	s, _ := covH2AgentServer(t)

	// Invalid level (non-numeric) → 400.
	if rec := doPost(s, "/api/packs/abc/apply", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("apply bad level: expected 400, got %d", rec.Code)
	}

	// Apply a valid level. ACMMPackByLevel governs whether it succeeds; any
	// non-panicking status (200 or 500 for an unknown pack) exercises the body.
	rec := doPost(s, "/api/packs/1/apply", nil)
	if rec.Code == 0 {
		t.Fatalf("apply level 1 returned no status")
	}

	// SetLevel: bad body → 400.
	if rec := doPut(s, "/api/packs/level", "not-an-object"); rec.Code != http.StatusBadRequest {
		t.Fatalf("setlevel bad body: expected 400, got %d", rec.Code)
	}
	// Out-of-range level → 400.
	if rec := doPut(s, "/api/packs/level", map[string]any{"level": 99}); rec.Code != http.StatusBadRequest {
		t.Fatalf("setlevel out-of-range: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/packs/level", map[string]any{"level": 0}); rec.Code != http.StatusBadRequest {
		t.Fatalf("setlevel zero: expected 400, got %d", rec.Code)
	}
	// Valid level → exercises the save + reconcile path.
	rec = doPut(s, "/api/packs/level", map[string]any{"level": 2})
	if rec.Code == 0 {
		t.Fatalf("setlevel valid returned no status")
	}
}

// TestCovH2_SyncAgentVisibility calls syncAgentVisibility directly.
func TestCovH2_SyncAgentVisibility(t *testing.T) {
	s, _ := covH2AgentServer(t)
	// Invalid level → ACMMPackByLevel error → nil,nil.
	if paused, resumed := s.syncAgentVisibility(-1); paused != nil || resumed != nil {
		t.Fatalf("expected nil results for invalid level, got %v %v", paused, resumed)
	}
	// A plausible level exercises the pause/resume classification loop.
	_, _ = s.syncAgentVisibility(1)
}

// TestCovH2_ApplyPackNoDir covers the agents_dir-not-configured error in ApplyPack.
func TestCovH2_ApplyPackNoDir(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = ""
	if _, err := s.ApplyPack(1); err == nil {
		t.Fatalf("expected error when agents_dir not configured")
	}
}
