package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withProvisionContextDirs points the user and provision-request stores at a
// temp dir so these tests never touch /data.
func withProvisionContextDirs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	origUsers, origReqs := saasUsersDir, provisionRequestsDir
	saasUsersDir = filepath.Join(dir, "users")
	provisionRequestsDir = filepath.Join(dir, "provision-requests")
	t.Cleanup(func() { saasUsersDir, provisionRequestsDir = origUsers, origReqs })
}

// TestRoleForUserOnHive covers the lookup that puts "· owner" next to the hive
// an approved request was assigned. The role comes from SaaSUser.Hives — the
// authoritative grant — so a revoked grant must read as no role rather than
// silently keeping the badge.
func TestRoleForUserOnHive(t *testing.T) {
	users := []SaaSUser{
		{GitHubUsername: "alice", Hives: map[string]string{
			"hosted-oke-06": "owner",
			"hosted-oke-07": "read-write",
		}},
		{GitHubUsername: "Bob", Hives: map[string]string{"hosted-oke-08": "read"}},
		{GitHubUsername: "carol", Hives: nil},
	}

	tests := []struct {
		name     string
		username string
		hiveID   string
		want     string
	}{
		{"owner role", "alice", "hosted-oke-06", "owner"},
		{"read-write role", "alice", "hosted-oke-07", "read-write"},
		{"username match is case-insensitive", "bob", "hosted-oke-08", "read"},
		{"no grant on that hive", "alice", "hosted-oke-99", ""},
		{"user has no hives at all", "carol", "hosted-oke-06", ""},
		{"unknown user", "mallory", "hosted-oke-06", ""},
		{"empty username", "", "hosted-oke-06", ""},
		{"empty hive id", "alice", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roleForUserOnHive(tt.username, tt.hiveID, users); got != tt.want {
				t.Errorf("roleForUserOnHive(%q, %q) = %q, want %q", tt.username, tt.hiveID, got, tt.want)
			}
		})
	}
}

// TestHivesForUser covers the "other hives" cell: the requester's footprint
// beyond the hive this row is about, owners first then alphabetical, with the
// assigned hive excluded so it is not shown twice.
func TestHivesForUser(t *testing.T) {
	users := []SaaSUser{
		{GitHubUsername: "alice", Hives: map[string]string{
			"hosted-b": "read",
			"hosted-a": "read-write",
			"hosted-z": "owner",
			"hosted-c": "owner",
		}},
		{GitHubUsername: "lonely", Hives: map[string]string{"hosted-only": "owner"}},
		{GitHubUsername: "nohives", Hives: map[string]string{}},
	}

	tests := []struct {
		name     string
		username string
		exclude  string
		want     []UserHiveRole
	}{
		{
			name:     "owners first then alphabetical, assigned hive excluded",
			username: "alice",
			exclude:  "hosted-a",
			want: []UserHiveRole{
				{HiveID: "hosted-c", Role: "owner"},
				{HiveID: "hosted-z", Role: "owner"},
				{HiveID: "hosted-b", Role: "read"},
			},
		},
		{
			name:     "nothing excluded returns the whole footprint",
			username: "alice",
			exclude:  "",
			want: []UserHiveRole{
				{HiveID: "hosted-c", Role: "owner"},
				{HiveID: "hosted-z", Role: "owner"},
				{HiveID: "hosted-a", Role: "read-write"},
				{HiveID: "hosted-b", Role: "read"},
			},
		},
		{
			// A user whose ONLY hive is the one they were just assigned has no
			// other memberships — the cell must render "—", not an empty list.
			name:     "user with no other hives",
			username: "lonely",
			exclude:  "hosted-only",
			want:     nil,
		},
		{"user with no hives at all", "nohives", "", nil},
		{"unknown user", "mallory", "", nil},
		{"empty username", "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hivesForUser(tt.username, tt.exclude, users)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hivesForUser(%q, %q) = %+v, want %+v", tt.username, tt.exclude, got, tt.want)
			}
		})
	}
}

// TestEnrichProvisionRequests checks the per-row enrichment the Past Requests
// table reads, including that a denied request gets no assigned role.
func TestEnrichProvisionRequests(t *testing.T) {
	withProvisionContextDirs(t)

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{
		"hosted-oke-06": "owner",
		"hosted-oke-07": "read",
	}}); err != nil {
		t.Fatalf("saveSaaSUser(alice): %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "bob", Hives: map[string]string{}}); err != nil {
		t.Fatalf("saveSaaSUser(bob): %v", err)
	}

	reqs := []ProvisionRequest{
		{Username: "alice", Status: provisionStatusApproved, AssignedHive: "hosted-oke-06"},
		{Username: "bob", Status: provisionStatusDenied, DenyReason: "no capacity"},
		{Username: "ghost", Status: provisionStatusApproved, AssignedHive: "hosted-oke-99"},
	}
	got := enrichProvisionRequests(reqs)

	if got[0].AssignedRole != "owner" {
		t.Errorf("alice assigned_role = %q, want %q", got[0].AssignedRole, "owner")
	}
	wantOther := []UserHiveRole{{HiveID: "hosted-oke-07", Role: "read"}}
	if !reflect.DeepEqual(got[0].OtherHives, wantOther) {
		t.Errorf("alice other_hives = %+v, want %+v", got[0].OtherHives, wantOther)
	}
	// A denial assigns nothing, so there is no role to show.
	if got[1].AssignedRole != "" || got[1].OtherHives != nil {
		t.Errorf("denied request enriched: role=%q other=%+v", got[1].AssignedRole, got[1].OtherHives)
	}
	// A request whose requester has no user record must not invent access.
	if got[2].AssignedRole != "" || got[2].OtherHives != nil {
		t.Errorf("unknown requester enriched: role=%q other=%+v", got[2].AssignedRole, got[2].OtherHives)
	}

	// Empty input must not blow up or read the roster.
	if out := enrichProvisionRequests(nil); out != nil {
		t.Errorf("enrichProvisionRequests(nil) = %+v, want nil", out)
	}
}

// TestPastRequestsContextStaysAdminOnly pins the authorization boundary. The
// enriched payload exposes OTHER users' hive memberships and roles, so it must
// only ever reach the hub admin. A non-admin (or anonymous) caller hitting an
// admin route gets a 4xx and no body at all.
func TestPastRequestsContextStaysAdminOnly(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// Stand-in for any handler that would serve enriched provision requests.
	handler := srv.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"provision_requests":[]}`))
	})

	tests := []struct {
		name     string
		cookie   string
		wantCode int
	}{
		{"anonymous caller is rejected", "", http.StatusForbidden},
		{"non-admin user is rejected", "regularuser", http.StatusForbidden},
		{"another non-admin is rejected", "alice", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/saas/admin/users", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: tt.cookie})
			}
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if body := w.Body.String(); strings.Contains(body, "provision_requests") {
				t.Errorf("non-admin response leaked provision context: %s", body)
			}
		})
	}
}
