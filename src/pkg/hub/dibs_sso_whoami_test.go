package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSessionCookieDomainForHost verifies host-based cookie domain resolution.
func TestSessionCookieDomainForHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"hive.kubestellar.io", ".kubestellar.io"},
		{"dibs.kubestellar.io", ".kubestellar.io"},
		{"hosted-hive-1.hive.kubestellar.io", ".kubestellar.io"},
		{"kubestellar.io", ".kubestellar.io"},
		{"hive.kubestellar.io:443", ".kubestellar.io"},
		{"dibs.kubestellar.io:8443", ".kubestellar.io"},
		{"localhost", ""},
		{"localhost:8080", ""},
		{"127.0.0.1", ""},
		{"127.0.0.1:3000", ""},
		{"example.com", ""},
		{"evil.kubestellar.io.attacker.com", ""},
		{"", ""},
	}

	for _, tc := range cases {
		got := sessionCookieDomainForHost(tc.host)
		if got != tc.want {
			t.Errorf("sessionCookieDomainForHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// TestMintSessionCookiesDomainScoping verifies that cookie minting scopes to .kubestellar.io
// for kubestellar.io hosts and host-only for localhost/dev.
func TestMintSessionCookiesDomainScoping(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")

	// 1. Host under kubestellar.io
	recProd := httptest.NewRecorder()
	if !s.mintSessionCookiesForHost(recProd, "hive.kubestellar.io", "github:alice") {
		t.Fatal("mintSessionCookiesForHost failed for hive.kubestellar.io")
	}
	cookiesProd := recProd.Result().Cookies()
	var prodCookie *http.Cookie
	for _, c := range cookiesProd {
		if c.Name == "hive_hub_user" {
			prodCookie = c
			break
		}
	}
	if prodCookie == nil {
		t.Fatal("no hive_hub_user cookie minted for production host")
	}
	if prodCookie.Domain != ".kubestellar.io" && prodCookie.Domain != "kubestellar.io" {
		t.Errorf("prod cookie Domain = %q, want %q", prodCookie.Domain, ".kubestellar.io")
	}
	if !prodCookie.Secure || !prodCookie.HttpOnly || prodCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("prod cookie lost hardening: Secure=%v HttpOnly=%v SameSite=%v",
			prodCookie.Secure, prodCookie.HttpOnly, prodCookie.SameSite)
	}

	// 2. Localhost dev host
	recDev := httptest.NewRecorder()
	if !s.mintSessionCookiesForHost(recDev, "localhost:8080", "github:alice") {
		t.Fatal("mintSessionCookiesForHost failed for localhost")
	}
	cookiesDev := recDev.Result().Cookies()
	var devCookie *http.Cookie
	for _, c := range cookiesDev {
		if c.Name == "hive_hub_user" {
			devCookie = c
			break
		}
	}
	if devCookie == nil {
		t.Fatal("no hive_hub_user cookie minted for dev host")
	}
	if devCookie.Domain != "" {
		t.Errorf("dev cookie Domain = %q, want empty (host-only)", devCookie.Domain)
	}
}

// TestLogoutClearsBothNewAndLegacyDomains verifies that handleLogout clears both
// .kubestellar.io and .hive.kubestellar.io domains on logout.
func TestLogoutClearsBothNewAndLegacyDomains(t *testing.T) {
	s := f10f15Server(t)

	// Logout request from hive.kubestellar.io
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Host = "hive.kubestellar.io"
	s.handleLogout(rec, req)

	var clearedParent, clearedLegacy bool
	for _, c := range rec.Result().Cookies() {
		if c.Name != "hive_hub_user" || c.MaxAge >= 0 {
			continue
		}
		if c.Domain == ".kubestellar.io" || c.Domain == "kubestellar.io" {
			clearedParent = true
		}
		if c.Domain == ".hive.kubestellar.io" || c.Domain == "hive.kubestellar.io" {
			clearedLegacy = true
		}
	}

	if !clearedParent {
		t.Error("logout did not clear .kubestellar.io cookie")
	}
	if !clearedLegacy {
		t.Error("logout did not clear legacy .hive.kubestellar.io cookie")
	}
}

// TestDualCookieAcceptanceDuringRollout verifies that when multiple hive_hub_user cookies
// are present in the request (e.g. legacy + new, or invalid + valid), authentication
// succeeds as long as at least one valid cookie is present.
func TestDualCookieAcceptanceDuringRollout(t *testing.T) {
	s := f10f15Server(t)
	mkUser(t, "alice")

	// Valid cookie for alice
	rec := httptest.NewRecorder()
	if !s.mintSessionCookies(rec, "github:alice") {
		t.Fatal("mintSessionCookies failed")
	}
	var validCookieVal string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" {
			validCookieVal = c.Value
		}
	}
	if validCookieVal == "" {
		t.Fatal("no valid cookie value")
	}

	invalidCookieVal := "invalid.signature.cookie"

	// Case 1: First cookie invalid, second cookie valid
	req1 := httptest.NewRequest(http.MethodGet, "/api/saas/whoami", nil)
	req1.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: invalidCookieVal})
	req1.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: validCookieVal})

	user1 := s.getRealAuthUser(req1)
	if user1 != "alice" && user1 != "github:alice" {
		t.Errorf("getRealAuthUser with invalid+valid cookies = %q, want alice", user1)
	}

	recWhoami := httptest.NewRecorder()
	s.handleWhoami(recWhoami, req1)
	if recWhoami.Code != http.StatusOK {
		t.Errorf("handleWhoami with invalid+valid cookies returned status %d, want 200", recWhoami.Code)
	}

	// Case 2: First cookie valid, second cookie invalid
	req2 := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	req2.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: validCookieVal})
	req2.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: invalidCookieVal})

	recAuthUser := httptest.NewRecorder()
	s.handleAuthUser(recAuthUser, req2)
	var authUserResp map[string]any
	if err := json.Unmarshal(recAuthUser.Body.Bytes(), &authUserResp); err != nil {
		t.Fatalf("json parse error: %v", err)
	}
	if authUserResp["authenticated"] != true {
		t.Errorf("handleAuthUser with valid+invalid cookies returned authenticated=false, want true")
	}
}

// TestWhoamiEndpoint tests the GET /api/saas/whoami endpoint thoroughly.
func TestWhoamiEndpoint(t *testing.T) {
	s := f10f15Server(t)

	// Create GitHub user "alice" without extra profile fields
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "alice",
		CanonicalID:    "github:alice",
		Provider:       "github",
		Hives:          map[string]string{},
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	// Create GitHub user "bob" with enriched profile
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "bob",
		CanonicalID:    "github:bob",
		Provider:       "github",
		DisplayName:    "Bob Jones",
		Email:          "bob@example.com",
		AvatarURL:      "https://example.com/bob.png",
		Hives:          map[string]string{},
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	// Create OIDC user "google:1078" with enriched profile
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "google:1078",
		CanonicalID:    "google:1078",
		Provider:       "google",
		DisplayName:    "Carol Danvers",
		Email:          "carol@google.com",
		AvatarURL:      "https://lh3.google.com/carol",
		Hives:          map[string]string{},
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	// 1. Unauthenticated request -> 401
	{
		req := httptest.NewRequest(http.MethodGet, "/api/saas/whoami", nil)
		rec := httptest.NewRecorder()
		s.handleWhoami(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated /api/saas/whoami code = %d, want 401", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		var errBody map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("json parse error: %v", err)
		}
		if errBody["error"] != "unauthorized" {
			t.Errorf("error msg = %q, want unauthorized", errBody["error"])
		}
	}

	// 2. Forged cookie -> 401
	{
		req := httptest.NewRequest(http.MethodGet, "/api/saas/whoami", nil)
		req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "forged.value"})
		rec := httptest.NewRecorder()
		s.handleWhoami(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("forged cookie /api/saas/whoami code = %d, want 401", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	}

	// 3. GitHub user alice (default derived avatar, empty display_name and email) -> 200
	{
		recMint := httptest.NewRecorder()
		s.mintSessionCookies(recMint, "github:alice")
		var cookieVal string
		for _, c := range recMint.Result().Cookies() {
			if c.Name == "hive_hub_user" {
				cookieVal = c.Value
			}
		}

		req := httptest.NewRequest(http.MethodGet, "/api/saas/whoami", nil)
		req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: cookieVal})
		rec := httptest.NewRecorder()
		s.handleWhoami(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("alice /api/saas/whoami code = %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var resp whoamiResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json parse error: %v", err)
		}
		if resp.Username != "alice" {
			t.Errorf("alice username = %q, want alice", resp.Username)
		}
		if resp.AvatarURL != "https://github.com/alice.png" {
			t.Errorf("alice avatar_url = %q, want https://github.com/alice.png", resp.AvatarURL)
		}
		if resp.DisplayName != "" {
			t.Errorf("alice display_name = %q, want empty", resp.DisplayName)
		}
		if resp.Email != "" {
			t.Errorf("alice email = %q, want empty", resp.Email)
		}
	}

	// 4. GitHub user bob (enriched fields) -> 200
	{
		recMint := httptest.NewRecorder()
		s.mintSessionCookies(recMint, "github:bob")
		var cookieVal string
		for _, c := range recMint.Result().Cookies() {
			if c.Name == "hive_hub_user" {
				cookieVal = c.Value
			}
		}

		req := httptest.NewRequest(http.MethodGet, "/api/saas/whoami", nil)
		req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: cookieVal})
		rec := httptest.NewRecorder()
		s.handleWhoami(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("bob /api/saas/whoami code = %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var resp whoamiResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json parse error: %v", err)
		}
		if resp.Username != "bob" {
			t.Errorf("bob username = %q, want bob", resp.Username)
		}
		if resp.DisplayName != "Bob Jones" {
			t.Errorf("bob display_name = %q, want 'Bob Jones'", resp.DisplayName)
		}
		if resp.Email != "bob@example.com" {
			t.Errorf("bob email = %q, want 'bob@example.com'", resp.Email)
		}
		if resp.AvatarURL != "https://example.com/bob.png" {
			t.Errorf("bob avatar_url = %q, want 'https://example.com/bob.png'", resp.AvatarURL)
		}
	}

	// 5. OIDC user Carol (canonical provider:sub key) -> 200
	{
		recMint := httptest.NewRecorder()
		s.mintSessionCookies(recMint, "google:1078")
		var cookieVal string
		for _, c := range recMint.Result().Cookies() {
			if c.Name == "hive_hub_user" {
				cookieVal = c.Value
			}
		}

		req := httptest.NewRequest(http.MethodGet, "/api/saas/whoami", nil)
		req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: cookieVal})
		rec := httptest.NewRecorder()
		s.handleWhoami(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("carol /api/saas/whoami code = %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var resp whoamiResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json parse error: %v", err)
		}
		if resp.Username != "google:1078" {
			t.Errorf("carol username = %q, want 'google:1078'", resp.Username)
		}
		if resp.DisplayName != "Carol Danvers" {
			t.Errorf("carol display_name = %q, want 'Carol Danvers'", resp.DisplayName)
		}
		if resp.Email != "carol@google.com" {
			t.Errorf("carol email = %q, want 'carol@google.com'", resp.Email)
		}
		if resp.AvatarURL != "https://lh3.google.com/carol" {
			t.Errorf("carol avatar_url = %q, want 'https://lh3.google.com/carol'", resp.AvatarURL)
		}
	}

	// 6. Non-GET method -> 405
	{
		req := httptest.NewRequest(http.MethodPost, "/api/saas/whoami", nil)
		rec := httptest.NewRecorder()
		s.handleWhoami(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST /api/saas/whoami code = %d, want 405", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	}
}
