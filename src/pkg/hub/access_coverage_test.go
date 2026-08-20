package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================
// saas.go — access-request lifecycle and access management
// ============================================================

func setPathValues(r *http.Request, kv map[string]string) *http.Request {
	for k, v := range kv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestAccessRequestLifecycle(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	// Owner + hive.
	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	mkUser(t, "requester")

	// 1. Request access with a note.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"please let me in"}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request access status = %d body=%s", rec.Code, rec.Body.String())
	}

	// 1b. Duplicate pending request -> 400.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"again"}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("dup request status = %d, want 400", rec.Code)
	}

	// 1c. Missing note -> 400.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/req", `{}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no-note status = %d, want 400", rec.Code)
	}

	// 1d. Missing hive -> 404.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"x"}`, "requester"), "id", "nope")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing-hive request status = %d, want 404", rec.Code)
	}

	// 2. Owner lists pending requests.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/reqs", "", "owner"), "id", "h1")
	s.handleGetRequests(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get requests status = %d", rec.Code)
	}
	var got map[string][]AccessRequest
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got["requests"]) != 1 {
		t.Errorf("expected 1 pending request, got %+v", got)
	}

	// 2b. Non-privileged user cannot list.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/reqs", "", "requester"), "id", "h1")
	s.handleGetRequests(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner get requests status = %d, want 403", rec.Code)
	}

	// 3. Owner approves the request as "read".
	rec = httptest.NewRecorder()
	req = setPathValues(reqWithUser(http.MethodPost, "/approve", `{"role":"read"}`, "owner"), map[string]string{"id": "h1", "username": "requester"})
	s.handleApproveRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
	}
	// Requester should now have read access.
	if u := loadSaaSUser("requester"); u == nil || u.Hives["h1"] != "read" {
		t.Errorf("requester role not set: %+v", u)
	}
}

func TestHandleApproveRequestRoleGuard(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	// read-write approver cannot grant owner.
	saveSaaSUser(&SaaSUser{GitHubUsername: "rw", Hives: map[string]string{"h1": "read-write"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "someone"})
	mkUser(t, "target")

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodPost, "/approve", `{"role":"owner"}`, "rw"), map[string]string{"id": "h1", "username": "target"})
	s.handleApproveRequest(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("rw granting owner status = %d, want 403", rec.Code)
	}
}

func TestHandleDenyRequest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	saveAccessRequests("h1", []AccessRequest{{Username: "req1", Status: "pending", Note: "n"}})

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodPost, "/deny", "", "owner"), map[string]string{"id": "h1", "username": "req1"})
	s.handleDenyRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAccessAddListRemove(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	// Add grantee.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/access", `{"username":"grantee","role":"read"}`, "owner"), "id", "h1")
	s.handleAccessAdd(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("access add status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Bad role.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/access", `{"username":"x","role":"superuser"}`, "owner"), "id", "h1")
	s.handleAccessAdd(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad-role add status = %d, want 400", rec.Code)
	}

	// List access.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/access", "", "owner"), "id", "h1")
	s.handleAccessList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("access list status = %d", rec.Code)
	}
	var listed map[string][]map[string]string
	json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed["access"]) < 2 { // owner + grantee
		t.Errorf("expected owner+grantee in access list, got %+v", listed)
	}

	// Remove grantee.
	rec = httptest.NewRecorder()
	req = setPathValues(reqWithUser(http.MethodDelete, "/access", "", "owner"), map[string]string{"id": "h1", "username": "grantee"})
	s.handleAccessRemove(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("access remove status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Cannot remove the last owner.
	rec = httptest.NewRecorder()
	req = setPathValues(reqWithUser(http.MethodDelete, "/access", "", "owner"), map[string]string{"id": "h1", "username": "owner"})
	s.handleAccessRemove(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("remove-last-owner status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least one owner is required") {
		t.Errorf("remove-last-owner body = %q, want clear last-owner error", rec.Body.String())
	}
}

func TestHandleToggleAutoUpgradeSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice"})

	s := &HubServer{
		hubSecret:        testHubSecret,
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPut, "/au", `{"auto_upgrade":true}`, "alice"), "id", "h1")
	s.handleToggleAutoUpgrade(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle auto-upgrade status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h1"); got == nil || !got.AutoUpgrade {
		t.Errorf("auto_upgrade not persisted: %+v", got)
	}

	// OPTIONS preflight.
	rec = httptest.NewRecorder()
	opt := reqWithUser(http.MethodOptions, "/au", "", "alice")
	opt.Header.Set("Origin", "https://hive.kubestellar.io")
	s.handleToggleAutoUpgrade(rec, opt)
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", rec.Code)
	}
}
