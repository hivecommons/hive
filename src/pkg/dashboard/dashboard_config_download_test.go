package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestHandleConfigDownloadSuccess(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  org: testorg\n"), 0644)

	t.Setenv("HIVE_CONFIG", cfgPath)

	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo", Repos: []string{"testrepo"}},
		Agents:  map[string]config.AgentConfig{},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}

	req := httptest.NewRequest("GET", "/api/config/download", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleConfigDownload(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "yaml") {
		t.Error("should return YAML content type")
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "hive-testorg-testrepo") {
		t.Errorf("filename should contain org-repo, got %q", w.Header().Get("Content-Disposition"))
	}
}

func TestHandleConfigDownloadNonOwner(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/config/download", nil)
	req.Header.Set("X-Hive-Role", "contributor")
	w := httptest.NewRecorder()
	srv.handleConfigDownload(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleConfigDownloadMissingFile(t *testing.T) {
	t.Setenv("HIVE_CONFIG", "/tmp/nonexistent-hive-config-xyz.yaml")

	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/config/download", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleConfigDownload(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleSnapshotAPIEnabled(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
		Hub:     config.HubConfig{AutoSnapshot: true},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	// AutoSnapshot=true but no status yet → 503
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (no data yet), got %d", w.Code)
	}
}

func TestHandleSnapshotAPIWithStatus(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
		Hub:     config.HubConfig{AutoSnapshot: true},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}
	srv.status = &StatusPayload{Timestamp: "2024-01-01T00:00:00Z", HiveID: "test"}

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "test") {
		t.Error("should contain hive ID")
	}
}

func TestHandleSnapshotPageWithHubURL(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
		Hub:     config.HubConfig{URL: "https://custom-hub.example.com", AutoSnapshot: false},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}

	req := httptest.NewRequest("GET", "/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "custom-hub.example.com") {
		t.Error("should use custom hub URL in redirect")
	}
}

func TestHandleHistoryNoSeedFile(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
	}
	srv := NewServer(0, slog.Default())
	gov := governor.New(cfg.Governor, cfg.Agents, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default(), Governor: gov, Ctx: context.Background()}

	req := httptest.NewRequest("GET", "/api/history", nil)
	w := httptest.NewRecorder()
	srv.handleHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleTrendsWeekRange(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
	}
	srv := NewServer(0, slog.Default())
	gov := governor.New(cfg.Governor, cfg.Agents, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default(), Governor: gov, Ctx: context.Background()}

	req := httptest.NewRequest("GET", "/api/trends?range=week", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleTrendsCustomHours(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
	}
	srv := NewServer(0, slog.Default())
	gov := governor.New(cfg.Governor, cfg.Agents, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default(), Governor: gov, Ctx: context.Background()}

	req := httptest.NewRequest("GET", "/api/trends?hours=48", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleTrendsMaxHoursClamped(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:  map[string]config.AgentConfig{},
	}
	srv := NewServer(0, slog.Default())
	gov := governor.New(cfg.Governor, cfg.Agents, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default(), Governor: gov, Ctx: context.Background()}

	req := httptest.NewRequest("GET", "/api/trends?hours=9999", nil)
	w := httptest.NewRecorder()
	srv.handleTrends(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleSelfUpgradeWithHubURL(t *testing.T) {
	// Mock hub that returns upgrade response
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"upgrading"}`))
	}))
	defer hub.Close()

	cfg := &config.Config{
		Project:   config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:    map[string]config.AgentConfig{},
		Hub:       config.HubConfig{URL: hub.URL},
		HiveID:    "test-hive-123",
		Dashboard: config.DashboardConfig{AuthToken: "spoke-token"},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}

	req := httptest.NewRequest("POST", "/api/self-upgrade", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleSelfUpgrade(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upgrading") {
		t.Error("should return upgrade status")
	}
}

func TestHandleSelfUpgradeForwardsSpokeSessionProof(t *testing.T) {
	const (
		user       = "alice"
		role       = "owner"
		proofToken = "spoke-dashboard-token"
	)

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Hive-User"); got != user {
			t.Errorf("X-Hive-User = %q, want %q", got, user)
		}
		if got := r.Header.Get("X-Hive-Role"); got != role {
			t.Errorf("X-Hive-Role = %q, want %q", got, role)
		}
		if got := r.Header.Get(proxyAuthHeader); got != proofToken {
			t.Errorf("%s = %q, want %q", proxyAuthHeader, got, proofToken)
		}
		if _, err := r.Cookie("hive_hub_user"); err == nil {
			t.Fatal("test request unexpectedly depended on hub cookie auth")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"upgrading"}`))
	}))
	defer hub.Close()

	cfg := &config.Config{
		Project:   config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:    map[string]config.AgentConfig{},
		Hub:       config.HubConfig{URL: hub.URL},
		HiveID:    "test-hive-123",
		Dashboard: config.DashboardConfig{AuthToken: proofToken},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}

	req := httptest.NewRequest("POST", "/api/self-upgrade", nil)
	req.Header.Set("X-Hive-User", user)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

func TestHandleSelfUpgradeHubUnreachable(t *testing.T) {
	cfg := &config.Config{
		Project:   config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		Agents:    map[string]config.AgentConfig{},
		Hub:       config.HubConfig{URL: "http://127.0.0.1:1"},
		HiveID:    "test-hive-123",
		Dashboard: config.DashboardConfig{AuthToken: "spoke-token"},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}

	req := httptest.NewRequest("POST", "/api/self-upgrade", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleSelfUpgrade(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestHandleAPIv1NoAuth(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/v1", nil)
	w := httptest.NewRecorder()
	srv.handleAPIv1(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid or missing GitHub token") {
		t.Error("should return auth error message")
	}
}

func TestHandleAPIv1WithBearerPrefix(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer ghp_invalidtoken123")
	w := httptest.NewRecorder()
	srv.handleAPIv1(w, req)

	// Token is invalid → 401
	if w.Code != http.StatusUnauthorized {
		t.Logf("APIv1 with invalid bearer: %d", w.Code)
	}
}

func TestHandleAPIv1WithTokenPrefix(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Authorization", "token ghp_invalidtoken123")
	w := httptest.NewRecorder()
	srv.handleAPIv1(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Logf("APIv1 with invalid token prefix: %d", w.Code)
	}
}

func TestHandleAPIv1WithQueryToken(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/v1/status?token=ghp_invalidtoken123", nil)
	w := httptest.NewRecorder()
	srv.handleAPIv1(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Logf("APIv1 with invalid query token: %d", w.Code)
	}
}

func TestHandleGitSourcesConnectMissingName(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"url":"https://github.com/org/repo"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectMissingURL(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"my-source"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing url, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectBadURLScheme(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"my-source","url":"ftp://example.com"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad URL scheme, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectPathTraversal(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"../evil","url":"https://github.com/org/repo"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectBadBranch(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"my-source","url":"https://github.com/org/repo","branch":"-evil"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad branch, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectBadSubpath(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"my-source","url":"https://github.com/org/repo","subpath":"../etc"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad subpath, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectSubpathDash(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"my-source","url":"https://github.com/org/repo","subpath":"-rf"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for subpath starting with dash, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectNameWithSlash(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"bad/name","url":"https://github.com/org/repo"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for name with slash, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectGitAtURL(t *testing.T) {
	srv := newMinimalServer(t)
	body := `{"name":"my-source","url":"git@github.com:org/repo.git"}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for git@ URL, got %d", w.Code)
	}
}

func TestHandleGitSourcesDisconnectMissingURL(t *testing.T) {
	srv := newMinimalServer(t)
	// Give it a knowledge instance
	srv.ensureKnowledge()

	body := `{}`
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources/disconnect", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesDisconnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing URL, got %d", w.Code)
	}
}

func TestHandleGitSourcesDisconnectBadJSON(t *testing.T) {
	srv := newMinimalServer(t)
	srv.ensureKnowledge()

	req := httptest.NewRequest("POST", "/api/knowledge/git-sources/disconnect", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesDisconnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d", w.Code)
	}
}

func TestHandleGitSourcesConnectBadJSON(t *testing.T) {
	srv := newMinimalServer(t)
	req := httptest.NewRequest("POST", "/api/knowledge/git-sources", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d", w.Code)
	}
}

// TestHandleConfigDownloadMissingRoleDeniedWithAuthToken pins F9 (CWE-862): on a
// spoke that HAS an auth boundary (a shared authToken), a request with NO
// X-Hive-Role must be denied — a missing role must not be silently promoted to
// owner and hand out the raw hive.yaml. This is the compounding half of the
// ownerless-config finding.
func TestHandleConfigDownloadMissingRoleDeniedWithAuthToken(t *testing.T) {
	srv := newMinimalServer(t)
	srv.authToken = "shared-secret-token" // gives the spoke an auth boundary
	req := httptest.NewRequest("GET", "/api/config/download", nil)
	// No X-Hive-Role header at all.
	w := httptest.NewRecorder()
	srv.handleConfigDownload(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("missing role on a token-guarded spoke must be denied; got %d, want 403", w.Code)
	}
}

// TestHandleConfigDownloadOwnerRoleAllowed confirms the legitimate owner path is
// untouched by the F9 fix: an explicit owner role is served even on a
// token-guarded spoke.
func TestHandleConfigDownloadOwnerRoleAllowed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  org: testorg\n"), 0644)
	t.Setenv("HIVE_CONFIG", cfgPath)

	srv := newMinimalServer(t)
	srv.authToken = "shared-secret-token"
	req := httptest.NewRequest("GET", "/api/config/download", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleConfigDownload(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("explicit owner must still be served; got %d, want 200", w.Code)
	}
}

// TestHandleConfigDownloadMissingRoleOpenSpoke confirms the open/dev spoke (no
// authToken, no allowlist) still treats a routed missing-role request as owner:
// authenticate stamps the verified owner marker before the owner-only handler.
func TestHandleConfigDownloadMissingRoleOpenSpoke(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  org: testorg\n"), 0644)
	t.Setenv("HIVE_CONFIG", cfgPath)

	srv := newMinimalServer(t) // NewServer => authToken=="" and no allowlist
	req := httptest.NewRequest("GET", "/api/config/download", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("open/dev spoke should still serve missing-role as owner; got %d, want 200", w.Code)
	}
}

func TestHandleConfigDownloadEnvVar(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "custom.yaml")
	os.WriteFile(cfgPath, []byte("custom: true\n"), 0644)

	t.Setenv("HIVE_CONFIG", cfgPath)

	srv := newMinimalServer(t)
	req := httptest.NewRequest("GET", "/api/config/download", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleConfigDownload(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "custom: true") {
		t.Error("should return custom config file contents")
	}
}
