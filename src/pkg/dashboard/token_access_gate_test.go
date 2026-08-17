package dashboard

// Security regression tests for #3936: GET /api/token-access must be gated at
// owner role so read-only and read-write authenticated users cannot enumerate the
// full gh-command audit log (which contains full argument lists: --repo, --title,
// --body, etc.).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTokenAccessRequiresOwnerRole is the direct behavioural guard.
func TestTokenAccessRequiresOwnerRole(t *testing.T) {
	s, _ := apiServer(t)

	// Non-owner GET — must be 403.
	rec := doGet(s, "/api/token-access")
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner GET /api/token-access = %d, want 403 — any authenticated "+
			"user can read the full gh-command audit log (sec-check #3936)", rec.Code)
	}

	// Owner GET — must succeed (no file in tests → no-log fallback, still 200).
	rec = doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Errorf("owner GET /api/token-access = %d, want 200", rec.Code)
	}
}

// TestTokenAccessReadWriteIsRejected pins that a contributor (read-write) role
// is NOT sufficient — contributors must not see the token access log.
func TestTokenAccessReadWriteIsRejected(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/token-access", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	// Deliberately no X-Hive-Owner-Role-Verified header.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("read-write GET /api/token-access = %d, want 403 — contributor can "+
			"read full gh-command history (sec-check #3936)", rec.Code)
	}
}

// TestTokenAccessSourceGate is the source-level invariant: handleTokenAccess
// must contain requireOwnerRole so a sync merge cannot silently drop the gate.
// Mirrors the pattern established in f16_owner_gate_test.go.
func TestTokenAccessSourceGate(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "api.go"), "handleTokenAccess")
	if !strings.Contains(body, "requireOwnerRole(w, r)") {
		t.Error("handleTokenAccess in api.go has no requireOwnerRole gate — " +
			"any authenticated user can read the full gh-command audit log (#3936). " +
			"Restore the gate; do not remove this test.")
	}
}
