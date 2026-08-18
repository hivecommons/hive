package dashboard

import (
	"net/http"
	"testing"
	"time"
)

func caServer(t *testing.T) *Server {
	t.Helper()
	return covApiServer(t)
}

func TestCovCA_Status(t *testing.T) {
	s := caServer(t)
	// No credentials on disk → logged out.
	if rec := doGet(s, "/api/claude-auth/status"); rec.Code != http.StatusOK {
		t.Fatalf("claude status: %d", rec.Code)
	}
}

func TestCovCA_Start(t *testing.T) {
	s := caServer(t)
	// Start generates a PKCE flow + auth URL (no network).
	if rec := doPost(s, "/api/claude-auth/start", nil); rec.Code != http.StatusOK {
		t.Fatalf("claude start: %d", rec.Code)
	}
}

func TestCovCA_Exchange_BadJSON(t *testing.T) {
	s := caServer(t)
	if rec := doPostRaw(s, "/api/claude-auth/exchange", "not-json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange bad json: %d", rec.Code)
	}
}

func TestCovCA_Exchange_NoCode(t *testing.T) {
	s := caServer(t)
	if rec := doPost(s, "/api/claude-auth/exchange", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange no code: %d", rec.Code)
	}
}

func TestCovCA_Exchange_CallbackWithError(t *testing.T) {
	s := caServer(t)
	rec := doPost(s, "/api/claude-auth/exchange", map[string]any{
		"callback_url": "https://x/cb?error=access_denied&error_description=nope",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange callback error: %d", rec.Code)
	}
}

func TestCovCA_Exchange_CallbackBadURL(t *testing.T) {
	s := caServer(t)
	// A control character makes url.Parse fail.
	rec := doPost(s, "/api/claude-auth/exchange", map[string]any{
		"callback_url": "http://\x7f\x00",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange bad url: %d", rec.Code)
	}
}

func TestCovCA_Exchange_ExpiredSession(t *testing.T) {
	s := caServer(t)
	// A code present but no active PKCE flow (verifier empty) → session expired.
	rec := doPost(s, "/api/claude-auth/exchange", map[string]any{"code": "abc123"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange expired: %d", rec.Code)
	}
}

func TestCovCA_Exchange_CodeFromCallback(t *testing.T) {
	s := caServer(t)
	// Prime a PKCE flow so the verifier is present; the code is pulled from the
	// callback URL and the exchange proceeds to claude.ExchangeCode (which fails
	// against the real endpoint) — covering the code-extraction + exchange path.
	s.claudeOAuthFlow.mu.Lock()
	s.claudeOAuthFlow.codeVerifier = "test-verifier"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(time.Hour)
	s.claudeOAuthFlow.mu.Unlock()

	rec := doPost(s, "/api/claude-auth/exchange", map[string]any{
		"callback_url": "https://claude.ai/cb?code=fromcallback",
	})
	// Real token exchange fails (no network / invalid) → 400 or 500; either
	// exercises the exchange branch.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusInternalServerError {
		t.Fatalf("exchange from callback: unexpected %d", rec.Code)
	}
}

func TestCovCA_Logout(t *testing.T) {
	s := caServer(t)
	if rec := doPost(s, "/api/claude-auth/logout", nil); rec.Code != http.StatusOK {
		t.Fatalf("claude logout: %d", rec.Code)
	}
}
