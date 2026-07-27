package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// saas.go — a few remaining cheap handler branches
// ============================================================

func TestHandleHiveStatusBranches(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}

	// Missing hive -> 404.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodGet, "/st", "", "alice"), "id", "nope")
	s.handleHiveStatus(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing-hive status = %d, want 404", rec.Code)
	}

	// Owner -> 200.
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/st", "", "alice"), "id", "h1")
	s.handleHiveStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("owner hive-status = %d, want 200", rec.Code)
	}

	// Non-owner without access -> 403.
	mkUser(t, "stranger")
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/st", "", "stranger"), "id", "h1")
	s.handleHiveStatus(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("stranger hive-status = %d, want 403", rec.Code)
	}
}

func TestHandleAccessAddBadBody_Cov6(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}
	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	// Malformed body -> 400.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/access", `{bad`, "owner"), "id", "h1")
	s.handleAccessAdd(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad-body access-add status = %d, want 400", rec.Code)
	}

	// Missing hive -> 404.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/access", `{"username":"x","role":"read"}`, "owner"), "id", "nope")
	s.handleAccessAdd(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing-hive access-add status = %d, want 404", rec.Code)
	}
}

func TestHandleRequestAccessAlreadyHas(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}

	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	// User already has access to h1.
	saveSaaSUser(&SaaSUser{GitHubUsername: "member", Hives: map[string]string{"h1": "read"}})

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"let me in"}`, "member"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("already-has-access request status = %d, want 400", rec.Code)
	}
}

func TestHandleAccessRemoveUserNotFound(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}
	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodDelete, "/access", "", "owner"), map[string]string{"id": "h1", "username": "ghost"})
	s.handleAccessRemove(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("remove-missing-user status = %d, want 404", rec.Code)
	}
}
