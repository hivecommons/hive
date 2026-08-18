package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// grantableTestSecret signs the session cookies these tests present.
const grantableTestSecret = "grantable-test-secret"

// withTempSaaSDirs redirects both the users and hives directories at temp
// trees so the roster handler reads fixtures instead of the real /data PVC.
func withTempSaaSDirs(t *testing.T) {
	t.Helper()
	origUsers, origHives := saasUsersDir, saasHivesDir
	dir := t.TempDir()
	saasUsersDir = dir + "/users"
	saasHivesDir = dir + "/hives"
	t.Cleanup(func() { saasUsersDir = origUsers; saasHivesDir = origHives })
}

// grantableTestServer builds a HubServer whose cookie secret matches the one
// these tests sign with.
func grantableTestServer() *HubServer {
	return &HubServer{logger: slog.Default(), hubSecret: grantableTestSecret}
}

// putUser persists a SaaSUser fixture.
func putUser(t *testing.T, username string) {
	t.Helper()
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: username}); err != nil {
		t.Fatalf("saveSaaSUser(%q): %v", username, err)
	}
}

// putHiveOwnedBy persists a SaaSHive fixture with the given owner.
func putHiveOwnedBy(t *testing.T, id, owner string) {
	t.Helper()
	if err := saveSaaSHive(&SaaSHive{ID: id, Owner: owner}); err != nil {
		t.Fatalf("saveSaaSHive(%q): %v", id, err)
	}
}

// getGrantableUsers calls the handler as username (empty means logged out).
func getGrantableUsers(t *testing.T, s *HubServer, username string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/saas/grantable-users", nil)
	if username != "" {
		// Sign with the derived SESSION sub-key (C2 domain separation), the same
		// key the hub verifies session cookies with.
		req.AddCookie(&http.Cookie{
			Name:  "hive_hub_user",
			Value: mintHubUserCookieValueV2(deriveDomainKey(grantableTestSecret, infoSessionEd25519Seed), username),
		})
	}
	rec := httptest.NewRecorder()
	s.handleGrantableUsers(rec, req)
	return rec
}

// decodeUsers pulls the roster out of a successful response.
func decodeUsers(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	return resp.Users
}

// TestHandleGrantableUsersRequiresHiveOwnership pins the access bar stated in
// the handler: the roster is visible to anyone who owns at least one hive —
// the same bar as being able to open Manage Access at all — and to nobody
// else. A logged-out or hive-less caller must be refused, not shown the full
// user list.
func TestHandleGrantableUsersRequiresHiveOwnership(t *testing.T) {
	tests := []struct {
		name     string
		username string
		// ownsHive seeds a hive owned by username.
		ownsHive bool
		wantCode int
	}{
		{name: "owner of a hive may list", username: "alice", ownsHive: true, wantCode: http.StatusOK},
		{name: "admin may list without owning anything", username: hubAdminUsername, wantCode: http.StatusOK},
		{name: "user owning no hive is refused", username: "mallory", wantCode: http.StatusForbidden},
		{name: "logged out is refused", username: "", wantCode: http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withTempSaaSDirs(t)
			putUser(t, "alice")
			putUser(t, "bob")
			if tc.username != "" {
				putUser(t, tc.username)
			}
			if tc.ownsHive {
				putHiveOwnedBy(t, "hive-1", tc.username)
			} else {
				// A hive owned by someone else must not grant access.
				putHiveOwnedBy(t, "hive-1", "someone-else")
			}

			rec := getGrantableUsers(t, grantableTestServer(), tc.username)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// TestHandleGrantableUsersReturnsSortedRoster checks the successful response:
// every registered username, sorted for a stable UI, as a JSON array.
func TestHandleGrantableUsersReturnsSortedRoster(t *testing.T) {
	withTempSaaSDirs(t)
	// Deliberately created out of order.
	for _, u := range []string{"carol", "alice", "bob"} {
		putUser(t, u)
	}
	putHiveOwnedBy(t, "hive-1", "alice")

	rec := getGrantableUsers(t, grantableTestServer(), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	got := decodeUsers(t, rec)
	want := []string{"alice", "bob", "carol"}
	if len(got) != len(want) {
		t.Fatalf("roster = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("roster = %v, want %v (sorted)", got, want)
			break
		}
	}
}

// TestHandleGrantableUsersRosterIsNeverNull checks the users field always
// serialises as a JSON array, never null, since the UI iterates it directly.
// The roster is built with make([]string, 0) precisely for this reason, and a
// hub whose only record is the caller's own is the case that would otherwise
// expose a nil slice.
func TestHandleGrantableUsersRosterIsNeverNull(t *testing.T) {
	withTempSaaSDirs(t)
	// getAuthUser requires the caller's own record to exist.
	putUser(t, hubAdminUsername)
	putHiveOwnedBy(t, "hive-1", hubAdminUsername)

	rec := getGrantableUsers(t, grantableTestServer(), hubAdminUsername)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if got := decodeUsers(t, rec); got == nil {
		t.Errorf("users = null (body=%q), want a JSON array", rec.Body.String())
	}
}

// TestHandleGrantableUsersSkipsRecordsWithoutUsername checks the filter: a
// user record on the PVC that carries no GitHub username is not emitted as an
// empty entry, which would render as a blank row in the Manage Access picker.
func TestHandleGrantableUsersSkipsRecordsWithoutUsername(t *testing.T) {
	withTempSaaSDirs(t)
	putUser(t, "alice")
	putHiveOwnedBy(t, "hive-1", "alice")
	// A record whose file exists but whose GitHubUsername field is empty.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "blank-record"}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	blankPath := saasUsersDir + "/blank-record.json"
	if err := os.WriteFile(blankPath, []byte(`{"github_username":""}`), 0o644); err != nil {
		t.Fatalf("write blank record: %v", err)
	}

	rec := getGrantableUsers(t, grantableTestServer(), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	for _, u := range decodeUsers(t, rec) {
		if u == "" {
			t.Errorf("roster contains an empty username: %v", decodeUsers(t, rec))
		}
	}
}
