package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type accessOwnerRoleCase struct {
	name     string
	username string
	role     string
	known    bool
	wantCode int
}

func accessOwnerRoleCases() []accessOwnerRoleCase {
	return []accessOwnerRoleCase{
		{name: "creator", username: "creator", known: true, wantCode: http.StatusOK},
		{name: "granted owner role", username: "granted-owner", role: "owner", known: true, wantCode: http.StatusOK},
		{name: "hub admin", username: hubAdminUsername, known: true, wantCode: http.StatusOK},
		{name: "read-write role", username: "writer", role: "read-write", known: true, wantCode: http.StatusForbidden},
		{name: "read role", username: "reader", role: "read", known: true, wantCode: http.StatusForbidden},
		{name: "unknown user", username: "unknown", wantCode: http.StatusForbidden},
		{name: "empty user", username: "", wantCode: http.StatusForbidden},
	}
}

func seedAccessOwnerRoleCase(t *testing.T, tc accessOwnerRoleCase) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)

	mustSaveAccessOwnerRoleUser(t, "creator", map[string]string{})
	mustSaveAccessOwnerRoleUser(t, hubAdminUsername, map[string]string{})
	mustSaveAccessOwnerRoleUser(t, "target", map[string]string{"h1": "read"})
	if tc.known && tc.username != "creator" && tc.username != hubAdminUsername {
		hives := map[string]string{}
		if tc.role != "" {
			hives["h1"] = tc.role
		}
		mustSaveAccessOwnerRoleUser(t, tc.username, hives)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "h1", Owner: "creator"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	return &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
}

func mustSaveAccessOwnerRoleUser(t *testing.T, username string, hives map[string]string) {
	t.Helper()
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: username, Hives: hives}); err != nil {
		t.Fatalf("save user %q: %v", username, err)
	}
}

func accessOwnerRoleReq(method, target, body, username string) *http.Request {
	if username == "" {
		if body == "" {
			return httptest.NewRequest(method, target, nil)
		}
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	return reqWithUser(method, target, body, username)
}

func TestHandleAccessListAllowsGrantedHiveOwners(t *testing.T) {
	for _, tc := range accessOwnerRoleCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := seedAccessOwnerRoleCase(t, tc)
			rec := httptest.NewRecorder()
			req := setPathValue(accessOwnerRoleReq(http.MethodGet, "/access", "", tc.username), "id", "h1")

			s.handleAccessList(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

func TestHandleGrantableUsersAllowsGrantedHiveOwners(t *testing.T) {
	for _, tc := range accessOwnerRoleCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := seedAccessOwnerRoleCase(t, tc)
			rec := httptest.NewRecorder()
			req := accessOwnerRoleReq(http.MethodGet, "/api/saas/grantable-users", "", tc.username)

			s.handleGrantableUsers(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

func TestHandleGrantableUsersAllowsAdminWithNoHives(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	mustSaveAccessOwnerRoleUser(t, hubAdminUsername, map[string]string{})
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	req := accessOwnerRoleReq(http.MethodGet, "/api/saas/grantable-users", "", hubAdminUsername)

	s.handleGrantableUsers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleAccessAddAllowsGrantedHiveOwners(t *testing.T) {
	for _, tc := range accessOwnerRoleCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := seedAccessOwnerRoleCase(t, tc)
			rec := httptest.NewRecorder()
			req := setPathValue(accessOwnerRoleReq(http.MethodPost, "/access", `{"username":"new-grantee","role":"read"}`, tc.username), "id", "h1")

			s.handleAccessAdd(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				if u := loadSaaSUser("new-grantee"); u == nil || u.Hives["h1"] != "read" {
					t.Fatalf("new-grantee role = %+v, want read on h1", u)
				}
			}
		})
	}
}

func TestHandleAccessRemoveAllowsGrantedHiveOwners(t *testing.T) {
	for _, tc := range accessOwnerRoleCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := seedAccessOwnerRoleCase(t, tc)
			rec := httptest.NewRecorder()
			req := setPathValues(accessOwnerRoleReq(http.MethodDelete, "/access", "", tc.username), map[string]string{"id": "h1", "username": "target"})

			s.handleAccessRemove(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				if u := loadSaaSUser("target"); u == nil || u.Hives["h1"] != "" {
					t.Fatalf("target role = %+v, want removed from h1", u)
				}
			}
		})
	}
}
