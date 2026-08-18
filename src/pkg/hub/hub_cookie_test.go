package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSignedHubCookieRoundTrip confirms a value minted by the hub verifies back
// to the same username under the same secret.
func TestSignedHubCookieRoundTrip(t *testing.T) {
	const user = "octocat"
	value := mintHubUserCookieValue(testHubSecret, user)
	if value == "" {
		t.Fatal("mint returned empty value")
	}
	if value == user {
		t.Fatal("cookie value must not be the raw username")
	}
	got, ok := verifyHubUserCookieValue(testHubSecret, value)
	if !ok || got != user {
		t.Fatalf("verify = (%q, %v), want (%q, true)", got, ok, user)
	}
}

// TestForgedHubCookieRejected pins the core F4 guarantee: a hand-crafted
// (unsigned) cookie value or one signed with the wrong secret does NOT
// authenticate. getAuthUser must return "" so the request is unauthenticated.
func TestForgedHubCookieRejected(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)

	srv := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	cases := []struct {
		name  string
		value string
	}{
		{"raw unsigned username (legacy/forged)", hubAdminUsername},
		{"empty signature", hubAdminUsername + "."},
		{"garbage signature", hubAdminUsername + ".not-a-real-hmac"},
		{"signed with wrong secret", mintHubUserCookieValue("attacker-secret", hubAdminUsername)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/x", nil)
			req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: tc.value})
			if got := srv.getAuthUser(req); got != "" {
				t.Errorf("forged cookie authenticated as %q, want unauthenticated", got)
			}
		})
	}
}

// TestSignedHubCookieAuthenticates confirms a properly-signed cookie for a
// known user does authenticate through getAuthUser.
func TestSignedHubCookieAuthenticates(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")

	srv := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(testAuthCookie("alice"))
	if got := srv.getAuthUser(req); got != "alice" {
		t.Errorf("getAuthUser = %q, want alice", got)
	}
}

// TestHandleHubVersionNeverLeaksSecret confirms the version endpoint never
// returns the hub secret, even to a request carrying a valid admin cookie.
func TestHandleHubVersionNeverLeaksSecret(t *testing.T) {
	srv := &HubServer{logger: slog.Default(), hubSecret: "super-secret-value"}

	req := httptest.NewRequest("GET", "/api/hub/version", nil)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	w := httptest.NewRecorder()
	srv.handleHubVersion(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := resp["hub_secret"]; present {
		t.Error("version response must never include hub_secret")
	}
	if got := w.Body.String(); strings.Contains(got, "super-secret-value") {
		t.Errorf("version body leaked the raw secret: %s", got)
	}
}
