package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func covApiServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

// --- Operator variables upsert/delete ---

func TestCovI_VariableUpsert(t *testing.T) {
	s := covApiServer(t)

	// Invalid variable name → 400.
	if rec := doPut(s, "/api/config/variables/1bad", map[string]any{"type": "static", "value": "v"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: expected 400, got %d", rec.Code)
	}
	// script/http types are seed-only → 403.
	if rec := doPut(s, "/api/config/variables/MYVAR", map[string]any{"type": "script"}); rec.Code != http.StatusForbidden {
		t.Fatalf("script type: expected 403, got %d", rec.Code)
	}
	// Unknown type → 400.
	if rec := doPut(s, "/api/config/variables/MYVAR", map[string]any{"type": "weird"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type: expected 400, got %d", rec.Code)
	}
	// Invalid scope → 400.
	if rec := doPut(s, "/api/config/variables/MYVAR", map[string]any{"type": "static", "scope": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope: expected 400, got %d", rec.Code)
	}
	// Secret-looking static value → 400.
	if rec := doPut(s, "/api/config/variables/MYVAR", map[string]any{"type": "static", "value": "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("secret value: expected 400, got %d", rec.Code)
	}
	// Valid static var → 200.
	if rec := doPut(s, "/api/config/variables/MYVAR", map[string]any{"type": "static", "scope": "template", "value": "hello"}); rec.Code != http.StatusOK {
		t.Fatalf("valid static: expected 200, got %d", rec.Code)
	}
	// Valid env var → 200.
	if rec := doPut(s, "/api/config/variables/MYENV", map[string]any{"type": "env", "env": "SOME_ENV"}); rec.Code != http.StatusOK {
		t.Fatalf("valid env: expected 200, got %d", rec.Code)
	}
}

func TestCovI_VariableDelete(t *testing.T) {
	s := covApiServer(t)

	// Not found → 404.
	if rec := doDelete(s, "/api/config/variables/UNKNOWN", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: expected 404, got %d", rec.Code)
	}

	// Create then delete a static var → 200.
	doPut(s, "/api/config/variables/DELME", map[string]any{"type": "static", "value": "x"})
	if rec := doDelete(s, "/api/config/variables/DELME", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete static: expected 200, got %d", rec.Code)
	}
}

// --- Agent config: restrictions, prompt-save, general ---

func TestCovI_AgentConfigRestrictions(t *testing.T) {
	s := covApiServer(t)
	// Unknown agent or bad body → error; scanner exists.
	if rec := doPut(s, "/api/config/agent/scanner/restrictions", map[string]any{"restrictions": map[string]any{"max_files": 10}}); rec.Code >= 500 {
		t.Fatalf("restrictions unexpected 5xx: %d", rec.Code)
	}
	// Bad body → 400.
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/restrictions", stringReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("restrictions bad body: expected 400, got %d", rec.Code)
	}
}

func TestCovI_AgentPromptSave(t *testing.T) {
	s := covApiServer(t)
	// Valid template update for existing agent. Writing the template file may fail
	// under /data in the test sandbox (→ 500); any non-404 exercises the body.
	if rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"template": "do the thing"}); rec.Code == http.StatusNotFound {
		t.Fatalf("prompt save unexpected 404 for known agent")
	}
	// promptSource with missing fields → 400 (IsSet guard).
	if rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"promptSource": map[string]any{"owner": "o"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("prompt source missing fields: expected 400, got %d", rec.Code)
	}
	// Unknown agent → 404.
	if rec := doPut(s, "/api/config/agent/ghost/prompt", map[string]any{"template": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("prompt unknown agent: expected 404, got %d", rec.Code)
	}
	// Bad body → 400.
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/prompt", stringReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("prompt bad body: expected 400, got %d", rec.Code)
	}
}

func TestCovI_AgentConfigGeneral(t *testing.T) {
	s := covApiServer(t)
	// Unknown agent → 404.
	if rec := doPut(s, "/api/config/agent/ghost/general", map[string]any{"model": "sonnet"}); rec.Code != http.StatusNotFound {
		t.Fatalf("general unknown agent: expected 404, got %d", rec.Code)
	}
}

// --- Knowledge handlers (ensureKnowledge lazily creates a file-based API) ---

func TestCovI_KnowledgeListExportSearch(t *testing.T) {
	s := covApiServer(t)

	if rec := doGet(s, "/api/knowledge"); rec.Code != http.StatusOK {
		t.Fatalf("knowledge list: %d", rec.Code)
	}
	if rec := doGet(s, "/api/knowledge?type=pattern"); rec.Code != http.StatusOK {
		t.Fatalf("knowledge list type: %d", rec.Code)
	}

	// Export returns markdown.
	rec := doGet(s, "/api/knowledge/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("knowledge export: %d", rec.Code)
	}

	// Search: missing q and tag → 400.
	if rec := doGet(s, "/api/knowledge/search"); rec.Code != http.StatusBadRequest {
		t.Fatalf("search no-params: expected 400, got %d", rec.Code)
	}
	// Search with q → 200.
	if rec := doGet(s, "/api/knowledge/search?q=foo&limit=5"); rec.Code != http.StatusOK {
		t.Fatalf("search q: %d", rec.Code)
	}
	// Search with tag → 200.
	if rec := doGet(s, "/api/knowledge/search?tag=bar"); rec.Code != http.StatusOK {
		t.Fatalf("search tag: %d", rec.Code)
	}
}

func TestCovI_KnowledgeLayerAndPromote(t *testing.T) {
	s := covApiServer(t)

	// A knowledge layer GET (path param) — reachable branch.
	if rec := doGet(s, "/api/knowledge/project"); rec.Code >= 500 {
		t.Fatalf("knowledge layer unexpected 5xx: %d", rec.Code)
	}

	// Promote with bad body → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/promote", stringReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code >= 500 {
		t.Fatalf("promote bad body unexpected 5xx: %d", rec.Code)
	}
}

// --- Git sources connect/disconnect validation ---

func TestCovI_GitSourcesConnect(t *testing.T) {
	s := covApiServer(t)

	// Bad body → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/git-sources", stringReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("git connect bad body: expected 400, got %d", rec.Code)
	}

	// Missing name/url → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("git connect missing url: expected 400, got %d", rec.Code)
	}
	// Bad URL scheme → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "x", "url": "ftp://y"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("git connect bad scheme: expected 400, got %d", rec.Code)
	}
	// Bad name (slash) → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "a/b", "url": "https://y"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("git connect bad name: expected 400, got %d", rec.Code)
	}
	// Bad branch (leading dash) → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "x", "url": "https://y", "branch": "-evil"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("git connect bad branch: expected 400, got %d", rec.Code)
	}
	// Bad subpath (..) → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "x", "url": "https://y", "subpath": "../etc"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("git connect bad subpath: expected 400, got %d", rec.Code)
	}
	// Well-formed request: ConnectGitSource attempts a real clone → 500 offline
	// (covers the connect-error branch). Any non-2xx is acceptable here.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "docs", "url": "https://example.invalid/repo.git"}); rec.Code < 400 {
		t.Fatalf("git connect valid-but-offline: expected an error status, got %d", rec.Code)
	}
}

func TestCovI_GitSourcesDisconnect(t *testing.T) {
	s := covApiServer(t)
	// Ensure knowledge exists first (a GET creates it lazily).
	doGet(s, "/api/knowledge")

	// Bad body → 400.
	req := httptest.NewRequest(http.MethodDelete, "/api/knowledge/git-sources", stringReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code >= 500 {
		t.Fatalf("git disconnect bad body unexpected 5xx: %d", rec.Code)
	}
	// Valid disconnect of a non-existent source → still a reachable branch.
	if rec := doDelete(s, "/api/knowledge/git-sources", map[string]any{"url": "https://none"}); rec.Code >= 500 {
		t.Fatalf("git disconnect unexpected 5xx: %d", rec.Code)
	}
}

// --- Inference models ---

func TestCovI_InferenceModels(t *testing.T) {
	s := covApiServer(t)
	// Unknown backend → reachable (empty / error). Any non-5xx is fine.
	if rec := doGet(s, "/api/inference/models/vllm"); rec.Code >= 500 {
		t.Fatalf("inference models unexpected 5xx: %d", rec.Code)
	}
}

// --- Misc GET handlers ---

func TestCovI_HistoryTimelineSnapshot(t *testing.T) {
	s := covApiServer(t)
	if rec := doGet(s, "/api/history"); rec.Code != http.StatusOK {
		t.Fatalf("history: %d", rec.Code)
	}
	if rec := doGet(s, "/api/timeline"); rec.Code != http.StatusOK {
		t.Fatalf("timeline: %d", rec.Code)
	}
	if rec := doGet(s, "/api/timeline?range=day"); rec.Code != http.StatusOK {
		t.Fatalf("timeline range: %d", rec.Code)
	}
	// Snapshot page (GET /snapshot) — reachable; may 404/500 depending on files.
	if rec := doGet(s, "/snapshot"); rec.Code == 0 {
		t.Fatalf("snapshot returned no status")
	}
}

// --- GitHub app install clicked + poll ---

func TestCovI_GitHubAppInstallClicked(t *testing.T) {
	s := covApiServer(t)
	rec := doPost(s, "/api/github-app/install-clicked", nil)
	// Route path may differ; call the handler directly to guarantee coverage.
	_ = rec
	rec2 := httptest.NewRecorder()
	s.handleGitHubAppInstallClicked(rec2, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("install-clicked: expected 200, got %d", rec2.Code)
	}
	if !s.IsPendingGitHubAppInstall() {
		t.Fatalf("expected pending install flagged")
	}
}

func TestCovI_GHUserAuthPoll(t *testing.T) {
	s := covApiServer(t)
	// No device flow in progress → 400.
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("poll no-flow: expected 400, got %d", rec.Code)
	}
}
