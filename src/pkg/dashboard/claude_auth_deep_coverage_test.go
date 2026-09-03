package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// stubClaudeSeams overrides the package-level seams in claude_auth.go with
// test doubles and restores them on t.Cleanup. Returns the temp directory
// used for credential/token file paths.
func stubClaudeSeams(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()

	origCredPath := claudeCredPath
	origTokenPath := claudeTokenFilePath
	origReadToken := claudeReadToken
	origWriteCreds := claudeWriteCreds
	origExchange := claudeExchange
	origRemove := claudeRemoveFile

	claudeCredPath = filepath.Join(tmp, "credentials.json")
	claudeTokenFilePath = filepath.Join(tmp, "claude-user-token")
	claudeReadToken = func(path string) string { return "" }
	claudeWriteCreds = func(tok *claude.OAuthTokens, path string) error { return nil }
	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		return nil, fmt.Errorf("exchange not stubbed")
	}
	claudeRemoveFile = func(path string) error { return nil }

	t.Cleanup(func() {
		claudeCredPath = origCredPath
		claudeTokenFilePath = origTokenPath
		claudeReadToken = origReadToken
		claudeWriteCreds = origWriteCreds
		claudeExchange = origExchange
		claudeRemoveFile = origRemove
	})
	return tmp
}

func claudeDeepServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.deps = &Dependencies{Logger: logger}
	return s
}

func decodeJSONMap(t *testing.T, body io.Reader) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// handleClaudeAuthStatus
// ---------------------------------------------------------------------------

func TestClaudeDeep_Status_NotLoggedIn(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/claude-auth/status", nil)
	s.handleClaudeAuthStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	m := decodeJSONMap(t, rec.Body)
	if m["logged_in"] != false {
		t.Fatalf("logged_in = %v, want false", m["logged_in"])
	}
}

func TestClaudeDeep_Status_LoggedIn(t *testing.T) {
	stubClaudeSeams(t)
	claudeReadToken = func(path string) string { return "sk-valid-token" }
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/claude-auth/status", nil)
	s.handleClaudeAuthStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	m := decodeJSONMap(t, rec.Body)
	if m["logged_in"] != true {
		t.Fatalf("logged_in = %v, want true", m["logged_in"])
	}
}

func TestClaudeDeep_Status_ReadsCorrectPath(t *testing.T) {
	stubClaudeSeams(t)
	var gotPath string
	claudeReadToken = func(path string) string {
		gotPath = path
		return ""
	}
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/claude-auth/status", nil)
	s.handleClaudeAuthStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	if gotPath != claudeCredPath {
		t.Errorf("readToken path = %q, want %q", gotPath, claudeCredPath)
	}
}

// ---------------------------------------------------------------------------
// handleClaudeAuthStart
// ---------------------------------------------------------------------------

func TestClaudeDeep_Start_ReturnsAuthorizeURL(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/start", nil)
	markOwnerRequest(req)
	s.handleClaudeAuthStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	m := decodeJSONMap(t, rec.Body)
	authURL, ok := m["authorize_url"].(string)
	if !ok || authURL == "" {
		t.Fatal("authorize_url missing or empty")
	}
	if !strings.Contains(authURL, "code_challenge_method=S256") {
		t.Error("authorize_url missing PKCE S256 challenge method")
	}
	if !strings.Contains(authURL, "client_id=") {
		t.Error("authorize_url missing client_id")
	}
	if !strings.Contains(authURL, "response_type=code") {
		t.Error("authorize_url missing response_type=code")
	}
}

func TestClaudeDeep_Start_StoresPKCEState(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/start", nil)
	markOwnerRequest(req)
	s.handleClaudeAuthStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	s.claudeOAuthFlow.mu.Lock()
	defer s.claudeOAuthFlow.mu.Unlock()
	if s.claudeOAuthFlow.codeVerifier == "" {
		t.Error("codeVerifier not stored after start")
	}
	if s.claudeOAuthFlow.state == "" {
		t.Error("state not stored after start")
	}
	if s.claudeOAuthFlow.expiresAt.Before(time.Now()) {
		t.Error("flow expiry is in the past")
	}
	// Expiry should be ~10 minutes from now.
	if s.claudeOAuthFlow.expiresAt.After(time.Now().Add(11 * time.Minute)) {
		t.Error("flow expiry is too far in the future")
	}
}

func TestClaudeDeep_Start_UniqueState(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	// Call start twice, states should differ.
	req1 := httptest.NewRequest("POST", "/api/claude-auth/start", nil)
	markOwnerRequest(req1)
	s.handleClaudeAuthStart(httptest.NewRecorder(), req1)
	s.claudeOAuthFlow.mu.Lock()
	state1 := s.claudeOAuthFlow.state
	s.claudeOAuthFlow.mu.Unlock()

	req2 := httptest.NewRequest("POST", "/api/claude-auth/start", nil)
	markOwnerRequest(req2)
	s.handleClaudeAuthStart(httptest.NewRecorder(), req2)
	s.claudeOAuthFlow.mu.Lock()
	state2 := s.claudeOAuthFlow.state
	s.claudeOAuthFlow.mu.Unlock()

	if state1 == state2 {
		t.Error("two starts produced identical state values")
	}
}

// ---------------------------------------------------------------------------
// handleClaudeAuthExchange — error paths
// ---------------------------------------------------------------------------

func TestClaudeDeep_Exchange_InvalidJSON(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange", strings.NewReader("not json"))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid JSON") {
		t.Errorf("body = %q, want 'invalid JSON'", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_NoCode(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no authorization code") {
		t.Errorf("body = %q, want 'no authorization code'", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_ExpiredFlow(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	s.claudeOAuthFlow.codeVerifier = "test-verifier"
	s.claudeOAuthFlow.state = "test-state"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(-time.Hour)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"auth-code-123"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("body = %q, want mention of 'expired'", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_EmptyVerifier(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"auth-code-123"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("body = %q, want mention of 'expired'", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_CallbackURLWithError(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"callback_url":"https://example.com/cb?error=access_denied&error_description=user+denied"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Authorization denied") {
		t.Errorf("body = %q, want 'Authorization denied'", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "access_denied") {
		t.Errorf("body = %q, want error type included", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_CallbackURLExtractsCode(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	s.claudeOAuthFlow.codeVerifier = "test-verifier"
	s.claudeOAuthFlow.state = "test-state"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	var gotCode string
	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		gotCode = code
		return &claude.OAuthTokens{
			AccessToken: "sk-test-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"callback_url":"https://example.com/cb?code=extracted-456"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotCode != "extracted-456" {
		t.Errorf("exchange got code = %q, want 'extracted-456'", gotCode)
	}
}

func TestClaudeDeep_Exchange_InvalidGrant(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	s.claudeOAuthFlow.codeVerifier = "test-verifier"
	s.claudeOAuthFlow.state = "test-state"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		return nil, fmt.Errorf("token error: invalid_grant — code already used")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"used-code"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired or already used") {
		t.Errorf("body = %q, want 'expired or already used'", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_GenericError(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	s.claudeOAuthFlow.codeVerifier = "test-verifier"
	s.claudeOAuthFlow.state = "test-state"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		return nil, fmt.Errorf("network timeout")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"some-code"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Token exchange failed") {
		t.Errorf("body = %q, want 'Token exchange failed'", rec.Body.String())
	}
}

func TestClaudeDeep_Exchange_WriteCredsError(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	s.claudeOAuthFlow.codeVerifier = "test-verifier"
	s.claudeOAuthFlow.state = "test-state"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		return &claude.OAuthTokens{AccessToken: "sk-test"}, nil
	}
	claudeWriteCreds = func(tok *claude.OAuthTokens, path string) error {
		return fmt.Errorf("disk full")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"some-code"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to save credentials") {
		t.Errorf("body = %q, want 'Failed to save credentials'", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleClaudeAuthExchange — success path
// ---------------------------------------------------------------------------

func TestClaudeDeep_Exchange_Success(t *testing.T) {
	tmp := stubClaudeSeams(t)
	s := claudeDeepServer(t)

	claudeCredPath = filepath.Join(tmp, "creds.json")
	claudeTokenFilePath = filepath.Join(tmp, "token")

	s.claudeOAuthFlow.codeVerifier = "real-verifier"
	s.claudeOAuthFlow.state = "real-state"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	var exchangedCode, exchangedVerifier, exchangedRedirect string
	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		exchangedCode = code
		exchangedVerifier = verifier
		exchangedRedirect = redirect
		return &claude.OAuthTokens{
			AccessToken:  "sk-success-token",
			RefreshToken: "rt-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
			Scopes:       []string{"user:inference"},
		}, nil
	}

	var writtenPath string
	var writtenToken string
	claudeWriteCreds = func(tok *claude.OAuthTokens, path string) error {
		writtenPath = path
		writtenToken = tok.AccessToken
		return nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"good-code"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	m := decodeJSONMap(t, rec.Body)
	if m["status"] != "complete" {
		t.Errorf("status = %v, want 'complete'", m["status"])
	}

	// Verify exchange was called with correct arguments.
	if exchangedCode != "good-code" {
		t.Errorf("exchange got code = %q, want 'good-code'", exchangedCode)
	}
	if exchangedVerifier != "real-verifier" {
		t.Errorf("exchange got verifier = %q, want 'real-verifier'", exchangedVerifier)
	}
	if exchangedRedirect != claudeRedirectURI {
		t.Errorf("exchange got redirect = %q, want %q", exchangedRedirect, claudeRedirectURI)
	}

	// Verify credentials were written to correct path.
	if writtenPath != claudeCredPath {
		t.Errorf("writeCreds path = %q, want %q", writtenPath, claudeCredPath)
	}
	if writtenToken != "sk-success-token" {
		t.Errorf("writeCreds token = %q, want 'sk-success-token'", writtenToken)
	}

	// Verify PKCE state was cleared after success.
	s.claudeOAuthFlow.mu.Lock()
	if s.claudeOAuthFlow.codeVerifier != "" {
		t.Error("codeVerifier not cleared after success")
	}
	if s.claudeOAuthFlow.state != "" {
		t.Error("state not cleared after success")
	}
	s.claudeOAuthFlow.mu.Unlock()
}

func TestClaudeDeep_Exchange_DirectCodePreferred(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)

	s.claudeOAuthFlow.codeVerifier = "v"
	s.claudeOAuthFlow.state = "s"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	var gotCode string
	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		gotCode = code
		return &claude.OAuthTokens{AccessToken: "sk-t"}, nil
	}

	// When both code and callback_url are present, code takes priority.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange",
		strings.NewReader(`{"code":"direct-code","callback_url":"https://x/cb?code=from-url"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	if gotCode != "direct-code" {
		t.Errorf("exchange got code = %q, want 'direct-code' (direct should take priority)", gotCode)
	}
}

// ---------------------------------------------------------------------------
// handleClaudeAuthLogout
// ---------------------------------------------------------------------------

func TestClaudeDeep_Logout_RemovesFiles(t *testing.T) {
	tmp := stubClaudeSeams(t)
	s := claudeDeepServer(t)

	claudeCredPath = filepath.Join(tmp, "creds.json")
	claudeTokenFilePath = filepath.Join(tmp, "token")

	// Create sentinel files.
	os.WriteFile(claudeCredPath, []byte("{}"), 0o600)
	os.WriteFile(claudeTokenFilePath, []byte("tok"), 0o600)

	var removedPaths []string
	claudeRemoveFile = func(path string) error {
		removedPaths = append(removedPaths, path)
		return os.Remove(path)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/logout", nil)
	markOwnerRequest(req)
	s.handleClaudeAuthLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	m := decodeJSONMap(t, rec.Body)
	if m["status"] != "logged_out" {
		t.Errorf("status = %v, want 'logged_out'", m["status"])
	}
	if len(removedPaths) != 2 {
		t.Fatalf("removed %d files, want 2: %v", len(removedPaths), removedPaths)
	}

	// Verify files actually removed.
	if _, err := os.Stat(claudeCredPath); !os.IsNotExist(err) {
		t.Error("credentials file still exists after logout")
	}
	if _, err := os.Stat(claudeTokenFilePath); !os.IsNotExist(err) {
		t.Error("token file still exists after logout")
	}
}

func TestClaudeDeep_Logout_NilAgentMgr(t *testing.T) {
	stubClaudeSeams(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.deps = &Dependencies{Logger: logger}
	// deps.AgentMgr is nil — must not panic.

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/logout", nil)
	markOwnerRequest(req)
	s.handleClaudeAuthLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Route registration — smoke test via httptest.Server
// ---------------------------------------------------------------------------

func TestClaudeDeep_Routes_Smoke(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)
	s.registerClaudeAuthRoutes()

	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// GET status endpoint.
	resp, err := http.Get(ts.URL + "/api/claude-auth/status")
	if err != nil {
		t.Fatalf("GET /api/claude-auth/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status endpoint = %d, want 200", resp.StatusCode)
	}

	// POST start endpoint.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/claude-auth/start", nil)
	if err != nil {
		t.Fatalf("build POST /api/claude-auth/start: %v", err)
	}
	markOwnerRequest(req)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/claude-auth/start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("start endpoint = %d, want 200", resp.StatusCode)
	}
}
