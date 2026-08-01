package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
)

// --- handleCopilotAuthStatus tests ---

func TestCopilotAuthStatus_NotLoggedIn(t *testing.T) {
	// Token path doesn't exist in test env → logged_in = false
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registerCopilotAuthRoutes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)

	if body["logged_in"] != false {
		t.Errorf("expected logged_in=false (no token file), got %v", body["logged_in"])
	}
	if body["pending"] != false {
		t.Errorf("expected pending=false, got %v", body["pending"])
	}
	if body["error"] != "" {
		t.Errorf("expected empty error, got %q", body["error"])
	}
}

func TestCopilotAuthStatus_LoggedInViaEnvVar(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registerCopilotAuthRoutes()

	t.Setenv("COPILOT_GITHUB_TOKEN", "gho_envtoken")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil)
	s.mux.ServeHTTP(rec, req)

	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)

	if body["logged_in"] != true {
		t.Errorf("expected logged_in=true with COPILOT_GITHUB_TOKEN set, got %v", body["logged_in"])
	}
}

func TestCopilotAuthStatus_PendingState(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registerCopilotAuthRoutes()

	// Simulate an in-progress poll
	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = true
	s.copilotAuthFlow.lastError = ""
	s.copilotAuthFlow.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil)
	s.mux.ServeHTTP(rec, req)

	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)

	if body["pending"] != true {
		t.Errorf("expected pending=true during poll, got %v", body["pending"])
	}
}

func TestCopilotAuthStatus_ErrorState(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registerCopilotAuthRoutes()

	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = false
	s.copilotAuthFlow.lastError = "access_denied — user rejected"
	s.copilotAuthFlow.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil)
	s.mux.ServeHTTP(rec, req)

	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)

	if body["pending"] != false {
		t.Errorf("expected pending=false after error, got %v", body["pending"])
	}
	errStr, _ := body["error"].(string)
	if !strings.Contains(errStr, "access_denied") {
		t.Errorf("expected error containing 'access_denied', got %q", errStr)
	}
}

// --- saveCopilotToken tests ---

func TestSaveCopilotToken_WritesAndRenames(t *testing.T) {
	// saveCopilotToken writes to agent.CopilotUserTokenPath (/data/copilot-user-token).
	// In CI this path is writable; skip if not.
	dir := agent.CopilotUserTokenPath[:strings.LastIndex(agent.CopilotUserTokenPath, "/")]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create dir %s: %v", dir, err)
	}
	// Clean up after test
	defer os.Remove(agent.CopilotUserTokenPath)
	defer os.Remove(agent.CopilotUserTokenPath + ".tmp")

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	err := s.saveCopilotToken("gho_test_token_abc")
	if err != nil {
		t.Fatalf("saveCopilotToken: %v", err)
	}

	// Verify file contents
	data, err := os.ReadFile(agent.CopilotUserTokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != "gho_test_token_abc" {
		t.Errorf("token mismatch: got %q, want %q", string(data), "gho_test_token_abc")
	}

	// Verify permissions are 0600
	info, err := os.Stat(agent.CopilotUserTokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions: got %o, want 0600", info.Mode().Perm())
	}

	// Verify .tmp file was removed (by os.Rename)
	if _, err := os.Stat(agent.CopilotUserTokenPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not exist after rename, got: %v", err)
	}
}

func TestSaveCopilotToken_OverwriteExisting(t *testing.T) {
	dir := agent.CopilotUserTokenPath[:strings.LastIndex(agent.CopilotUserTokenPath, "/")]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create dir %s: %v", dir, err)
	}
	defer os.Remove(agent.CopilotUserTokenPath)
	defer os.Remove(agent.CopilotUserTokenPath + ".tmp")

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Write initial token
	s.saveCopilotToken("gho_first")
	// Overwrite
	err := s.saveCopilotToken("gho_second")
	if err != nil {
		t.Fatalf("overwrite failed: %v", err)
	}

	data, _ := os.ReadFile(agent.CopilotUserTokenPath)
	if string(data) != "gho_second" {
		t.Errorf("expected 'gho_second', got %q", string(data))
	}
}

// --- handleCopilotAuthLogout tests ---

func TestCopilotAuthLogout_RemovesToken(t *testing.T) {
	dir := agent.CopilotUserTokenPath[:strings.LastIndex(agent.CopilotUserTokenPath, "/")]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create dir %s: %v", dir, err)
	}
	// Pre-create a token file
	os.WriteFile(agent.CopilotUserTokenPath, []byte("gho_logout_me"), 0o600)
	defer os.Remove(agent.CopilotUserTokenPath)

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registerCopilotAuthRoutes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/copilot-auth/logout", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "logged_out" {
		t.Errorf("expected status='logged_out', got %v", body["status"])
	}

	// Token file should be gone
	if _, err := os.Stat(agent.CopilotUserTokenPath); !os.IsNotExist(err) {
		t.Errorf("token file should not exist after logout")
	}
}

func TestCopilotAuthLogout_StatusAfterLogout(t *testing.T) {
	dir := agent.CopilotUserTokenPath[:strings.LastIndex(agent.CopilotUserTokenPath, "/")]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create dir %s: %v", dir, err)
	}
	os.WriteFile(agent.CopilotUserTokenPath, []byte("gho_pre_logout"), 0o600)
	defer os.Remove(agent.CopilotUserTokenPath)

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registerCopilotAuthRoutes()

	// Before: logged in
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil)
	s.mux.ServeHTTP(rec, req)
	var before map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&before)
	if before["logged_in"] != true {
		t.Fatalf("expected logged_in=true before logout")
	}

	// Logout
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/copilot-auth/logout", nil)
	s.mux.ServeHTTP(rec, req)

	// After: not logged in
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/copilot-auth/status", nil)
	s.mux.ServeHTTP(rec, req)
	var after map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&after)
	if after["logged_in"] != false {
		t.Errorf("expected logged_in=false after logout, got %v", after["logged_in"])
	}
}

// --- pollCopilotToken state machine tests ---

func TestPollCopilotToken_SetsDoneOnExpiry(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Start a poll with zero expiry — it should exit immediately with "expired"
	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = true
	s.copilotAuthFlow.mu.Unlock()

	// Use a very short expiry (1 nanosecond) so the loop exits instantly.
	// We can't easily intercept the HTTP calls, but we can verify
	// the deadline logic: with 0 expiry the for-loop condition fails immediately.
	go s.pollCopilotToken("dc_fake", 1, time.Nanosecond)

	// Wait a bit for the goroutine to finish
	time.Sleep(50 * time.Millisecond)

	s.copilotAuthFlow.mu.Lock()
	polling := s.copilotAuthFlow.polling
	lastErr := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()

	if polling {
		t.Errorf("expected polling=false after expiry")
	}
	if !strings.Contains(lastErr, "expired") {
		t.Errorf("expected error containing 'expired', got %q", lastErr)
	}
}

// --- Constants validation ---

func TestCopilotAuthConstants(t *testing.T) {
	if copilotClientID == "" {
		t.Error("copilotClientID must not be empty")
	}
	if !strings.HasPrefix(copilotClientID, "Iv1.") {
		t.Errorf("copilotClientID should start with 'Iv1.', got %q", copilotClientID)
	}
	if copilotDefaultPollIntervalSec <= 0 {
		t.Errorf("default poll interval must be > 0, got %d", copilotDefaultPollIntervalSec)
	}
	if copilotSlowDownBumpSec <= 0 {
		t.Errorf("slow down bump must be > 0, got %d", copilotSlowDownBumpSec)
	}
	if copilotFlowExpiry != 15*time.Minute {
		t.Errorf("flow expiry: got %v, want 15m", copilotFlowExpiry)
	}
	if copilotHTTPTimeout != 30*time.Second {
		t.Errorf("HTTP timeout: got %v, want 30s", copilotHTTPTimeout)
	}

	// Verify CopilotUserTokenPath is defined and non-empty
	if agent.CopilotUserTokenPath == "" {
		t.Error("agent.CopilotUserTokenPath must not be empty")
	}
}
