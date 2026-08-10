package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthRolloutEndpointE2E exercises the real admin route: the authorization
// gate and the JSON shape an operator will actually read.
//
// Unit-testing AuthRolloutReadiness alone would not catch a route registered
// without requireAdmin, or a response body that does not marshal — and this
// endpoint enumerates hive IDs, so the auth gate is part of the contract.
func TestAuthRolloutEndpointE2E(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	s.authRolloutSeen = make(map[string]authRolloutEntry)
	mkUser(t, hubAdminUsername)

	s.noteHeartbeatAuthPath("hive-rolled", true)
	s.noteHeartbeatAuthPath("hive-laggard", false)

	h := s.requireAdmin(s.handleAuthRollout)

	// Non-admin must not read fleet-shaped data.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/saas/admin/auth-rollout", nil)
	req.AddCookie(testAuthCookie("someuser"))
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin got %d, want 403", rec.Code)
	}

	// Admin gets the summary.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/saas/admin/auth-rollout", nil)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got AuthRolloutStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if got.TotalHives != 2 || got.LegacyBearer != 1 || got.HeartbeatLaneReady {
		t.Fatalf("unexpected summary: %+v", got)
	}
	if len(got.LegacyHives) != 1 || got.LegacyHives[0] != "hive-laggard" {
		t.Fatalf("laggard not named: %+v", got.LegacyHives)
	}
	t.Logf("endpoint OK: %s", rec.Body.String())
}
