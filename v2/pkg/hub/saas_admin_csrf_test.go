package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// N6 (CWE-352): requireAdmin must run the SAME CSRF check requireAuth does.
//
// Before this fix requireAdmin resolved identity and nothing else, so every
// admin mutation (~21 routes: hub upgrade, cluster App key writes, user
// delete, banners, provisioning approval) was reachable by a cross-site form
// POST carrying the admin's ambient session cookie — from ANY origin, not just
// a sibling *.hive.kubestellar.io tenant.
//
// These tests present a GENUINELY VALID admin cookie, so a 403 can only come
// from the CSRF gate. That distinction matters: the pre-existing
// TestRequireAdminNonAdmin / TestRequireAdminNoAuth also assert 403, but they
// get it from the identity check and would keep passing with the CSRF hole
// wide open. The suite had a requireAuth CSRF test (TestRequireAuthCSRFFails)
// and no requireAdmin counterpart, which is why this shipped.

// adminCSRFHandler builds a requireAdmin-wrapped handler that records whether
// the protected body ever ran. Reaching the body is the failure condition.
func adminCSRFHandler(s *HubServer, reached *bool) http.HandlerFunc {
	return s.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// TestRequireAdminCSRFCrossOriginPostDenied is the regression for N6: a POST
// from an unrelated origin, with a valid admin session cookie, must be refused
// before the handler runs.
func TestRequireAdminCSRFCrossOriginPostDenied(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)

	reached := false
	handler := adminCSRFHandler(s, &reached)

	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/hub-banner", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.AddCookie(testAuthCookie(hubAdminUsername))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin admin POST: code=%d, want 403 (CSRF); body=%q", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatal("cross-origin admin POST reached the protected handler — CSRF gate did not run")
	}
}

// TestRequireAdminCSRFSiblingTenantOriginDenied covers the lane the audit
// reproduced: a hostile tenant spoke. isTrustedOrigin still allows
// *.hive.kubestellar.io, so this currently passes the CSRF check and is denied
// only if the wider trusted-origin reduction lands. Asserting the *documented*
// current behaviour keeps this test honest rather than aspirational — flip it
// to expect 403 when the exact-origin change ships.
func TestRequireAdminCSRFSiblingTenantOriginStillTrusted(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/hub-banner", nil)
	req.Header.Set("Origin", "https://attacker-hive.hive.kubestellar.io")

	if !isCSRFSafe(req) {
		t.Skip("sibling-tenant origin is no longer trusted — the exact-origin fix has landed; update this test to assert 403 through requireAdmin")
	}
	t.Log("KNOWN GAP (N6, second half): a sibling *.hive.kubestellar.io origin is still CSRF-trusted; " +
		"the requireAdmin gate does not close this lane on its own. Tracked for the exact-origin + CSRF-token work.")
}

// TestRequireAdminSafeGetStillAllowed guards against over-correction: the CSRF
// check must not break admin GETs, which is how the dashboard reads admin data.
func TestRequireAdminSafeGetStillAllowed(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)

	reached := false
	handler := adminCSRFHandler(s, &reached)

	req := httptest.NewRequest(http.MethodGet, "/api/saas/admin/users", nil)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("admin GET with valid session: code=%d reached=%v, want 200/true", rec.Code, reached)
	}
}

// TestRequireAdminSameOriginPostAllowed proves the legitimate dashboard path
// (a same-origin POST from the hub itself) is unaffected by the new gate.
func TestRequireAdminSameOriginPostAllowed(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)

	reached := false
	handler := adminCSRFHandler(s, &reached)

	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/hub-banner", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.AddCookie(testAuthCookie(hubAdminUsername))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("same-origin admin POST: code=%d reached=%v, want 200/true", rec.Code, reached)
	}
}
