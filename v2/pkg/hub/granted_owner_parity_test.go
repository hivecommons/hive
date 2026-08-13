package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGrantedOwnerParityForHiveManagementHandlers proves that every hive
// management handler resolves ownership through userIsHiveOwner: the registry
// creator and any user GRANTED the owner role pass, while granted read,
// granted read-write, and unrelated users are rejected. The creator case is
// the positive control that the port did not break original owners, and the
// rejected cases are the positive control that the gate still exists at all.
//
// Ported from v2 (#3567) and re-expressed against v4's role gating: the
// authorization change here only EXTENDS who passes the owner checks.
func TestGrantedOwnerParityForHiveManagementHandlers(t *testing.T) {
	type parityCase struct {
		name    string
		method  string
		target  string
		body    string
		prepare func(t *testing.T, s *HubServer, hiveID string)
		handler func(*HubServer, http.ResponseWriter, *http.Request)
	}

	cases := []parityCase{
		{
			name:   "delete hive",
			method: http.MethodDelete,
			target: "/delete",
			prepare: func(t *testing.T, s *HubServer, hiveID string) {
				t.Helper()
				h := loadSaaSHive(hiveID)
				h.ClusterID = "missing-cluster"
				if err := saveSaaSHive(h); err != nil {
					t.Fatalf("save hive: %v", err)
				}
			},
			handler: (*HubServer).handleDeleteHive,
		},
		// "migrate hive" was removed from this table with the migration API
		// itself (v2 #3577 port) — the route no longer exists to gate.
		{
			name:   "upgrade hive",
			method: http.MethodPost,
			target: "/upgrade",
			prepare: func(t *testing.T, s *HubServer, hiveID string) {
				t.Helper()
				h := loadSaaSHive(hiveID)
				h.ClusterID = "missing-cluster"
				if err := saveSaaSHive(h); err != nil {
					t.Fatalf("save hive: %v", err)
				}
			},
			handler: (*HubServer).handleUpgradeHive,
		},
		{
			name:    "switch branch",
			method:  http.MethodPost,
			target:  "/switch",
			body:    `{"branch":""}`,
			handler: (*HubServer).handleSwitchBranch,
		},
		{
			name:    "toggle visibility",
			method:  http.MethodPut,
			target:  "/visibility",
			body:    `{"is_public":true}`,
			handler: (*HubServer).handleToggleVisibility,
		},
		{
			name:    "rename hive",
			method:  http.MethodPut,
			target:  "/rename",
			body:    `{"project_name":"Granted Owner Rename"}`,
			handler: (*HubServer).handleRenameHive,
		},
		{
			name:    "toggle auto-upgrade",
			method:  http.MethodPut,
			target:  "/auto-upgrade",
			body:    `{"auto_upgrade":true,"auto_upgrade_mode":"daily"}`,
			handler: (*HubServer).handleToggleAutoUpgrade,
		},
	}

	actors := []struct {
		name       string
		username   string
		role       string
		wantStatus int
	}{
		{name: "creator is allowed", username: "creator"},
		{name: "granted owner is allowed", username: "granted-owner", role: "owner"},
		{name: "granted read is rejected", username: "reader", role: "read", wantStatus: http.StatusForbidden},
		{name: "granted read-write is rejected", username: "writer", role: "read-write", wantStatus: http.StatusForbidden},
		{name: "unrelated user is rejected", username: "stranger", wantStatus: http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, actor := range actors {
				t.Run(actor.name, func(t *testing.T) {
					cleanup := helperSetupTempDirs(t)
					defer cleanup()

					s := newHandlerHub()
					hiveID := "hosted-parity"
					if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "creator", ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
						t.Fatalf("save hive: %v", err)
					}
					if err := saveSaaSUser(&SaaSUser{GitHubUsername: "creator", Hives: map[string]string{}}); err != nil {
						t.Fatalf("save creator: %v", err)
					}
					if actor.username != "creator" {
						user := &SaaSUser{GitHubUsername: actor.username, Hives: map[string]string{}}
						if actor.role != "" {
							user.Hives[hiveID] = actor.role
						}
						if err := saveSaaSUser(user); err != nil {
							t.Fatalf("save actor: %v", err)
						}
					}
					if tc.prepare != nil {
						tc.prepare(t, s, hiveID)
					}

					req := setPathValue(reqWithUser(tc.method, tc.target, tc.body, actor.username), "id", hiveID)
					rec := httptest.NewRecorder()
					tc.handler(s, rec, req)

					if actor.wantStatus == http.StatusForbidden {
						if rec.Code != http.StatusForbidden {
							t.Fatalf("%s got status %d, want 403 (body=%s)", actor.name, rec.Code, rec.Body.String())
						}
						return
					}
					if rec.Code == http.StatusForbidden {
						t.Fatalf("%s got 403, want authorization to pass (body=%s)", actor.name, rec.Body.String())
					}
				})
			}
		})
	}
}

// TestHiveStatusAccessSemantics pins handleHiveStatus's owner-only semantics
// (operator decision 2026-08-13, matching v2's tightening): effective owners
// (creator and granted owner) pass via userIsHiveOwner; granted read and
// read-write roles are rejected — the full SaaSHive record carries operational
// metadata beyond what a read grant implies; unrelated users remain rejected
// as the positive control that the gate is still enforced.
func TestHiveStatusAccessSemantics(t *testing.T) {
	actors := []struct {
		name       string
		username   string
		role       string
		wantStatus int
	}{
		{name: "creator is allowed", username: "creator", wantStatus: http.StatusOK},
		{name: "granted owner is allowed", username: "granted-owner", role: "owner", wantStatus: http.StatusOK},
		{name: "granted read is rejected (owner-only)", username: "reader", role: "read", wantStatus: http.StatusForbidden},
		{name: "granted read-write is rejected (owner-only)", username: "writer", role: "read-write", wantStatus: http.StatusForbidden},
		{name: "unrelated user is rejected", username: "stranger", wantStatus: http.StatusForbidden},
	}

	for _, actor := range actors {
		t.Run(actor.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()

			s := newHandlerHub()
			hiveID := "hosted-parity-status"
			if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "creator", ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
				t.Fatalf("save hive: %v", err)
			}
			if err := saveSaaSUser(&SaaSUser{GitHubUsername: "creator", Hives: map[string]string{}}); err != nil {
				t.Fatalf("save creator: %v", err)
			}
			if actor.username != "creator" {
				user := &SaaSUser{GitHubUsername: actor.username, Hives: map[string]string{}}
				if actor.role != "" {
					user.Hives[hiveID] = actor.role
				}
				if err := saveSaaSUser(user); err != nil {
					t.Fatalf("save actor: %v", err)
				}
			}

			req := setPathValue(reqWithUser(http.MethodGet, "/status", "", actor.username), "id", hiveID)
			rec := httptest.NewRecorder()
			s.handleHiveStatus(rec, req)

			if rec.Code != actor.wantStatus {
				t.Fatalf("%s got status %d, want %d (body=%s)", actor.name, rec.Code, actor.wantStatus, rec.Body.String())
			}
		})
	}
}
