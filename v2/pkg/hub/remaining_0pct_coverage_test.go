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

func TestVerifyHubUserCookieEither_FallsBackToLegacy(t *testing.T) {
	legacyCookie := mintHubUserCookieValue(testHubSecret, "bob")
	if legacyCookie == "" {
		t.Fatal("mintHubUserCookieValue returned empty")
	}
	// Pass an empty pubHex so V2 verification fails; should fall back to legacy.
	user, ok := verifyHubUserCookieEither("", testHubSecret, legacyCookie)
	if !ok || user != "bob" {
		t.Errorf("legacy fallback: got (%q, %v), want (bob, true)", user, ok)
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
