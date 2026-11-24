package hub

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const selfRegisteredHiveID = "self-registered"

func seedSelfRegisteredHive(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	mustSaveAccessOwnerRoleUser(t, "spoke-owner", map[string]string{})
	mustSaveAccessOwnerRoleUser(t, hubAdminUsername, map[string]string{})
	mustSaveAccessOwnerRoleUser(t, "co-owner", map[string]string{selfRegisteredHiveID: "owner"})
	mustSaveAccessOwnerRoleUser(t, "writer", map[string]string{selfRegisteredHiveID: "read-write"})
	mustSaveAccessOwnerRoleUser(t, "target", map[string]string{})
	return &HubServer{
		logger:    slog.Default(),
		hubSecret: testHubSecret,
		timeline:  newTimelineStore(),
		registry:  Registry{Hives: []RegistryEntry{{ID: selfRegisteredHiveID, Owner: "spoke-owner"}}},
	}
}

func assertNoMetaRecord(t *testing.T, hiveID string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(saasHivesDir, hiveID, "meta.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("meta.json for %s: stat err = %v, want not exist", hiveID, err)
	}
}

func accessCall(s *HubServer, handler func(http.ResponseWriter, *http.Request), method, body, user string, path map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler(rec, setPathValues(accessOwnerRoleReq(method, "/access", body, user), path))
	return rec
}

func TestAccessGrantsOnSelfRegisteredHive(t *testing.T) {
	for _, user := range []string{hubAdminUsername, "co-owner"} {
		t.Run(user, func(t *testing.T) {
			s := seedSelfRegisteredHive(t)
			hive := map[string]string{"id": selfRegisteredHiveID}
			grant := map[string]string{"id": selfRegisteredHiveID, "username": "target"}

			if rec := accessCall(s, s.handleAccessAdd, http.MethodPost, `{"username":"target","role":"read"}`, user, hive); rec.Code != http.StatusOK {
				t.Fatalf("add: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			if got := loadSaaSUser("target").Hives[selfRegisteredHiveID]; got != "read" {
				t.Fatalf("after add: target role = %q, want read", got)
			}
			rec := accessCall(s, s.handleAccessList, http.MethodGet, "", user, hive)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"target"`) {
				t.Fatalf("list: status = %d, body = %q, want 200 naming target", rec.Code, rec.Body.String())
			}
			rec = accessCall(s, s.handleAccessLog, http.MethodGet, "", user, hive)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "access granted to target as read") {
				t.Fatalf("access log: status = %d, body = %q, want 200 with the grant", rec.Code, rec.Body.String())
			}
			if rec := accessCall(s, s.handleAccessRemove, http.MethodDelete, "", user, grant); rec.Code != http.StatusOK {
				t.Fatalf("remove: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			if _, ok := loadSaaSUser("target").Hives[selfRegisteredHiveID]; ok {
				t.Fatalf("after remove: target still has a grant")
			}
			assertNoMetaRecord(t, selfRegisteredHiveID)
		})
	}
}

func TestAccessGrantsOnSelfRegisteredHiveRefuseNonOwners(t *testing.T) {
	for _, user := range []string{"spoke-owner", "writer", "target", ""} {
		t.Run(user, func(t *testing.T) {
			s := seedSelfRegisteredHive(t)
			hive := map[string]string{"id": selfRegisteredHiveID}
			if rec := accessCall(s, s.handleAccessAdd, http.MethodPost, `{"username":"target","role":"owner"}`, user, hive); rec.Code != http.StatusForbidden {
				t.Fatalf("add: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
			}
			if rec := accessCall(s, s.handleAccessList, http.MethodGet, "", user, hive); rec.Code != http.StatusForbidden {
				t.Fatalf("list: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
			}
			if _, ok := loadSaaSUser("target").Hives[selfRegisteredHiveID]; ok {
				t.Fatalf("a refused add wrote a grant")
			}
		})
	}
}

func TestAccessGrantRefusesUnknownHive(t *testing.T) {
	s := seedSelfRegisteredHive(t)
	rec := accessCall(s, s.handleAccessAdd, http.MethodPost, `{"username":"newcomer","role":"read"}`, hubAdminUsername, map[string]string{"id": "nowhere"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
	if loadSaaSUser("newcomer") != nil {
		t.Fatalf("an add for an unknown hive created a user record")
	}
}

func TestAccessRequestsOnSelfRegisteredHive(t *testing.T) {
	s := seedSelfRegisteredHive(t)
	hive := map[string]string{"id": selfRegisteredHiveID}
	grant := map[string]string{"id": selfRegisteredHiveID, "username": "target"}

	if rec := accessCall(s, s.handleRequestAccess, http.MethodPost, `{"note":"I review this project"}`, "target", hive); rec.Code != http.StatusOK {
		t.Fatalf("request: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	rec := accessCall(s, s.handleGetRequests, http.MethodGet, "", "writer", hive)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"target"`) {
		t.Fatalf("pending requests: status = %d, body = %q, want 200 naming target", rec.Code, rec.Body.String())
	}
	if rec := accessCall(s, s.handleApproveRequest, http.MethodPost, `{"role":"read"}`, "co-owner", grant); rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSUser("target").Hives[selfRegisteredHiveID]; got != "read" {
		t.Fatalf("after approve: target role = %q, want read", got)
	}

	for _, step := range []struct {
		requester string
		handler   func(http.ResponseWriter, *http.Request)
		method    string
		wantRole  string
	}{
		{"denied", s.handleDenyRequest, http.MethodPost, ""},
		{"approved-by-put", s.handleApproveAccess, http.MethodPut, "read"},
		{"denied-by-delete", s.handleDenyAccess, http.MethodDelete, ""},
	} {
		mustSaveAccessOwnerRoleUser(t, step.requester, map[string]string{})
		if rec := accessCall(s, s.handleRequestAccess, http.MethodPost, `{"note":"please"}`, step.requester, hive); rec.Code != http.StatusOK {
			t.Fatalf("%s: request: status = %d, want 200 (body=%q)", step.requester, rec.Code, rec.Body.String())
		}
		path := map[string]string{"id": selfRegisteredHiveID, "username": step.requester}
		if rec := accessCall(s, step.handler, step.method, "", "co-owner", path); rec.Code != http.StatusOK {
			t.Fatalf("%s: decision: status = %d, want 200 (body=%q)", step.requester, rec.Code, rec.Body.String())
		}
		var got string
		if u := loadSaaSUser(step.requester); u != nil {
			got = u.Hives[selfRegisteredHiveID]
		}
		if got != step.wantRole {
			t.Fatalf("%s: role = %q, want %q", step.requester, got, step.wantRole)
		}
	}
	assertNoMetaRecord(t, selfRegisteredHiveID)
}

func TestGrantableUsersRefusesHeartbeatReportedOwner(t *testing.T) {
	s := seedSelfRegisteredHive(t)
	if rec := accessCall(s, s.handleGrantableUsers, http.MethodGet, "", "spoke-owner", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("spoke-owner: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	for _, user := range []string{hubAdminUsername, "co-owner"} {
		if rec := accessCall(s, s.handleGrantableUsers, http.MethodGet, "", user, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body=%q)", user, rec.Code, rec.Body.String())
		}
	}
}

func TestHeartbeatReportedOwnerCannotApproveOrRevoke(t *testing.T) {
	s := seedSelfRegisteredHive(t)
	hive := map[string]string{"id": selfRegisteredHiveID}
	grant := map[string]string{"id": selfRegisteredHiveID, "username": "target"}
	mustSaveAccessOwnerRoleUser(t, "holder", map[string]string{selfRegisteredHiveID: "read"})
	revoke := map[string]string{"id": selfRegisteredHiveID, "username": "holder"}

	if rec := accessCall(s, s.handleRequestAccess, http.MethodPost, `{"note":"please"}`, "target", hive); rec.Code != http.StatusOK {
		t.Fatalf("request: status = %d, want 200", rec.Code)
	}
	if rec := accessCall(s, s.handleApproveRequest, http.MethodPost, `{"role":"read"}`, "spoke-owner", grant); rec.Code != http.StatusForbidden {
		t.Fatalf("approve: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if rec := accessCall(s, s.handleAccessRemove, http.MethodDelete, "", "spoke-owner", revoke); rec.Code != http.StatusForbidden {
		t.Fatalf("revoke: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if _, ok := loadSaaSUser("target").Hives[selfRegisteredHiveID]; ok {
		t.Fatalf("a refused approve wrote a grant")
	}
	if _, ok := loadSaaSUser("holder").Hives[selfRegisteredHiveID]; !ok {
		t.Fatalf("a refused revoke removed the grant")
	}
}

func TestDashboardOffersManageAccessOnSelfRegisteredHive(t *testing.T) {
	runNodeAssertions(t, "const assert = require('node:assert/strict');\n"+
		jsFunc(t, "roleAtLeast")+"\n"+jsFunc(t, "canManageAccess")+`
for (const role of ['owner', 'merger', 'read-write']) {
  assert.equal(canManageAccess({id: 'self-registered', hiveType: 'local', role}), true, 'self-registered hive, role ' + role);
  assert.equal(canManageAccess({id: 'hosted-acme', hiveType: 'hosted', role}), true, 'hosted hive, role ' + role);
}
assert.equal(canManageAccess({id: 'self-registered', hiveType: 'local', role: 'read'}), false, 'read role');
assert.equal(canManageAccess({id: 'hosted-acme', hiveType: 'hosted', role: 'read'}), false, 'hosted read role');
`)
}

func runNodeAssertions(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node assertions failed: %v\n%s", err, out)
	}
}
