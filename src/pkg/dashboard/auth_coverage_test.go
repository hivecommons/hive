package dashboard

import (
	"net/http"
	"testing"
)

// NOTE: claude.CredentialsPath / agent.CopilotUserTokenPath are package consts
// pointing at fixed /data paths, so these handlers read from an absent file in
// tests (giving the logged-out branch). copilot_auth.go's pollCopilotToken,
// saveCopilotToken, and handleCopilotAuthStart perform real time.Sleep /
// github.com network calls, so they are left uncovered rather than driven here.

func TestCovB_ClaudeAuthStatus_LoggedOut(t *testing.T) {
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doGet(s, "/api/claude-auth/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["logged_in"] != false {
		t.Fatalf("logged_in = %v, want false", got["logged_in"])
	}
}

func TestCovB_ClaudeAuthStart(t *testing.T) {
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doPost(s, "/api/claude-auth/start", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if u, _ := got["authorize_url"].(string); u == "" {
		t.Fatalf("authorize_url empty in %v", got)
	}
}

func TestCovB_ClaudeAuthExchange_Errors(t *testing.T) {
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	// Malformed JSON body.
	rec := covBPostRaw(s, "/api/claude-auth/exchange", "{bad")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}

	// No code and no callback_url.
	rec = doPost(s, "/api/claude-auth/exchange", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no code status = %d, want 400", rec.Code)
	}

	// callback_url carrying an error param.
	rec = doPost(s, "/api/claude-auth/exchange", map[string]interface{}{
		"callback_url": "https://example.com/cb?error=access_denied&error_description=nope",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("error param status = %d, want 400", rec.Code)
	}

	// A code present but no in-flight PKCE flow (verifier empty) → expired.
	rec = doPost(s, "/api/claude-auth/exchange", map[string]interface{}{
		"code": "some-code",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired flow status = %d, want 400", rec.Code)
	}

	// callback_url with an embedded code but still no flow → expired.
	rec = doPost(s, "/api/claude-auth/exchange", map[string]interface{}{
		"callback_url": "https://example.com/cb?code=xyz",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback code, expired flow status = %d, want 400", rec.Code)
	}
}

func TestCovB_ClaudeAuthLogout(t *testing.T) {
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doPost(s, "/api/claude-auth/logout", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["status"] != "logged_out" {
		t.Fatalf("status = %v, want logged_out", got["status"])
	}
}

func TestCovB_CopilotAuthStatus(t *testing.T) {
	withCopilotTokenPath(t)
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	// File absent and no env → logged_in false.
	rec := doGet(s, "/api/copilot-auth/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["logged_in"] != false {
		t.Fatalf("logged_in = %v, want false", got["logged_in"])
	}

	// Env token present → logged_in true.
	t.Setenv("COPILOT_GITHUB_TOKEN", "x")
	rec = doGet(s, "/api/copilot-auth/status")
	got = decodeJSON(t, rec)
	if got["logged_in"] != true {
		t.Fatalf("logged_in = %v, want true (env)", got["logged_in"])
	}
}

func TestCovB_CopilotAuthLogout(t *testing.T) {
	withCopilotTokenPath(t)
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doPost(s, "/api/copilot-auth/logout", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["status"] != "logged_out" {
		t.Fatalf("status = %v, want logged_out", got["status"])
	}
}
