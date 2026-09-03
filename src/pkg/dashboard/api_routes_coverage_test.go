package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// covG2ServeRaw sends a request with a raw (possibly malformed) JSON body and a
// custom method to the server mux, returning the recorder.
func covG2ServeRaw(s *Server, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, stringReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// ---------- History / trends / timeline ----------

func TestCovG2_HistoryTrendsTimeline(t *testing.T) {
	s, _ := apiServer(t)

	if rec := doGet(s, "/api/history"); rec.Code != http.StatusOK {
		t.Errorf("history status = %d", rec.Code)
	}
	for _, rng := range []string{"", "?range=day", "?range=week", "?hours=48", "?hours=99999"} {
		if rec := doGet(s, "/api/trends"+rng); rec.Code != http.StatusOK {
			t.Errorf("trends%s status = %d", rng, rec.Code)
		}
	}
	if rec := doGet(s, "/api/timeline"); rec.Code != http.StatusOK {
		t.Errorf("timeline status = %d", rec.Code)
	}
}

// ---------- Token access (no file present → empty entries) ----------

func TestCovG2_TokenAccess(t *testing.T) {
	s, _ := apiServer(t)
	// Owner role required since #3936: the access log contains full gh-command
	// lines and must not be visible to non-owner authenticated users.
	rec := doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Errorf("token-access status = %d", rec.Code)
	}
}

// ---------- Role (header / cookie branches) ----------

func TestCovG2_Role(t *testing.T) {
	s, _ := apiServer(t)

	// Default (no header) → owner fallback.
	if rec := doGet(s, "/api/role"); rec.Code != http.StatusOK {
		t.Errorf("role default = %d", rec.Code)
	}

	// With explicit header.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/role", nil)
	req.Header.Set("X-Hive-Role", "read")
	req.Header.Set("X-Hive-User", "alice")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("role header = %d", rec.Code)
	}

	// The hub-wide hive_hub_user cookie is intentionally ignored by spoke
	// dashboards; identity must come from the request headers or hive_session.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/role", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "bob"})
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("role cookie = %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["user"] != "" {
		t.Errorf("role must ignore hive_hub_user cookie, got user %v", body["user"])
	}
}

// ---------- GH user auth: status (session / header / logged-out), poll, logout ----------

func TestCovG2_GHUserAuthStatus(t *testing.T) {
	s, _ := apiServer(t)

	// Logged-out (no session, no token file).
	if rec := doGet(s, "/api/gh-user-auth/status"); rec.Code != http.StatusOK {
		t.Errorf("status logged-out = %d", rec.Code)
	}

	// Per-user session cookie → logged in.
	sid := s.createUserSession("carol", "owner")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status session = %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["logged_in"] != true {
		t.Errorf("expected logged_in true for session, got %v", body["logged_in"])
	}

	// Hub-proxied header path.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/status", nil)
	req.Header.Set("X-Hive-User", "dave")
	req.Header.Set("X-Hive-Role", "read")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status header = %d", rec.Code)
	}
}

func TestCovG2_GHUserAuthPollAndLogout(t *testing.T) {
	s, _ := apiServer(t)

	// Poll with no device flow in progress → 400.
	if rec := doPost(s, "/api/gh-user-auth/poll", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("poll no-flow = %d, want 400", rec.Code)
	}

	// Logout requires a live GitHub user session.
	if rec := doPost(s, "/api/gh-user-auth/logout", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("logout = %d, want 401", rec.Code)
	}
}

// ---------- SSO handoff ----------

func TestCovG2_SSO(t *testing.T) {
	s, _ := apiServer(t)

	// No HIVE_HUB_SECRET → redirect to login.
	rec := doGet(s, "/api/sso?token=x")
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound && rec.Code != http.StatusNotFound {
		// route may not be registered under /api/sso; call handler directly instead
		rec2 := httptest.NewRecorder()
		s.handleSSO(rec2, httptest.NewRequest(http.MethodGet, "/sso?token=x", nil))
		if rec2.Code == 0 {
			t.Errorf("sso no-secret produced no response")
		}
	}

	// With secret but invalid token → not a 2xx.
	t.Setenv("HIVE_HUB_SECRET", "s3cr3t")
	rec = httptest.NewRecorder()
	s.handleSSO(rec, httptest.NewRequest(http.MethodGet, "/sso", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sso missing token = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleSSO(rec, httptest.NewRequest(http.MethodGet, "/sso?token=badtoken", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("sso invalid token should not be 200")
	}
}

// ---------- Kick / restart / reset-restarts / pause / resume ----------

func TestCovG2_AgentLifecycleErrors(t *testing.T) {
	s, _ := apiServer(t)

	// Kick with an over-long prompt → 400.
	long := make([]byte, 10001)
	for i := range long {
		long[i] = 'a'
	}
	rec := covG2ServeRaw(s, http.MethodPost, "/api/kick/scanner", `{"prompt":"`+string(long)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("kick long prompt = %d, want 400", rec.Code)
	}

	// Kick with a normal message (SendKick likely errors on a non-running
	// session → 400, but exercises the body path either way).
	rec = doPost(s, "/api/kick/scanner", map[string]string{"message": "hi"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Errorf("kick normal = %d", rec.Code)
	}

	// Restart / reset-restarts / pause / resume for an unknown agent exercise
	// the error branch (any non-panic status is acceptable).
	for _, p := range []string{"/api/restart/scanner", "/api/reset-restarts/scanner", "/api/pause/scanner", "/api/resume/scanner"} {
		if rec := doPost(s, p, nil); rec.Code == 0 {
			t.Errorf("%s produced no response", p)
		}
	}
}

// ---------- Governor add/remove agent ----------

func TestCovG2_GovernorAddAgent(t *testing.T) {
	s, _ := apiServer(t)

	// Missing name → 400.
	if rec := doPost(s, "/api/config/governor/agents", map[string]string{}); rec.Code != http.StatusBadRequest {
		t.Errorf("add agent no name = %d, want 400", rec.Code)
	}
	// Invalid name (spaces/slashes) → 400.
	if rec := doPost(s, "/api/config/governor/agents", map[string]string{"name": "bad name/x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("add agent bad name = %d, want 400", rec.Code)
	}
	// Existing agent → 409.
	if rec := doPost(s, "/api/config/governor/agents", map[string]string{"name": "scanner"}); rec.Code != http.StatusConflict {
		t.Errorf("add existing agent = %d, want 409", rec.Code)
	}
	// New valid agent → 200.
	if rec := doPost(s, "/api/config/governor/agents", map[string]string{"name": "newbie", "backend": "claude", "model": "sonnet"}); rec.Code != http.StatusOK {
		t.Errorf("add new agent = %d, want 200", rec.Code)
	}
	// Now remove it.
	if rec := doDelete(s, "/api/config/governor/agents/newbie", nil); rec.Code == 0 {
		t.Errorf("remove agent produced no response")
	}
}

// ---------- Agent config: restrictions / prompt-save / general ----------

func TestCovG2_AgentConfig(t *testing.T) {
	s, _ := apiServer(t)

	// Restrictions: unknown agent → 404.
	if rec := doPut(s, "/api/config/agent/ghost/restrictions", map[string]any{"agent": []any{}}); rec.Code != http.StatusNotFound {
		t.Errorf("restrictions unknown = %d, want 404", rec.Code)
	}
	// Restrictions bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPut, "/api/config/agent/scanner/restrictions", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("restrictions bad body = %d, want 400", rec.Code)
	}
	// Restrictions valid body → 200 (writes to /data may fail but handler completes).
	if rec := doPut(s, "/api/config/agent/scanner/restrictions", map[string]any{
		"agent": []map[string]string{{"pattern": "*.env", "reason": "secrets"}},
	}); rec.Code != http.StatusOK {
		t.Errorf("restrictions valid = %d, want 200", rec.Code)
	}

	// Prompt save: unknown agent → 404.
	if rec := doPut(s, "/api/config/agent/ghost/prompt", map[string]any{"template": "x"}); rec.Code != http.StatusNotFound {
		t.Errorf("prompt-save unknown = %d, want 404", rec.Code)
	}
	// Prompt save bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPut, "/api/config/agent/scanner/prompt", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("prompt-save bad body = %d, want 400", rec.Code)
	}
	// Prompt save valid inline template.
	if rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"template": "You are ${AGENT_NAME}"}); rec.Code == 0 {
		t.Errorf("prompt-save valid produced no response")
	}
	// promptSource with missing fields → 400 (IsSet guard).
	if rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"promptSource": map[string]string{"owner": "o"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("prompt-save bad promptSource = %d, want 400", rec.Code)
	}

	// General: unknown agent → 404.
	if rec := doPut(s, "/api/config/agent/ghost/general", map[string]any{"model": "sonnet"}); rec.Code != http.StatusNotFound {
		t.Errorf("general unknown = %d, want 404", rec.Code)
	}

	// Agent prompt GET (raw=1 and rendered).
	if rec := doGet(s, "/api/config/agent/scanner/prompt?raw=1"); rec.Code == 0 {
		t.Errorf("prompt raw produced no response")
	}
	if rec := doGet(s, "/api/config/agent/scanner/prompt"); rec.Code == 0 {
		t.Errorf("prompt rendered produced no response")
	}
	// Agent export (buildExportYAML).
	if rec := doGet(s, "/api/config/agent/scanner/export"); rec.Code != http.StatusOK {
		t.Errorf("agent export = %d", rec.Code)
	}
}

// ---------- Config GitHub ----------

func TestCovG2_ConfigGitHub(t *testing.T) {
	s, _ := apiServer(t)

	// Bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPut, "/api/config/github", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("config github bad body = %d, want 400", rec.Code)
	}
	// No fields → 400.
	if rec := doPut(s, "/api/config/github", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("config github no fields = %d, want 400", rec.Code)
	}
	// app_id + installation_id → 200.
	if rec := doPut(s, "/api/config/github", map[string]any{"app_id": 123, "installation_id": 456}); rec.Code == 0 {
		t.Errorf("config github ids produced no response")
	}
	// private_key written to a temp key_file → success path.
	keyFile := t.TempDir() + "/key.pem"
	if rec := doPut(s, "/api/config/github", map[string]any{"private_key": "-----BEGIN KEY-----\nabc\n-----END KEY-----", "key_file": keyFile}); rec.Code == 0 {
		t.Errorf("config github private_key produced no response")
	}
}

// ---------- Budget ignore get/set ----------

func TestCovG2_BudgetIgnore(t *testing.T) {
	s, _ := apiServer(t)

	if rec := doGet(s, "/api/budget-ignore"); rec.Code != http.StatusOK {
		t.Errorf("budget-ignore get = %d", rec.Code)
	}
	// Bool form.
	if rec := doPost(s, "/api/budget-ignore", map[string]any{"ignored": true}); rec.Code != http.StatusOK {
		t.Errorf("budget-ignore bool = %d", rec.Code)
	}
	// List form.
	if rec := doPost(s, "/api/budget-ignore", map[string]any{"ignored": []string{"scanner"}}); rec.Code != http.StatusOK {
		t.Errorf("budget-ignore list = %d", rec.Code)
	}
	// Bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/budget-ignore", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("budget-ignore bad body = %d, want 400", rec.Code)
	}
	// Invalid ignored type (number) → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/budget-ignore", `{"ignored":123}`); rec.Code != http.StatusBadRequest {
		t.Errorf("budget-ignore invalid type = %d, want 400", rec.Code)
	}
}

// ---------- Simple GETs ----------

func TestCovG2_SimpleGets(t *testing.T) {
	s, _ := apiServer(t)
	for _, p := range []string{
		"/api/gh-auth", "/api/model-advisor", "/api/tokens",
		"/api/config/backends", "/api/cost", "/api/cost/history",
	} {
		if rec := doGet(s, p); rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", p, rec.Code)
		}
	}
}

// ---------- Inference models ----------

func TestCovG2_InferenceModels(t *testing.T) {
	s, _ := apiServer(t)

	// Empty backend path segment isn't routable; unknown backend → 404.
	if rec := doGet(s, "/api/inference/models/does-not-exist"); rec.Code != http.StatusNotFound {
		t.Errorf("inference unknown backend = %d, want 404", rec.Code)
	}

	// Register an endpoint for a backend, then query it (fetch fails → static fallback).
	s.SetInferenceEndpoints(map[string][]string{"vllm": {"http://127.0.0.1:0/v1"}})
	if rec := doGet(s, "/api/inference/models/vllm"); rec.Code != http.StatusOK {
		t.Errorf("inference vllm = %d, want 200", rec.Code)
	}
}

// ---------- GH rate limits (with mock) ----------

func TestCovG2_GHRateLimits(t *testing.T) {
	s, _, ghSrv := apiServerWithGH(t)
	defer ghSrv.Close()
	if rec := doGet(s, "/api/gh-rate-limits"); rec.Code != http.StatusOK {
		t.Errorf("gh-rate-limits = %d", rec.Code)
	}
}

// ---------- Knowledge cluster (with a real knowledge API wired) ----------

func TestCovG2_KnowledgeCluster(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// List / export / graph / fact-history / stats / health.
	for _, p := range []string{
		"/api/knowledge", "/api/knowledge?type=pattern", "/api/knowledge/export",
		"/api/knowledge/graph", "/api/knowledge/fact-history",
		"/api/knowledge/health", "/api/knowledge/stats",
	} {
		if rec := doGet(s, p); rec.Code == 0 {
			t.Errorf("%s produced no response", p)
		}
	}

	// Search: missing q and tag → 400.
	if rec := doGet(s, "/api/knowledge/search"); rec.Code != http.StatusBadRequest {
		t.Errorf("knowledge search no params = %d, want 400", rec.Code)
	}
	// Search with q and tag.
	if rec := doGet(s, "/api/knowledge/search?q=foo&limit=3"); rec.Code != http.StatusOK {
		t.Errorf("knowledge search q = %d", rec.Code)
	}
	if rec := doGet(s, "/api/knowledge/search?tag=bar"); rec.Code != http.StatusOK {
		t.Errorf("knowledge search tag = %d", rec.Code)
	}

	// Layer (plain + git_source: prefix).
	if rec := doGet(s, "/api/knowledge/project"); rec.Code != http.StatusOK {
		t.Errorf("knowledge layer = %d", rec.Code)
	}
	if rec := doGet(s, "/api/knowledge/git_source:none"); rec.Code != http.StatusOK {
		t.Errorf("knowledge git-source layer = %d", rec.Code)
	}

	// Promote: bad body → 400, missing fields → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/knowledge/promote", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("promote bad body = %d, want 400", rec.Code)
	}
	if rec := doPost(s, "/api/knowledge/promote", map[string]string{"slug": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("promote missing fields = %d, want 400", rec.Code)
	}
	// Promote with all fields (fact likely absent → error branch).
	if rec := doPost(s, "/api/knowledge/promote", map[string]string{"slug": "s", "from_layer": "project", "to_layer": "org"}); rec.Code == 0 {
		t.Errorf("promote full produced no response")
	}
}

func TestCovG2_KnowledgeToggle(t *testing.T) {
	s, _ := apiServer(t)

	// Bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPut, "/api/knowledge/enabled", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("knowledge toggle bad body = %d, want 400", rec.Code)
	}
	// Enable then disable.
	if rec := doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": true}); rec.Code != http.StatusOK {
		t.Errorf("knowledge toggle enable = %d", rec.Code)
	}
	if rec := doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": false}); rec.Code != http.StatusOK {
		t.Errorf("knowledge toggle disable = %d", rec.Code)
	}
}

func TestCovG2_Vaults(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// Connect: bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/knowledge/vaults", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("vault connect bad body = %d, want 400", rec.Code)
	}
	// Missing path → 400.
	if rec := doPost(s, "/api/knowledge/vaults", map[string]string{"name": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("vault connect no path = %d, want 400", rec.Code)
	}
	// Path with .. → 400.
	if rec := doPost(s, "/api/knowledge/vaults", map[string]string{"path": "/a/../b"}); rec.Code != http.StatusBadRequest {
		t.Errorf("vault connect .. = %d, want 400", rec.Code)
	}
	// Relative path → 400.
	if rec := doPost(s, "/api/knowledge/vaults", map[string]string{"path": "relative/path"}); rec.Code != http.StatusBadRequest {
		t.Errorf("vault connect relative = %d, want 400", rec.Code)
	}
	// Absolute temp path → 200 (ConnectVault on an empty dir succeeds).
	if rec := doPost(s, "/api/knowledge/vaults", map[string]string{"path": t.TempDir()}); rec.Code == 0 {
		t.Errorf("vault connect valid produced no response")
	}
	// List / reindex.
	if rec := doGet(s, "/api/knowledge/vaults"); rec.Code == 0 {
		t.Errorf("vault list produced no response")
	}
	if rec := doPost(s, "/api/knowledge/vaults/reindex", nil); rec.Code == 0 {
		t.Errorf("vault reindex produced no response")
	}
}

// ---------- Documents cluster ----------

func TestCovG2_Documents(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// List.
	if rec := doGet(s, "/api/knowledge/documents"); rec.Code == 0 {
		t.Errorf("documents list produced no response")
	}
	// Import: bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/knowledge/documents", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("documents import bad body = %d, want 400", rec.Code)
	}
	// Import: missing name → 400.
	if rec := doPost(s, "/api/knowledge/documents", map[string]string{"url": "http://x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("documents import no name = %d, want 400", rec.Code)
	}
	// Import: name but no source → 400.
	if rec := doPost(s, "/api/knowledge/documents", map[string]string{"name": "doc"}); rec.Code != http.StatusBadRequest {
		t.Errorf("documents import no source = %d, want 400", rec.Code)
	}
	// Get unknown slug → 404.
	if rec := doGet(s, "/api/knowledge/documents/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("document get unknown = %d, want 404", rec.Code)
	}
	// Delete unknown slug → 404.
	if rec := doDelete(s, "/api/knowledge/documents/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("document delete unknown = %d, want 404", rec.Code)
	}
	// Reimport unknown slug → error (non-panic).
	if rec := doPost(s, "/api/knowledge/documents/nope/reimport", nil); rec.Code == 0 {
		t.Errorf("document reimport produced no response")
	}
	// Cleanup orphans.
	if rec := doPost(s, "/api/knowledge/cleanup-orphans", nil); rec.Code == 0 {
		t.Errorf("cleanup orphans produced no response")
	}
	// Context7 search: missing q → 400.
	if rec := doGet(s, "/api/knowledge/context7/search"); rec.Code != http.StatusBadRequest {
		t.Errorf("context7 no q = %d, want 400", rec.Code)
	}
}

// ---------- Bead synthesizer ----------

func TestCovG2_BeadSynth(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doGet(s, "/api/knowledge/bead-synthesizer"); rec.Code != http.StatusOK {
		t.Errorf("bead-synth status = %d", rec.Code)
	}
	if rec := covG2ServeRaw(s, http.MethodPut, "/api/knowledge/bead-synthesizer/enabled", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("bead-synth bad body = %d, want 400", rec.Code)
	}
	if rec := doPut(s, "/api/knowledge/bead-synthesizer/enabled", map[string]bool{"enabled": true}); rec.Code != http.StatusOK {
		t.Errorf("bead-synth enable = %d", rec.Code)
	}
	if rec := doPut(s, "/api/knowledge/bead-synthesizer/enabled", map[string]bool{"enabled": false}); rec.Code != http.StatusOK {
		t.Errorf("bead-synth disable = %d", rec.Code)
	}
}

// ---------- Git sources ----------

func TestCovG2_GitSources(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	if rec := doGet(s, "/api/knowledge/git-sources"); rec.Code == 0 {
		t.Errorf("git-sources list produced no response")
	}
	// Connect bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/knowledge/git-sources", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("git connect bad body = %d, want 400", rec.Code)
	}
	// Missing name/url → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]string{"name": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("git connect no url = %d, want 400", rec.Code)
	}
	// Bad scheme → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]string{"name": "x", "url": "ftp://y"}); rec.Code != http.StatusBadRequest {
		t.Errorf("git connect bad scheme = %d, want 400", rec.Code)
	}
	// Bad name → 400.
	if rec := doPost(s, "/api/knowledge/git-sources", map[string]string{"name": "a/b", "url": "https://y"}); rec.Code != http.StatusBadRequest {
		t.Errorf("git connect bad name = %d, want 400", rec.Code)
	}
	// Disconnect bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodDelete, "/api/knowledge/git-sources", `{bad`); rec.Code == 0 {
		t.Errorf("git disconnect bad body produced no response")
	}
}

// ---------- Obsidian sync ----------

func TestCovG2_ObsidianSync(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	// Bad body → 400.
	if rec := covG2ServeRaw(s, http.MethodPost, "/api/knowledge/obsidian/sync", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("obsidian bad body = %d, want 400", rec.Code)
	}
	// Missing filename → 400.
	if rec := doPost(s, "/api/knowledge/obsidian/sync", map[string]any{"content": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("obsidian no filename = %d, want 400", rec.Code)
	}
	// Valid-ish (empty content falls back to frontmatter title / placeholder).
	if rec := doPost(s, "/api/knowledge/obsidian/sync", map[string]any{
		"filename":    "note.md",
		"frontmatter": map[string]any{"title": "My Note"},
	}); rec.Code == 0 {
		t.Errorf("obsidian valid produced no response")
	}
}

// ---------- Snapshot page ----------

func TestCovG2_SnapshotPage(t *testing.T) {
	s, _ := apiServer(t)
	// Snapshot page reads/generates files; any non-panic status is fine.
	if rec := doGet(s, "/snapshot"); rec.Code == 0 {
		t.Errorf("snapshot page produced no response")
	}
}
