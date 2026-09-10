package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/terminalassert"
)

func TestTerminalHandoffExpires(t *testing.T) {
	s := NewServer(0, nil)
	code := s.createTerminalHandoff("alice", config.RoleOwner)
	if code == "" {
		t.Fatal("createTerminalHandoff returned empty code")
	}
	s.terminalHandoffMu.Lock()
	h := s.terminalHandoffs[code]
	h.ExpiresAt = time.Now().Add(-time.Second)
	s.terminalHandoffs[code] = h
	s.terminalHandoffMu.Unlock()
	if _, _, ok := s.redeemTerminalHandoff(code); ok {
		t.Fatal("expired terminal handoff code redeemed successfully")
	}
}

func TestRedactedRequestURIRedactsTokenAndCodeQueryValues(t *testing.T) {
	u, err := url.Parse("/terminal/?arg=hive-quality&token=secret&code=abc123&keep=value")
	if err != nil {
		t.Fatal(err)
	}
	got := redactedRequestURI(u)
	if got == "" || got == u.RequestURI() {
		t.Fatalf("redactedRequestURI did not change sensitive URL: %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "abc123") {
		t.Fatalf("redactedRequestURI leaked sensitive values: %q", got)
	}
	if !(strings.Contains(got, "token=%5Bredacted%5D") || strings.Contains(got, "token=[redacted]")) || !(strings.Contains(got, "code=%5Bredacted%5D") || strings.Contains(got, "code=[redacted]")) || !strings.Contains(got, "keep=value") {
		t.Fatalf("redactedRequestURI lost expected query values: %q", got)
	}
}

func TestTerminalAssertionCookieAuthenticatesTerminalSubrequests(t *testing.T) {
	last := startFakeTtyd(t)
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	s.setTerminalAssertionCookie(w, req, "alice", config.RoleOwner)
	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("no terminal assertion cookie minted")
	}

	rec := httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/terminal/ws?arg=hive-quality", nil)
	req.AddCookie(c)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("terminal assertion cookie request = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if *last != "/ws?arg=hive-quality" {
		t.Fatalf("backend saw %q", *last)
	}
}

func TestAuthMiddlewareSessionCookieStillWorksWithTokenConfigured(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{}}
	sid := s.createUserSession("alice", config.RoleOwner)
	seen := false
	h := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		if got := r.Header.Get("X-Hive-User"); got != "alice" {
			t.Fatalf("X-Hive-User = %q, want alice", got)
		}
	}))
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !seen {
		t.Fatalf("session cookie auth = %d seen=%v", rec.Code, seen)
	}
}

func TestReadOnlySessionCannotOpenTerminal(t *testing.T) {
	last := startFakeTtyd(t)
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{}}
	sid := s.createUserSession("carol", config.RoleRead)
	req := httptest.NewRequest("GET", "/terminal/?arg=hive-quality", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only terminal via session = %d, want 403", rec.Code)
	}
	if *last != "" {
		t.Fatalf("read-only terminal reached ttyd: %q", *last)
	}
}

func TestTrustedTerminalRequestBurnsHandoffCode(t *testing.T) {
	last := startFakeTtyd(t)
	s := NewServerWithAuth(0, "secret", nil)
	code := s.createTerminalHandoff("alice", config.RoleOwner)
	req := httptest.NewRequest("GET", "/terminal/?arg=hive-quality&code="+code, nil)
	req.Header.Set("X-Hive-User", "alice")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(proxyAuthHeader, "secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted terminal request = %d, want 200", rec.Code)
	}
	if strings.Contains(*last, "code=") {
		t.Fatalf("backend saw handoff code: %q", *last)
	}
	if _, _, ok := s.redeemTerminalHandoff(code); ok {
		t.Fatal("trusted terminal request did not burn handoff code")
	}
}

func TestHandleCreateTerminalHandoff_RoleForbiddenContentType(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")
	req := httptest.NewRequest("POST", terminalHandoffPath, nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if body["error"] != "terminal access requires owner or read-write role" {
		t.Fatalf("error = %q, want %q", body["error"], "terminal access requires owner or read-write role")
	}
}

// TestHandleCreateTerminalHandoff_NoHubLanesFallsBackAndSucceeds pins the
// #6489 fix: a standalone spoke with NEITHER hub lane configured (no
// HIVE_TERMINAL_KEY, no HIVE_HUB_SECRET) no longer 503s — terminalassert's
// lane-3 persisted per-instance fallback key resolves instead, so the handoff
// mint succeeds like any hub-provisioned hive. Before the fix this was a 503
// "terminal handoff requires terminal signing key and hive id", which is what
// #6489 reported (masked further by nginx's error-page rewrite, #6494/#6496).
func TestHandleCreateTerminalHandoff_NoHubLanesFallsBackAndSucceeds(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")
	t.Setenv(hub.EnvTerminalKey, "")
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv(terminalassert.EnvFallbackKeyDir, t.TempDir())

	req := httptest.NewRequest("POST", terminalHandoffPath, nil)
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if body["code"] == "" {
		t.Fatalf("expected a handoff code, got body %q", rec.Body.String())
	}
}

func TestHandleCreateTerminalHandoff_NoHiveID503ContentType(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")
	s.deps.Config.HiveID = ""

	req := httptest.NewRequest("POST", terminalHandoffPath, nil)
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if body["error"] != "terminal handoff requires terminal signing key and hive id" {
		t.Fatalf("error = %q, want %q", body["error"], "terminal handoff requires terminal signing key and hive id")
	}
}

func TestWriteTerminalRoleForbidden_ContentType(t *testing.T) {
	t.Run("API path gets JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/terminal/handoff", nil)

		writeTerminalRoleForbidden(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "terminal access requires owner or read-write role" {
			t.Fatalf("API refusal body = %q (err=%v)", rec.Body.String(), err)
		}
	})

	t.Run("Browser path gets text/plain", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/terminal/", nil)

		writeTerminalRoleForbidden(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, want text/plain", ct)
		}
		if strings.Contains(rec.Body.String(), "{") {
			t.Fatalf("browser-facing refusal looks like JSON: %q", rec.Body.String())
		}
	})
}

func TestWriteQueryTokenRejected_ContentType(t *testing.T) {
	t.Run("API path gets JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/status?token=legacy", nil)

		writeQueryTokenRejected(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "query-string dashboard token authentication is no longer supported") {
			t.Fatalf("API refusal body = %q (err=%v)", rec.Body.String(), err)
		}
	})

	t.Run("Browser path gets text/plain", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/terminal/?token=legacy", nil)

		writeQueryTokenRejected(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, want text/plain", ct)
		}
		if strings.Contains(rec.Body.String(), "{") {
			t.Fatalf("browser-facing refusal looks like JSON: %q", rec.Body.String())
		}
	})
}

func TestAuthenticateMiddleware_QueryTokenRejected_ContentType(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{}}

	t.Run("API route with query token gets 401 JSON", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status?token=legacy", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "query-string dashboard token authentication is no longer supported") {
			t.Fatalf("API refusal body = %q (err=%v)", rec.Body.String(), err)
		}
	})

	t.Run("Non-API route with query token gets 401 text/plain", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/terminal/?token=legacy", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, want text/plain", ct)
		}
	})
}
