package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
			name:    "hive status",
			method:  http.MethodGet,
			target:  "/status",
			handler: (*HubServer).handleHiveStatus,
		},
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
					user := &SaaSUser{GitHubUsername: actor.username, Hives: map[string]string{}}
					if actor.role != "" {
						user.Hives[hiveID] = actor.role
					}
					if err := saveSaaSUser(user); err != nil {
						t.Fatalf("save actor: %v", err)
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
						t.Fatalf("granted owner got 403, want authorization to pass (body=%s)", rec.Body.String())
					}
				})
			}
		})
	}
}
