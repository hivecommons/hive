package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// #4171 — SSO with sibling first-party products (dibs.kubestellar.io).
//
// Two halves, both pinned here:
//   1. GET /api/saas/whoami — the identity bridge dibs calls server-to-server
//      with the browser's forwarded hive_hub_user cookie.
//   2. The session cookie's Domain widening from .hive.kubestellar.io to the
//      parent .kubestellar.io (so siblings receive it), including the
//      host-only fallback for local/dev hosts and logout clearing both scopes.
// ============================================================

func TestWhoamiGitHubUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/whoami", nil)
	req.AddCookie(testAuthCookie("octocat"))
	rec := httptest.NewRecorder()
	s.handleSaaSWhoami(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("whoami with a valid session = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the answer is per-session and revocable", cc)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("whoami body is not JSON: %v", err)
	}
	// username must be the STABLE key: the bare GitHub login, exactly what
	// every SaaSUser record and hive grant is keyed on.
	if got["username"] != "octocat" {
		t.Errorf("username = %q, want octocat (bare GitHub login)", got["username"])
	}
	if got["display_name"] != "octocat" {
		t.Errorf("display_name = %q, want the login fallback for a GitHub user with no stored DisplayName", got["display_name"])
	}
	if got["avatar_url"] != "https://github.com/octocat.png" {
		t.Errorf("avatar_url = %q, want the derived GitHub avatar", got["avatar_url"])
	}
	if _, ok := got["email"]; !ok {
		t.Error("email key missing from whoami payload")
	}
}

func TestWhoamiOIDCUserUsesCanonicalKey(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	if err := saveSaaSUser(&SaaSUser{
		CanonicalID: "google:1078",
		Provider:    "google",
		DisplayName: "Ada Lovelace",
		Email:       "ada@example.com",
		AvatarURL:   "https://lh3.example/pic.png",
		Hives:       map[string]string{},
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/whoami", nil)
	req.AddCookie(testAuthCookie("google:1078"))
	rec := httptest.NewRecorder()
	s.handleSaaSWhoami(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("whoami with a valid OIDC session = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("whoami body is not JSON: %v", err)
	}
	// The stable key for an OIDC user is the canonical provider:sub form —
	// NEVER the display name or email (emails are reassignable, subs are not).
	if got["username"] != "google:1078" {
		t.Errorf("username = %q, want the canonical provider:sub key google:1078", got["username"])
	}
	if got["display_name"] != "Ada Lovelace" {
		t.Errorf("display_name = %q, want the provider-asserted DisplayName", got["display_name"])
	}
	if got["email"] != "ada@example.com" {
		t.Errorf("email = %q, want the stored provider email", got["email"])
	}
	if got["avatar_url"] != "https://lh3.example/pic.png" {
		t.Errorf("avatar_url = %q, want the stored provider avatar", got["avatar_url"])
	}
}

func TestWhoamiUnauthenticated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	cases := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"forged cookie", &http.Cookie{Name: "hive_hub_user", Value: "attacker-chosen"}},
		{"valid signature, no such user", testAuthCookie("ghost-user-xyz")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/whoami", nil)
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			rec := httptest.NewRecorder()
			s.handleSaaSWhoami(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("whoami (%s) = %d, want 401", tc.name, rec.Code)
			}
			var got map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("401 body must be JSON for the dibs bridge, got %q: %v", rec.Body.String(), err)
			}
			if got["error"] == "" {
				t.Errorf("401 body carries no error field: %q", rec.Body.String())
			}
		})
	}
}

// TestWhoamiIsGETOnly pins the mux method pattern: dibs only ever issues GET,
// and there is no reason to let this route answer anything else.
func TestWhoamiIsGETOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := NewHubServer(0, slog.Default(), "", "")
	req := httptest.NewRequest(http.MethodPost, "https://hive.kubestellar.io/api/saas/whoami", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/saas/whoami = %d, want 405", rec.Code)
	}
}

// TestSessionCookieDomainWidenedToParent pins the #4171 scope: minted on the
// production hub host, the session cookie must carry Domain=.kubestellar.io so
// BOTH the hosted spokes (*.hive.kubestellar.io — deeper subdomains, still
// covered) and sibling products (dibs.kubestellar.io) receive it. The mint
// must also expire the legacy .hive.kubestellar.io-scoped copy so a fresh
// login converges to one session cookie.
func TestSessionCookieDomainWidenedToParent(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/auth/callback", nil)
	if !s.mintSessionCookies(rec, req, "github:alice") {
		t.Fatal("mintSessionCookies failed")
	}

	var live, legacyClear *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name != "hive_hub_user" {
			continue
		}
		if c.Value != "" {
			live = c
		} else {
			legacyClear = c
		}
	}
	if live == nil {
		t.Fatal("mint emitted no live hive_hub_user cookie")
	}
	if live.Domain != "kubestellar.io" {
		t.Fatalf("session cookie Domain = %q, want .kubestellar.io — dibs.kubestellar.io never receives it otherwise", live.Domain)
	}
	if !live.Secure || !live.HttpOnly || live.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie lost hardening: Secure=%v HttpOnly=%v SameSite=%v", live.Secure, live.HttpOnly, live.SameSite)
	}
	if legacyClear == nil {
		t.Fatal("mint did not expire the legacy .hive.kubestellar.io-scoped copy — the jar would hold two sessions")
	}
	if legacyClear.Domain != "hive.kubestellar.io" || legacyClear.MaxAge >= 0 {
		t.Errorf("legacy clear cookie Domain=%q MaxAge=%d, want .hive.kubestellar.io with MaxAge<0", legacyClear.Domain, legacyClear.MaxAge)
	}
}

// TestSessionCookieHostOnlyForLocalDev pins the fallback: on a host that is
// not under kubestellar.io the Domain attribute must be omitted entirely — a
// browser rejects a Set-Cookie whose Domain does not cover the request host,
// so widening there would break local login outright.
func TestSessionCookieHostOnlyForLocalDev(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "hub.example.internal"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/auth/callback", nil)
		if !s.mintSessionCookies(rec, req, "github:alice") {
			t.Fatal("mintSessionCookies failed")
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == "hive_hub_user" && c.Domain != "" {
				t.Errorf("host %s: session cookie carries Domain=%q, want host-only", host, c.Domain)
			}
		}
	}

	// Logout on a dev host must clear the same host-only cookie.
	rec := httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/auth/logout", nil))
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name != "hive_hub_user" {
			continue
		}
		cleared = true
		if c.Domain != "" {
			t.Errorf("dev logout clears Domain=%q, want host-only to match the mint", c.Domain)
		}
		if c.MaxAge >= 0 {
			t.Errorf("dev logout cookie is not a deletion (MaxAge=%d)", c.MaxAge)
		}
	}
	if !cleared {
		t.Fatal("dev logout emitted no hive_hub_user deletion")
	}
}

// TestSessionAcceptsEitherCookieCopy pins the rollout behaviour: while a
// browser holds both the legacy .hive.kubestellar.io-scoped cookie and the new
// parent-scoped one, it sends BOTH under the same name, in jar order the hub
// does not control. The hub must find the valid session regardless of which
// copy comes first.
func TestSessionAcceptsEitherCookieCopy(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "alice")

	valid := testAuthCookie("alice")
	stale := &http.Cookie{Name: "hive_hub_user", Value: "stale-or-forged"}

	for _, order := range [][]*http.Cookie{{stale, valid}, {valid, stale}} {
		req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/whoami", nil)
		for _, c := range order {
			req.AddCookie(c)
		}
		if got := s.getRealAuthUser(req); got != "alice" {
			t.Errorf("with cookie order %q first: getRealAuthUser = %q, want alice", order[0].Value, got)
		}
	}
}

// TestSessionCookieDomainDerivation pins sessionCookieDomain itself so the
// host classification is explicit.
func TestSessionCookieDomainDerivation(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"hive.kubestellar.io", ".kubestellar.io"},
		{"hive.kubestellar.io:443", ".kubestellar.io"},
		{"kubestellar.io", ".kubestellar.io"},
		{"dibs.kubestellar.io", ".kubestellar.io"},
		{"myspoke.hive.kubestellar.io", ".kubestellar.io"},
		{"localhost", ""},
		{"localhost:8080", ""},
		{"127.0.0.1:9090", ""},
		{"example.com", ""},
		// A lookalike must NOT be treated as under the parent domain.
		{"evilkubestellar.io", ""},
		{"kubestellar.io.evil.com", ""},
	}
	for _, tc := range cases {
		if got := sessionCookieDomain(tc.host); got != tc.want {
			t.Errorf("sessionCookieDomain(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
