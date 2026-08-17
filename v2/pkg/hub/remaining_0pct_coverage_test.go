package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// hub_cookie.go — verifyHubUserCookieEither: dual-lane cookie verification
// ============================================================

func TestVerifyHubUserCookieEither_V2Preferred(t *testing.T) {
	// Generate an Ed25519 seed and corresponding pub key.
	s := &HubServer{hubSecret: testHubSecret}
	seedHex := s.sessionSigningSeed()
	pubHex := s.sessionPublicKey()

	cookie := mintHubUserCookieValueV2(seedHex, "alice")
	if cookie == "" {
		t.Fatal("mintHubUserCookieValueV2 returned empty")
	}
	user, ok := verifyHubUserCookieEither(pubHex, testHubSecret, cookie)
	if !ok || user != "alice" {
		t.Errorf("V2 cookie: got (%q, %v), want (alice, true)", user, ok)
	}
}

// Adapted from the original "FallsBackToLegacy" case: the symmetric legacy
// lane was deleted (AUDIT F1 — any spoke operator holding the shared
// HIVE_SESSION_KEY could forge it), so a well-formed legacy HMAC cookie must
// now be REJECTED even when the correct legacy secret is supplied. This is a
// regression test that the lane stays dead.
func TestVerifyHubUserCookieEither_LegacyLaneRemoved(t *testing.T) {
	legacyCookie := mintHubUserCookieValue(testHubSecret, "bob")
	if legacyCookie == "" {
		t.Fatal("mintHubUserCookieValue returned empty")
	}
	if user, ok := verifyHubUserCookieEither("", testHubSecret, legacyCookie); ok {
		t.Errorf("legacy HMAC cookie was accepted as %q; the F1 legacy lane must remain removed", user)
	}
}

func TestVerifyHubUserCookieEither_BothFail(t *testing.T) {
	user, ok := verifyHubUserCookieEither("", "", "garbage-cookie-value")
	if ok {
		t.Errorf("expected failure, got user=%q", user)
	}
}

// ============================================================
// auth_rollout.go — handleAuthRollout: JSON readiness endpoint
// ============================================================

func TestHandleAuthRollout(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/rollout", nil)
	s.handleAuthRollout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

// ============================================================
// url_reachability.go — urlPrivateNetworkReason
// ============================================================

func TestURLPrivateNetworkReason_NoDashboardURL(t *testing.T) {
	h := RegistryEntry{}
	got := urlPrivateNetworkReason(h, 0)
	if got == "" {
		t.Error("expected non-empty reason")
	}
}

func TestURLPrivateNetworkReason_WithDashboardURL(t *testing.T) {
	h := RegistryEntry{DashboardURL: "https://spoke.example.com"}
	got := urlPrivateNetworkReason(h, 503)
	if got == "" {
		t.Error("expected non-empty reason")
	}
}

func TestURLPrivateNetworkReason_SelfCheckOK(t *testing.T) {
	h := RegistryEntry{
		DashboardURL: "https://spoke.example.com",
		PublicURLSelfCheck: &PublicURLSelfCheck{
			Status: PublicURLSelfCheckOK,
		},
	}
	got := urlPrivateNetworkReason(h, 0)
	if got == "" {
		t.Error("expected non-empty reason")
	}
}

// ============================================================
// url_reachability.go — publicURLSelfCheckOK
// ============================================================

func TestPublicURLSelfCheckOK_Nil(t *testing.T) {
	if publicURLSelfCheckOK(nil) {
		t.Error("nil check should return false")
	}
}

func TestPublicURLSelfCheckOK_OK(t *testing.T) {
	if !publicURLSelfCheckOK(&PublicURLSelfCheck{Status: PublicURLSelfCheckOK}) {
		t.Error("OK check should return true")
	}
}

func TestPublicURLSelfCheckOK_Fail(t *testing.T) {
	if publicURLSelfCheckOK(&PublicURLSelfCheck{Status: PublicURLSelfCheckFail}) {
		t.Error("Fail check should return false")
	}
}

// ============================================================
// url_reachability.go — heartbeatFreshForURLCheck
// ============================================================

func TestHeartbeatFreshForURLCheck_Online(t *testing.T) {
	h := RegistryEntry{Online: true}
	if !heartbeatFreshForURLCheck(h, time.Now()) {
		t.Error("online hive should be fresh")
	}
}

func TestHeartbeatFreshForURLCheck_RecentHeartbeat(t *testing.T) {
	h := RegistryEntry{
		LastHeartbeat: time.Now().Add(-30 * time.Second).Format(time.RFC3339),
	}
	if !heartbeatFreshForURLCheck(h, time.Now()) {
		t.Error("recent heartbeat should be fresh")
	}
}

func TestHeartbeatFreshForURLCheck_StaleHeartbeat(t *testing.T) {
	h := RegistryEntry{
		LastHeartbeat: time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
	}
	if heartbeatFreshForURLCheck(h, time.Now()) {
		t.Error("24h-old heartbeat should be stale")
	}
}

func TestHeartbeatFreshForURLCheck_NoHeartbeat(t *testing.T) {
	h := RegistryEntry{}
	if heartbeatFreshForURLCheck(h, time.Now()) {
		t.Error("no heartbeat should not be fresh")
	}
}

func TestHeartbeatFreshForURLCheck_BadTimestamp(t *testing.T) {
	h := RegistryEntry{LastHeartbeat: "not-a-timestamp"}
	if heartbeatFreshForURLCheck(h, time.Now()) {
		t.Error("bad timestamp should not be fresh")
	}
}

// ============================================================
// hub_cookie.go — hubCookieSessionID / hubCookieExpiry
// ============================================================

func TestHubCookieSessionID_ValidV3(t *testing.T) {
	s := &HubServer{hubSecret: testHubSecret}
	seedHex := s.sessionSigningSeed()
	now := time.Now()
	cookie, sid := mintHubUserCookieValueV3(seedHex, "alice", now, 24*time.Hour)
	if cookie == "" || sid == "" {
		t.Fatal("mintHubUserCookieValueV3 returned empty")
	}
	got := hubCookieSessionID(cookie)
	if got != sid {
		t.Errorf("hubCookieSessionID = %q, want %q", got, sid)
	}
}

func TestHubCookieSessionID_NonV3ReturnsEmpty(t *testing.T) {
	// V2 cookie has no session ID.
	cookie := mintHubUserCookieValue(testHubSecret, "bob")
	got := hubCookieSessionID(cookie)
	if got != "" {
		t.Errorf("hubCookieSessionID(v2) = %q, want empty", got)
	}
}

func TestHubCookieSessionID_GarbageReturnsEmpty(t *testing.T) {
	if got := hubCookieSessionID("not-a-cookie"); got != "" {
		t.Errorf("hubCookieSessionID(garbage) = %q, want empty", got)
	}
}

func TestHubCookieExpiry_ValidV3(t *testing.T) {
	s := &HubServer{hubSecret: testHubSecret}
	seedHex := s.sessionSigningSeed()
	now := time.Now()
	ttl := 24 * time.Hour
	cookie, _ := mintHubUserCookieValueV3(seedHex, "alice", now, ttl)
	if cookie == "" {
		t.Fatal("mintHubUserCookieValueV3 returned empty")
	}
	got := hubCookieExpiry(cookie)
	want := now.Add(ttl).Unix()
	if got != want {
		t.Errorf("hubCookieExpiry = %d, want %d", got, want)
	}
}

func TestHubCookieExpiry_NonV3ReturnsZero(t *testing.T) {
	cookie := mintHubUserCookieValue(testHubSecret, "bob")
	if got := hubCookieExpiry(cookie); got != 0 {
		t.Errorf("hubCookieExpiry(v2) = %d, want 0", got)
	}
}

// ============================================================
// oauth.go — displayIdentity
// ============================================================

func TestDisplayIdentity_GitHubLogin(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	login, avatar := s.displayIdentity("alice")
	if login != "alice" {
		t.Errorf("login = %q, want alice", login)
	}
	if avatar != "https://github.com/alice.png" {
		t.Errorf("avatar = %q, want github avatar URL", avatar)
	}
}

func TestDisplayIdentity_CanonicalGitHub(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	login, avatar := s.displayIdentity("github:bob")
	if login != "bob" {
		t.Errorf("login = %q, want bob", login)
	}
	if avatar != "https://github.com/bob.png" {
		t.Errorf("avatar = %q, want github avatar URL", avatar)
	}
}

func TestDisplayIdentity_NonGitHubFallsBack(t *testing.T) {
	// A non-GitHub identity with no SaaS user record falls back to the raw identity.
	s := &HubServer{logger: slog.Default()}
	login, avatar := s.displayIdentity("ibmid:user@example.com")
	// Without a stored SaaS user, login is the raw identity string.
	if login == "" {
		t.Error("login should not be empty")
	}
	// Avatar may be empty when no SaaS user record exists.
	_ = avatar
}
