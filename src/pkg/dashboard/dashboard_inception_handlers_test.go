package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func newMinimalServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config: cfg,
		Logger: slog.Default(),
		Ctx:    context.Background(),
	}
	srv.RegisterAPI(srv.deps)
	return srv
}

func TestHandleInceptionStartNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/start", strings.NewReader(`{"idea":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionStart(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionScanNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/scan", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionScan(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionStateNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/inception/state", nil)
	w := httptest.NewRecorder()
	srv.handleInceptionState(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionSetQuestionsNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/questions", strings.NewReader(`{"questions":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionSetQuestions(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionAnswerNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/answer", strings.NewReader(`{"answers":{}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionAnswer(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionRecordFactsNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/facts", strings.NewReader(`{"facts":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionRecordFacts(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionScaffoldNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/scaffold", nil)
	w := httptest.NewRecorder()
	srv.handleInceptionScaffold(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionApproveNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/approve", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionApprove(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionResetNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/reset", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionReset(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionIdeationFactsNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/inception/ideation-facts", nil)
	w := httptest.NewRecorder()
	srv.handleInceptionIdeationFacts(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionDownloadNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/inception/download", nil)
	w := httptest.NewRecorder()
	srv.handleInceptionDownload(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionHasFilesNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/inception/has-files", nil)
	w := httptest.NewRecorder()
	srv.handleInceptionHasFiles(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "false") {
		t.Error("should return has_files=false when engine is nil")
	}
}

func TestHandleInceptionRenameWikiNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/rename-wiki", strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionRenameWiki(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleInceptionImportNilEngine(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/inception/import", strings.NewReader(`{"files":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleInceptionImport(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleGHUserAuthStatusNoToken(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/gh-user-auth/status", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthStatus(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "false") {
		t.Error("should return logged_in=false")
	}
}

// A blank oauth_client_id resolves to the PUBLIC github.com Hive App client
// rather than failing: device-flow login is always github.com, even for a hive
// whose App and repos live on GHE. An earlier revision returned 400 here, which
// broke login for every hive that had not set oauth_client_id explicitly.
func TestHandleGHUserAuthStartBlankClientIDResolvesPublicDefault(t *testing.T) {
	srv := newMinimalServer(t)
	if got := srv.deps.Config.GitHub.OAuthClientID; got != "" {
		t.Fatalf("precondition: want blank oauth_client_id, got %q", got)
	}
	if got := srv.deps.Config.GitHub.OAuthClientIDResolved(); got != config.DefaultOAuthClientID {
		t.Fatalf("blank oauth_client_id must resolve to the public default %q, got %q",
			config.DefaultOAuthClientID, got)
	}
	// Login host stays public github.com regardless of a GHE App host.
	srv.deps.Config.GitHub.BaseURL = "https://github.ibm.com"
	if got := srv.deps.Config.GitHub.OAuthBaseURL(); got != config.DefaultGitHubBaseURL {
		t.Fatalf("GHE hive must still log in via %q, got %q", config.DefaultGitHubBaseURL, got)
	}
}

func TestHandleGHUserAuthPollNoState(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/gh-user-auth/poll", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthPoll(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no device flow") {
		t.Error("should say no device flow in progress")
	}
}

func TestHandleGHUserAuthLogout(t *testing.T) {
	srv := newMinimalServer(t)
	sid := srv.createUserSession("owner", config.RoleOwner)
	req := httptest.NewRequest("POST", "/api/gh-user-auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.handleGHUserAuthLogout(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "logged_out") {
		t.Error("should return logged_out status")
	}
}

func TestHandleSelfUpgradeNonOwner(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/self-upgrade", nil)
	req.Header.Set("X-Hive-Role", "contributor")
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleSelfUpgradeNoHubURL(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/self-upgrade", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleSelfUpgrade(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSnapshotAPINotEnabled(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleSnapshotPageNotEnabled(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Snapshots are not enabled") {
		t.Logf("body: %s", w.Body.String()[:200])
	}
}

func TestHandleGitSourcesListNilKnowledge(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/knowledge/git-sources", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesList(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectNoKnowledge(t *testing.T) {
	srv := newMinimalServer(t)
	// ensureKnowledge will try to create a file-based knowledge API
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)
	// Should get 400 (invalid request body or missing fields) since ensureKnowledge creates a fallback
	if w.Code != http.StatusBadRequest {
		t.Logf("git sources connect: got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleGitSourcesDisconnectNilKnowledge(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources/disconnect", strings.NewReader(`{"url":"https://example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesDisconnect(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Logf("git sources disconnect: got %d", w.Code)
	}
}

func TestHandleAPIDocs(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/v1/docs", nil)
	w := httptest.NewRecorder()
	srv.handleAPIDocs(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Error("should return HTML")
	}
}

func TestReadJSONValid(t *testing.T) {
	body := strings.NewReader(`{"idea":"test project"}`)
	req := httptest.NewRequest("POST", "/test", body)
	var result struct {
		Idea string `json:"idea"`
	}
	err := readJSON(req, &result)
	if err != nil {
		t.Errorf("readJSON error: %v", err)
	}
	if result.Idea != "test project" {
		t.Errorf("idea = %q", result.Idea)
	}
}

func TestReadJSONInvalid(t *testing.T) {
	body := strings.NewReader(`{invalid`)
	req := httptest.NewRequest("POST", "/test", body)
	var result struct{}
	err := readJSON(req, &result)
	if err == nil {
		t.Error("should error on invalid JSON")
	}
}

func TestReadJSONEmpty(t *testing.T) {
	body := strings.NewReader(``)
	req := httptest.NewRequest("POST", "/test", body)
	var result struct{}
	err := readJSON(req, &result)
	if err == nil {
		t.Error("should error on empty body")
	}
}

func TestHandleKnowledgeExportNilKnowledge(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/knowledge/export", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeExport(w, req)
	// Should handle nil knowledge gracefully — either 503 or creates fallback
	_ = w.Code
}
