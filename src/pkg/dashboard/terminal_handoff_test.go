package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
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
