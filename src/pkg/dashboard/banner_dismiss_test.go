package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dismissLogger() *slog.Logger { return slog.Default() }

// TestHandleBannerDismissedRequiresAuth verifies an unauthenticated request
// (no session cookie, no proxy-injected identity) is rejected: only signed-in
// users may dismiss a banner.
func TestHandleBannerDismissedRequiresAuth(t *testing.T) {
	s := NewServer(0, dismissLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/banner-dismissed", strings.NewReader(`{"id":"b-1"}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleBannerDismissed(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated dismiss status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleBannerDismissedRejectsReadOnly verifies a read-only viewer (role
// "read", the value nginx injects for preview users) cannot dismiss.
func TestHandleBannerDismissedRejectsReadOnly(t *testing.T) {
	s := NewServer(0, dismissLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/banner-dismissed", strings.NewReader(`{"id":"b-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "viewer")
	req.Header.Set("X-Hive-Role", "read")
	s.handleBannerDismissed(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("read-only dismiss status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleBannerDismissedProxiedWriteUser verifies a hub-proxied write/owner
// user (identity injected via X-Hive-User/X-Hive-Role) is allowed.
func TestHandleBannerDismissedProxiedWriteUser(t *testing.T) {
	s := NewServer(0, dismissLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/banner-dismissed", strings.NewReader(`{"id":"b-2"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "owner-user")
	req.Header.Set("X-Hive-Role", "owner")
	s.handleBannerDismissed(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("owner dismiss status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleBannerDismissedSessionUser verifies a direct-route user with a valid
// per-user session cookie is allowed to dismiss.
func TestHandleBannerDismissedSessionUser(t *testing.T) {
	s := NewServer(0, dismissLogger())
	id := s.createUserSession("alice", "owner")
	if id == "" {
		t.Fatal("could not create session")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/banner-dismissed", strings.NewReader(`{"id":"b-3"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	s.handleBannerDismissed(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("session-user dismiss status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleBannerDismissedBadBody verifies a missing/blank banner id is a 400.
func TestHandleBannerDismissedBadBody(t *testing.T) {
	s := NewServer(0, dismissLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/banner-dismissed", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "owner-user")
	req.Header.Set("X-Hive-Role", "owner")
	s.handleBannerDismissed(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty id status = %d, want 400", rec.Code)
	}
}

// TestSetHubBannerLogsFirstDisplayOnce is a light guard that SetHubBanner is
// idempotent on the stored state for the same id and updates it for a new id
// (the first-display log fires on the transition, which we exercise here for
// coverage of the id-change branch).
func TestSetHubBannerFirstDisplayTransition(t *testing.T) {
	s := NewServer(0, dismissLogger())

	s.SetHubBanner("b-1", "hello", "green")
	s.hubBannerMu.RLock()
	first := s.hubBanner
	s.hubBannerMu.RUnlock()
	if first == nil || first.ID != "b-1" {
		t.Fatalf("banner not set, got %+v", first)
	}

	// Same id again — state stays consistent.
	s.SetHubBanner("b-1", "hello", "green")
	// New id — banner is replaced.
	s.SetHubBanner("b-2", "world", "amber")
	s.hubBannerMu.RLock()
	got := s.hubBanner
	s.hubBannerMu.RUnlock()
	if got == nil || got.ID != "b-2" || got.Message != "world" {
		t.Errorf("banner not updated to new id, got %+v", got)
	}
}
