package hub

import (
	"encoding/json"
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
	for _, user := range []string{"writer", "target", ""} {
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

// TestHeartbeatOwnerCannotManageSelfRegisteredHive pins #19653: the registry
// Owner of a self-registered spoke comes from the spoke's own heartbeat, so it
// must not grant, revoke, approve, list or audit hub access to that hive.
func TestHeartbeatOwnerCannotManageSelfRegisteredHive(t *testing.T) {
	s := seedSelfRegisteredHive(t)
	hive := map[string]string{"id": selfRegisteredHiveID}
	mustSaveAccessOwnerRoleUser(t, "target", map[string]string{selfRegisteredHiveID: "read"})

	if rec := accessCall(s, s.handleAccessAdd, http.MethodPost, `{"username":"target","role":"owner"}`, "spoke-owner", hive); rec.Code != http.StatusForbidden {
		t.Fatalf("add: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSUser("target").Hives[selfRegisteredHiveID]; got != "read" {
		t.Fatalf("after refused add: target role = %q, want read", got)
	}
	grant := map[string]string{"id": selfRegisteredHiveID, "username": "target"}
	if rec := accessCall(s, s.handleAccessRemove, http.MethodDelete, "", "spoke-owner", grant); rec.Code != http.StatusForbidden {
		t.Fatalf("remove: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSUser("target").Hives[selfRegisteredHiveID]; got != "read" {
		t.Fatalf("after refused remove: target role = %q, want read", got)
	}
	if rec := accessCall(s, s.handleAccessList, http.MethodGet, "", "spoke-owner", hive); rec.Code != http.StatusForbidden {
		t.Fatalf("list: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if rec := accessCall(s, s.handleAccessLog, http.MethodGet, "", "spoke-owner", hive); rec.Code != http.StatusForbidden {
		t.Fatalf("access log: status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}

	for _, step := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		method  string
		body    string
	}{
		{"approve-request", s.handleApproveRequest, http.MethodPost, `{"role":"read"}`},
		{"approve-access", s.handleApproveAccess, http.MethodPut, ""},
	} {
		requester := "requester-" + step.name
		mustSaveAccessOwnerRoleUser(t, requester, map[string]string{})
		if rec := accessCall(s, s.handleRequestAccess, http.MethodPost, `{"note":"please"}`, requester, hive); rec.Code != http.StatusOK {
			t.Fatalf("%s: request: status = %d, want 200 (body=%q)", step.name, rec.Code, rec.Body.String())
		}
		path := map[string]string{"id": selfRegisteredHiveID, "username": requester}
		if rec := accessCall(s, step.handler, step.method, step.body, "spoke-owner", path); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403 (body=%q)", step.name, rec.Code, rec.Body.String())
		}
		if _, ok := loadSaaSUser(requester).Hives[selfRegisteredHiveID]; ok {
			t.Fatalf("%s: a refused approval wrote a grant", step.name)
		}
	}
	assertNoMetaRecord(t, selfRegisteredHiveID)
}

// TestHostedHiveOwnerStillManagesAccess pins that #19653 leaves hosted hives
// alone: the owner on the SaaS record still grants and revokes, even when the
// hive also has a registry entry.
func TestHostedHiveOwnerStillManagesAccess(t *testing.T) {
	s := seedSelfRegisteredHive(t)
	const hostedID = "hosted-acme"
	if err := saveSaaSHive(&SaaSHive{ID: hostedID, Owner: "hosted-owner"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	mustSaveAccessOwnerRoleUser(t, "hosted-owner", map[string]string{})
	s.registry.Hives = append(s.registry.Hives, RegistryEntry{ID: hostedID, Owner: "hosted-owner"})
	hive := map[string]string{"id": hostedID}

	if rec := accessCall(s, s.handleAccessAdd, http.MethodPost, `{"username":"target","role":"read"}`, "hosted-owner", hive); rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if rec := accessCall(s, s.handleAccessList, http.MethodGet, "", "hosted-owner", hive); rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	grant := map[string]string{"id": hostedID, "username": "target"}
	if rec := accessCall(s, s.handleAccessRemove, http.MethodDelete, "", "hosted-owner", grant); rec.Code != http.StatusOK {
		t.Fatalf("remove: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if rec := accessCall(s, s.handleGrantableUsers, http.MethodGet, "", "hosted-owner", nil); rec.Code != http.StatusOK {
		t.Fatalf("grantable users: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestGrantableUsersRefusesHeartbeatOwner(t *testing.T) {
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

func TestHubAccessRole(t *testing.T) {
	hosted := &SaaSHive{ID: "hosted-x", Owner: "alice"}
	stored := map[string]string{"h": "read-write", "hosted-x": "read"}
	for _, tc := range []struct {
		name     string
		username string
		hiveID   string
		isAdmin  bool
		sh       *SaaSHive
		repaired map[string]bool
		want     string
	}{
		{"admin", "root", "h", true, nil, nil, roleOwner},
		{"repaired", "bob", "h", false, nil, map[string]bool{"h": true}, roleOwner},
		{"hosted owner", "alice", "hosted-x", false, hosted, nil, roleOwner},
		{"hosted non-owner", "bob", "hosted-x", false, hosted, nil, "read"},
		{"self-registered stored grant", "bob", "h", false, nil, nil, "read-write"},
		{"self-registered no grant", "bob", "other", false, nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hubAccessRole(tc.username, stored, tc.hiveID, tc.isAdmin, tc.sh, tc.repaired); got != tc.want {
				t.Fatalf("hubAccessRole = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMyHivesWithholdsAccessFromHeartbeatOwner pins the My Hives side of
// #19653: the heartbeat owner still sees the row as owner, but its accessRole
// is empty, no owner grant is persisted for it, and the access roster and
// pending requests that only an access owner may see stay off the row.
func TestMyHivesWithholdsAccessFromHeartbeatOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	const hostedID = "hosted-acme"
	if err := saveSaaSHive(&SaaSHive{ID: hostedID, Owner: "spoke-owner"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	mustSaveAccessOwnerRoleUser(t, "spoke-owner", map[string]string{})
	mustSaveAccessOwnerRoleUser(t, "co-owner", map[string]string{selfRegisteredHiveID: "owner"})
	saveAccessRequests(selfRegisteredHiveID, []AccessRequest{{Username: "target", Status: "pending"}})
	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{
		{ID: selfRegisteredHiveID, Owner: "spoke-owner", Online: true},
		{ID: hostedID, Owner: "spoke-owner", Online: true},
	}

	rows := func(user string) map[string]MyHiveEntry {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", user))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body=%s)", user, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"accessRole":`) {
			t.Fatalf("%s: payload has no accessRole field", user)
		}
		var resp struct {
			Hives []MyHiveEntry `json:"hives"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]MyHiveEntry{}
		for _, h := range resp.Hives {
			out[h.ID] = h
		}
		return out
	}

	got := rows("spoke-owner")
	self, ok := got[selfRegisteredHiveID]
	if !ok {
		t.Fatal("self-registered hive missing from the heartbeat owner's My Hives")
	}
	if self.Role != roleOwner || self.AccessRole != "" {
		t.Fatalf("self-registered row: role = %q, accessRole = %q, want owner and empty", self.Role, self.AccessRole)
	}
	if len(self.Access) != 0 || self.PendingRequestCount != 0 {
		t.Fatalf("self-registered row: access = %v, pending = %d, want none", self.Access, self.PendingRequestCount)
	}
	if h := got[hostedID]; h.Role != roleOwner || h.AccessRole != roleOwner {
		t.Fatalf("hosted row: role = %q, accessRole = %q, want owner for both", h.Role, h.AccessRole)
	}
	u := loadSaaSUser("spoke-owner")
	if _, ok := u.Hives[selfRegisteredHiveID]; ok {
		t.Fatalf("My Hives persisted a grant for the heartbeat owner: %v", u.Hives)
	}
	if u.Hives[hostedID] != roleOwner {
		t.Fatalf("hosted owner grant = %q, want owner (repair unchanged for hosted hives)", u.Hives[hostedID])
	}

	co := rows("co-owner")[selfRegisteredHiveID]
	if co.AccessRole != roleOwner || co.PendingRequestCount != 1 {
		t.Fatalf("co-owner row: accessRole = %q, pending = %d, want owner and 1", co.AccessRole, co.PendingRequestCount)
	}
}

func TestDashboardOffersManageAccessOnSelfRegisteredHive(t *testing.T) {
	runNodeAssertions(t, "const assert = require('node:assert/strict');\n"+
		jsFunc(t, "roleAtLeast")+"\n"+jsFunc(t, "hubAccessRole")+"\n"+jsFunc(t, "canManageAccess")+`
for (const role of ['owner', 'merger', 'read-write']) {
  assert.equal(canManageAccess({id: 'self-registered', hiveType: 'local', role, accessRole: role}), true, 'self-registered hive, role ' + role);
  assert.equal(canManageAccess({id: 'hosted-acme', hiveType: 'hosted', role, accessRole: role}), true, 'hosted hive, role ' + role);
  assert.equal(canManageAccess({id: 'hosted-acme', hiveType: 'hosted', role}), true, 'older hub without accessRole, role ' + role);
}
assert.equal(canManageAccess({id: 'self-registered', hiveType: 'local', role: 'owner', accessRole: ''}), false, 'heartbeat owner with no hub access');
assert.equal(canManageAccess({id: 'self-registered', hiveType: 'local', role: 'read', accessRole: 'read'}), false, 'read role');
assert.equal(canManageAccess({id: 'hosted-acme', hiveType: 'hosted', role: 'read', accessRole: 'read'}), false, 'hosted read role');
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
