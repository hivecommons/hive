package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// --- displayIdentity tests ---

func TestDisplayIdentityGitHubBareLogin(t *testing.T) {
	s := &HubServer{}
	login, avatar := s.displayIdentity("alice")
	if login != "alice" {
		t.Fatalf("login = %q, want alice", login)
	}
	if avatar != "https://github.com/alice.png" {
		t.Fatalf("avatar = %q, want github avatar URL", avatar)
	}
}

func TestDisplayIdentityGitHubCanonical(t *testing.T) {
	s := &HubServer{}
	login, avatar := s.displayIdentity("github:bob")
	if login != "bob" {
		t.Fatalf("login = %q, want bob", login)
	}
	if avatar != "https://github.com/bob.png" {
		t.Fatalf("avatar = %q, want github avatar URL", avatar)
	}
}

func TestDisplayIdentityOIDCWithStoredUser(t *testing.T) {
	dir := t.TempDir()
	orig := saasUsersDir
	saasUsersDir = dir
	t.Cleanup(func() { saasUsersDir = orig })

	// Write a user file for google:12345
	u := SaaSUser{
		CanonicalID: "google:12345",
		Email:       "user@example.com",
		AvatarURL:   "https://lh3.google.com/photo.jpg",
	}
	data, _ := json.Marshal(u)
	os.WriteFile(filepath.Join(dir, "google.12345.json"), data, 0o644)

	s := &HubServer{}
	login, avatar := s.displayIdentity("google:12345")
	if login != "user@example.com" {
		t.Fatalf("login = %q, want user@example.com", login)
	}
	if avatar != "https://lh3.google.com/photo.jpg" {
		t.Fatalf("avatar = %q, want stored avatar", avatar)
	}
}

func TestDisplayIdentityOIDCNoStoredUser(t *testing.T) {
	dir := t.TempDir()
	orig := saasUsersDir
	saasUsersDir = dir
	t.Cleanup(func() { saasUsersDir = orig })

	s := &HubServer{}
	login, avatar := s.displayIdentity("google:99999")
	// Falls back to the raw identity string
	if login != "google:99999" {
		t.Fatalf("login = %q, want google:99999", login)
	}
	if avatar != "" {
		t.Fatalf("avatar = %q, want empty", avatar)
	}
}

// --- OIDC nonce cookie helpers ---

func TestOIDCNonceFromCookiePresent(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: oidcNonceCookieName, Value: "test-nonce-abc"})

	got := s.oidcNonceFromCookie(req)
	if got != "test-nonce-abc" {
		t.Fatalf("nonce = %q, want test-nonce-abc", got)
	}
}

func TestOIDCNonceFromCookieMissing(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	got := s.oidcNonceFromCookie(req)
	if got != "" {
		t.Fatalf("nonce = %q, want empty", got)
	}
}

func TestOIDCNonceFromCookieEmpty(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: oidcNonceCookieName, Value: ""})

	got := s.oidcNonceFromCookie(req)
	if got != "" {
		t.Fatalf("nonce = %q, want empty", got)
	}
}

func TestClearOIDCNonceCookieExpires(t *testing.T) {
	s := &HubServer{}
	rec := httptest.NewRecorder()

	s.clearOIDCNonceCookie(rec)

	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == oidcNonceCookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("expected oidc nonce cookie to be set")
	}
	if found.MaxAge != -1 {
		t.Fatalf("MaxAge = %d, want -1 (expire)", found.MaxAge)
	}
	if found.Value != "" {
		t.Fatalf("Value = %q, want empty", found.Value)
	}
}
