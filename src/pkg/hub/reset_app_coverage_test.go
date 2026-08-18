package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleResetAppArmsOnlyExistingHiveForAdmin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "bob")
	if err := saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner", Status: statusAssigned}); err != nil {
		t.Fatalf("save hive: %v", err)
	}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/h1/reset-app", "", "bob"), "id", "h1")
	s.handleResetApp(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin access required") {
		t.Fatalf("non-admin reset code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/missing/reset-app", "", hubAdminUsername), "id", "missing")
	s.handleResetApp(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "hive not found") {
		t.Fatalf("missing hive reset code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/h1/reset-app", "", hubAdminUsername), "id", "h1")
	s.handleResetApp(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "armed") {
		t.Fatalf("admin reset code=%d body=%q", rec.Code, rec.Body.String())
	}
	got := loadSaaSHive("h1")
	if got == nil || !got.RequestedAppReset {
		t.Fatalf("reset did not persist RequestedAppReset: %+v", got)
	}
}
