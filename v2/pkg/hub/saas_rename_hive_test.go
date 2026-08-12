package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleRenameHive covers the editable-hive-name endpoint
// (PUT /api/saas/hives/{id}/name): owner-or-admin gate, persistence of the
// renamed ProjectName, a blank value clearing the custom name, and the
// validation/CORS edges — mirroring TestHandleToggleVisibility's shape.
func TestHandleRenameHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "alice")
	mkUser(t, hubAdminUsername)

	// OPTIONS preflight -> 204, no auth needed.
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodOptions, "/name", "", "alice")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", rec.Code)
	}

	// Hive not found -> 404.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"X"}`, "alice"), "id", "missing")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing hive status = %d, want 404", rec.Code)
	}

	// Non-owner, non-admin -> 403 (the security boundary), and the hive is
	// NOT renamed on disk.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "bob", ProjectName: "original"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"hijacked"}`, "alice"), "id", "h1")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", rec.Code)
	}
	if got := loadSaaSHive("h1"); got == nil || got.ProjectName != "original" {
		t.Errorf("non-owner request must not persist a rename; got %+v", got)
	}

	// Owner renames -> 200 and the new name persists.
	saveSaaSHive(&SaaSHive{ID: "h2", Owner: "alice", ProjectName: "old-name"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"  Fancy Name  "}`, "alice"), "id", "h2")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner rename status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h2"); got == nil || got.ProjectName != "Fancy Name" {
		t.Errorf("rename not persisted (trimmed); got %+v", got)
	}

	// Admin can rename any hive.
	saveSaaSHive(&SaaSHive{ID: "h3", Owner: "bob", ProjectName: "x"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"admin-set"}`, hubAdminUsername), "id", "h3")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("admin rename status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h3"); got == nil || got.ProjectName != "admin-set" {
		t.Errorf("admin rename not persisted; got %+v", got)
	}

	// Blank clears the custom name (falls back to the org/repo label).
	saveSaaSHive(&SaaSHive{ID: "h4", Owner: "alice", ProjectName: "to-be-cleared"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"   "}`, "alice"), "id", "h4")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("blank rename status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h4"); got == nil || got.ProjectName != "" {
		t.Errorf("blank must clear ProjectName; got %+v", got)
	}

	// Too long -> 400.
	saveSaaSHive(&SaaSHive{ID: "h5", Owner: "alice"})
	long := `{"project_name":"` + strings.Repeat("a", maxHiveDisplayNameLen+1) + `"}`
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", long, "alice"), "id", "h5")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("too-long status = %d, want 400", rec.Code)
	}

	// Bad JSON body from owner -> 400.
	saveSaaSHive(&SaaSHive{ID: "h6", Owner: "alice"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{bad`, "alice"), "id", "h6")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}

	// Assignment in flight (Status=assigned && !ClaimDelivered) -> 409, and the
	// hive is NOT renamed on disk. Renaming during this window forks the hive's
	// identity (pod heartbeats under the baked slot HIVE_ID while the renamed
	// record is orphaned) -> offline + duplicate row.
	saveSaaSHive(&SaaSHive{ID: "h7", Owner: "alice", ProjectName: "in-flight", Status: statusAssigned, ClaimDelivered: false})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"too-soon"}`, "alice"), "id", "h7")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("mid-assignment rename status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h7"); got == nil || got.ProjectName != "in-flight" {
		t.Errorf("mid-assignment rename must not persist; got %+v", got)
	}

	// A fully-assigned, claim-delivered hive is a LIVE hive: rename still
	// succeeds (the in-flight guard applies only to the unclaimed window).
	saveSaaSHive(&SaaSHive{ID: "h8", Owner: "alice", ProjectName: "live-old", Status: statusAssigned, ClaimDelivered: true})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/name", `{"project_name":"live-new"}`, "alice"), "id", "h8")
	s.handleRenameHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim-delivered rename status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h8"); got == nil || got.ProjectName != "live-new" {
		t.Errorf("claim-delivered rename not persisted; got %+v", got)
	}
}
