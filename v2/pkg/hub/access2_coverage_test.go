package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// saas.go — PUT-style access approve/deny + status + assign validation
// ============================================================

func TestHandleApproveAccessFlow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	// No pending request -> 404.
	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodPut, "/aa", "", "owner"), map[string]string{"id": "h1", "username": "req1"})
	s.handleApproveAccess(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-pending approve-access status = %d, want 404", rec.Code)
	}

	// With a pending request -> approved.
	saveAccessRequests("h1", []AccessRequest{{Username: "req1", Status: "pending", Note: "n"}})
	rec = httptest.NewRecorder()
	req = setPathValues(reqWithUser(http.MethodPut, "/aa", "", "owner"), map[string]string{"id": "h1", "username": "req1"})
	s.handleApproveAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve-access status = %d body=%s", rec.Code, rec.Body.String())
	}
	if u := loadSaaSUser("req1"); u == nil || u.Hives["h1"] != "read" {
		t.Errorf("req1 not granted read: %+v", u)
	}

	// Non-owner approver -> 403.
	saveSaaSUser(&SaaSUser{GitHubUsername: "notowner", Hives: map[string]string{}})
	rec = httptest.NewRecorder()
	req = setPathValues(reqWithUser(http.MethodPut, "/aa", "", "notowner"), map[string]string{"id": "h1", "username": "req2"})
	s.handleApproveAccess(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner approve-access status = %d, want 403", rec.Code)
	}
}

func TestHandleDenyAccessFlow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	saveAccessRequests("h1", []AccessRequest{{Username: "req1", Status: "pending", Note: "n"}})

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodPut, "/da", "", "owner"), map[string]string{"id": "h1", "username": "req1"})
	s.handleDenyAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny-access status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAccessStatus(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}

	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	mkUser(t, "viewer")

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodGet, "/status", "", "viewer"), "id", "h1")
	s.handleAccessStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("access-status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Missing hive -> still responds (no access), exercising the not-found branch.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/status", "", "viewer"), "id", "nope")
	s.handleAccessStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("missing-hive access-status = %d, want 200", rec.Code)
	}
}

func TestHandleAssignHiveValidation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newHandlerHub2()
	saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	cases := []struct {
		name, body string
		want       int
	}{
		{"bad json", `{`, http.StatusBadRequest},
		{"invalid owner", `{"owner":"bad owner!","org":"acme","repos":"r"}`, http.StatusBadRequest},
		{"invalid org", `{"owner":"bob","org":"bad org!","repos":"r"}`, http.StatusBadRequest},
		{"no repos", `{"owner":"bob","org":"acme","repos":""}`, http.StatusBadRequest},
		{"invalid repo", `{"owner":"bob","org":"acme","repos":"bad repo!"}`, http.StatusBadRequest},
		{"bad acmm", `{"owner":"bob","org":"acme","repos":"r","acmm_level":99}`, http.StatusBadRequest},
		{"non-numeric app_id", `{"owner":"bob","org":"acme","repos":"r","app_id":"x","installation_id":"1","app_private_key":"-----BEGIN X-----"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset the placeholder for each case (assign mutates it on success).
			saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
			rec := httptest.NewRecorder()
			req := setPathValue(reqWithUser(http.MethodPost, "/assign", tc.body, hubAdminUsername), "id", "ph1")
			s.handleAssignHive(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
