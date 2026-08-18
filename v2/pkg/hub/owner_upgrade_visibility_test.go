package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests pin the fix for #4081: a hive's TRUE owner whose stored
// user.Hives role was demoted (e.g. an access approval overwrote the owner's
// grant with "read") must still be served role "owner" everywhere the admin
// is, so the Upgrade affordance is visible in the hub AND the spoke sees
// X-Hive-Role: owner. Without the fix only the hub admin was normalized.

// TestMyHivesNormalizesTrueOwnerRole: the registry owner with a stale "read"
// entry in user.Hives gets role "owner" in the my-hives payload (the gate the
// dashboard's Upgrade link renders on), and the stored role is repaired.
func TestMyHivesNormalizesTrueOwnerRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const username = "true-owner"
	u := ensureSaaSUser(username)
	u.Hives["hosted-ownerviz"] = "read" // stale demotion
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "hosted-ownerviz", Owner: "true-owner", Online: true, Name: "org/repo", HiveType: "hosted", GitHash: "abc1234", LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/my-hives", nil)
	req.AddCookie(testAuthCookie(username))
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Hives []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"hives"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, h := range resp.Hives {
		if h.ID == "hosted-ownerviz" {
			found = true
			if h.Role != "owner" {
				t.Errorf("true owner served role %q, want owner", h.Role)
			}
		}
	}
	if !found {
		t.Fatal("owned hive missing from my-hives payload")
	}

	// The repair must persist so the spoke auth proxy reads "owner" too.
	if got := loadSaaSUser(username).Hives["hosted-ownerviz"]; got != "owner" {
		t.Errorf("stored role = %q, want owner (repair must persist)", got)
	}
}

// TestMyHivesKeepsGrantedNonOwnerRole: a user who is NOT the hive owner keeps
// their granted role — the normalization is scoped to the true owner (+admin).
func TestMyHivesKeepsGrantedNonOwnerRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const username = "just-a-reader"
	u := ensureSaaSUser(username)
	u.Hives["hosted-ownerviz2"] = "read"
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "hosted-ownerviz2", Owner: "someone-else", Online: true, Name: "org/repo", HiveType: "hosted", LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/my-hives", nil)
	req.AddCookie(testAuthCookie(username))
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Hives []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"hives"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, h := range resp.Hives {
		if h.ID == "hosted-ownerviz2" && h.Role != "read" {
			t.Errorf("granted reader served role %q, want read", h.Role)
		}
	}
}

// TestAuthCheckElevatesTrueOwnerRole: the spoke auth proxy must forward
// X-Hive-Role: owner for the hive's true owner even when the stored role was
// demoted — the spoke's requireOwnerRole gates /api/self-upgrade on it.
func TestAuthCheckElevatesTrueOwnerRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const username = "spoke-owner"
	u := ensureSaaSUser(username)
	u.Hives["hosted-spoke-own"] = "read" // stale demotion
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "hosted-spoke-own", Owner: username, ClusterID: "hive-oke"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=hosted-spoke-own", nil)
	req.AddCookie(testAuthCookie(username))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Hive-Role"); got != "owner" {
		t.Errorf("X-Hive-Role = %q, want owner", got)
	}
}

// TestAuthCheckStillRejectsStranger: the owner elevation must not open the
// proxy to users with no grant and no ownership.
func TestAuthCheckStillRejectsStranger(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const username = "stranger"
	ensureSaaSUser(username)
	saveSaaSHive(&SaaSHive{ID: "hosted-not-yours", Owner: "someone-else", ClusterID: "hive-oke"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=hosted-not-yours", nil)
	req.AddCookie(testAuthCookie(username))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("stranger status = %d, want 403", w.Code)
	}
}

// TestAuthCheckKeepsGrantedNonOwnerRole: a granted reader on someone else's
// hive still reaches the spoke with their real (non-owner) role.
func TestAuthCheckKeepsGrantedNonOwnerRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const username = "reader-two"
	u := ensureSaaSUser(username)
	u.Hives["hosted-read-only"] = "read"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "hosted-read-only", Owner: "someone-else", ClusterID: "hive-oke"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=hosted-read-only", nil)
	req.AddCookie(testAuthCookie(username))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Hive-Role"); got != "read" {
		t.Errorf("X-Hive-Role = %q, want read", got)
	}
}
