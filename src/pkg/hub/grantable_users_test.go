package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// TestGrantableUserLabel pins the display-label preference order for the
// Manage Access picker: provider-asserted display name, then a plain GitHub
// login, then email, then linked GitHub login, then a truncated
// "provider: subject…" fallback. The label is display-only — the identity key
// used for grants is always the record's GitHubUsername and never changes.
func TestGrantableUserLabel(t *testing.T) {
	tests := []struct {
		name string
		user SaaSUser
		want string
	}{
		{
			name: "plain github login stays as-is",
			user: SaaSUser{GitHubUsername: "octocat"},
			want: "octocat",
		},
		{
			name: "github-prefixed key drops the prefix",
			user: SaaSUser{GitHubUsername: "github:octocat"},
			want: "octocat",
		},
		{
			name: "display name wins over everything",
			user: SaaSUser{GitHubUsername: "google:107812345678901234567", DisplayName: "Jane Doe", Email: "jane@example.com"},
			want: "Jane Doe",
		},
		{
			name: "email beats the raw google id",
			user: SaaSUser{GitHubUsername: "google:107812345678901234567", Email: "jane@example.com"},
			want: "jane@example.com",
		},
		{
			name: "linked github login beats the raw ibmid",
			user: SaaSUser{GitHubUsername: "ibmid:650001ABCD", LinkedGitHubLogin: "jane-gh"},
			want: "jane-gh",
		},
		{
			name: "opaque microsoft token-like sub is truncated with provider prefix",
			user: SaaSUser{GitHubUsername: "microsoft:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			want: "microsoft: AAAAAAAAAAAA…",
		},
		{
			name: "short opaque sub is shown whole",
			user: SaaSUser{GitHubUsername: "ibmid:65001"},
			want: "ibmid: 65001",
		},
		{
			name: "display name wins for a plain github user too",
			user: SaaSUser{GitHubUsername: "octocat", DisplayName: "The Octocat"},
			want: "The Octocat",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.user
			if got := grantableUserLabel(&u); got != tc.want {
				t.Errorf("grantableUserLabel(%+v) = %q, want %q", tc.user, got, tc.want)
			}
		})
	}
}

// TestGrantableUserProvider pins provider classification: the stored Provider
// field wins, else the identity-key prefix, else github (legacy plain login).
func TestGrantableUserProvider(t *testing.T) {
	tests := []struct {
		name string
		user SaaSUser
		want string
	}{
		{name: "stored provider wins", user: SaaSUser{GitHubUsername: "google:123", Provider: "google"}, want: "google"},
		{name: "stored ms alias normalizes to microsoft", user: SaaSUser{GitHubUsername: "ms:AAAA", Provider: "ms"}, want: "microsoft"},
		{name: "prefix classifies a legacy record", user: SaaSUser{GitHubUsername: "ibmid:650"}, want: "ibmid"},
		{name: "microsoft prefix", user: SaaSUser{GitHubUsername: "microsoft:AAAA"}, want: "microsoft"},
		{name: "ms prefix normalizes to microsoft", user: SaaSUser{GitHubUsername: "ms:AAAA"}, want: "microsoft"},
		{name: "github.com/ prefixed key classifies as github", user: SaaSUser{GitHubUsername: "github.com/octocat"}, want: "github"},
		{name: "plain login defaults to github", user: SaaSUser{GitHubUsername: "octocat"}, want: "github"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.user
			if got := grantableUserProvider(&u); got != tc.want {
				t.Errorf("grantableUserProvider(%+v) = %q, want %q", tc.user, got, tc.want)
			}
		})
	}
}

// TestHandleGrantableUsersEntriesCarryNormalizedLabels checks the enriched
// payload: each entry pairs the STABLE identity key (id — what the grant POST
// must send) with the normalized human label, entries sort by that label, and
// the legacy "users" array still carries the raw IDs for older dashboards.
func TestHandleGrantableUsersEntriesCarryNormalizedLabels(t *testing.T) {
	withTempSaaSDirs(t)
	putUser(t, "alice")
	putHiveOwnedBy(t, "hive-1", "alice")
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "google:107812345678901234567", DisplayName: "Jane Doe"}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "microsoft:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	rec := getGrantableUsers(t, grantableTestServer(), "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users   []string             `json:"users"`
		Entries []grantableUserEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	wantEntries := []grantableUserEntry{
		{ID: "alice", Label: "alice", Provider: "github"},
		{ID: "google:107812345678901234567", Label: "Jane Doe", Provider: "google"},
		{ID: "microsoft:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Label: "microsoft: AAAAAAAAAAAA…", Provider: "microsoft"},
	}
	if len(resp.Entries) != len(wantEntries) {
		t.Fatalf("entries = %+v, want %+v", resp.Entries, wantEntries)
	}
	for i, want := range wantEntries {
		if resp.Entries[i] != want {
			t.Errorf("entries[%d] = %+v, want %+v", i, resp.Entries[i], want)
		}
	}
	// Legacy field unchanged: raw stable IDs, sorted.
	wantUsers := []string{"alice", "google:107812345678901234567", "microsoft:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	if len(resp.Users) != len(wantUsers) {
		t.Fatalf("users = %v, want %v", resp.Users, wantUsers)
	}
	for i := range wantUsers {
		if resp.Users[i] != wantUsers[i] {
			t.Errorf("users = %v, want %v", resp.Users, wantUsers)
			break
		}
	}
}

// TestManageAccessDialogHasUserSearch pins the search affordance in the
// Manage Access dialog: a search input wired to the client-side filter, and a
// dropdown renderer that matches case-insensitively against both the friendly
// label and the raw identity key.
func TestManageAccessDialogHasUserSearch(t *testing.T) {
	for _, want := range []string{
		`id="access-user-search"`,
		`oninput="filterAccessUserDropdown()"`,
		`function filterAccessUserDropdown()`,
		`function renderAccessUserOptions(filter)`,
		`e.label.toLowerCase().indexOf(q) !== -1 || e.id.toLowerCase().indexOf(q) !== -1`,
		// Fallback for older hub payloads that only carry bare usernames.
		`data.entries || (data.users || []).map`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML missing %q", want)
		}
	}
}

// TestManageAccessProviderIcons pins the provider-icon UI: the dashboard must
// classify raw identity keys by prefix, carry inline SVG marks (no external
// fetches, CSP-safe) for known providers, render an icon beside each name in
// the existing access rows, and prepend an emoji mark (native <option>
// elements cannot render SVG) in the Add User picker.
func TestManageAccessProviderIcons(t *testing.T) {
	for _, want := range []string{
		// Prefix classification logic, including the ms → microsoft alias and
		// the github.com/ + bare-login → github fallbacks.
		`function identityProviderFromKey(key)`,
		`if (key.indexOf('github.com/') === 0) return 'github';`,
		`if (p === 'ms') return 'microsoft';`,
		// Inline SVG marks for the known providers.
		`var PROVIDER_ICON_SVG = {`,
		`github: '<svg viewBox="0 0 16 16"`,
		`google: '<svg viewBox="0 0 48 48"`,
		`microsoft: '<svg viewBox="0 0 23 23"`,
		`ibmid: '<svg viewBox="0 0 16 16"`,
		// Emoji fallbacks for the <select>, plus the generic-person default.
		`var PROVIDER_EMOJI = { google: '🔵', ibmid: '🔷', microsoft: '🟦', github: '🐙' };`,
		`PROVIDER_ICON_SVG[provider] || '👤'`,
		`(PROVIDER_EMOJI[provider] || '👤')`,
		// Icons applied in both the existing rows and the Add User picker.
		`providerIconHTML(provider)`,
		`providerOptionEmoji(e.provider || identityProviderFromKey(e.id))`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML missing %q", want)
		}
	}
}

// TestManageAccessShowsGitHubDisplayNames pins the async display-name
// enrichment (#4145): Manage Access user rows carry an empty placeholder the
// GitHub profile lookup fills in AFTER the username is already on screen, and
// the Add User picker upgrades bare-login labels the same way. Both surfaces
// must render the username first and only ever ADD the display name — a
// failed or rate-limited lookup leaves the UI untouched. Results are cached
// (in-memory + localStorage) so re-opening the dialog does not re-spend the
// unauthenticated GitHub API rate budget.
func TestManageAccessShowsGitHubDisplayNames(t *testing.T) {
	for _, want := range []string{
		// Cache + fetch plumbing.
		`var GH_NAME_CACHE_KEY = 'hiveGhDisplayNames'`,
		`function fetchGitHubDisplayName(username)`,
		`https://api.github.com/users/` + `' + encodeURIComponent(login)`,
		// OIDC identity keys ("google:1078…") have no GitHub profile to ask about.
		`function isPlainGitHubLogin(username)`,
		// User rows: placeholder rendered next to the username, filled async.
		`class="gh-display-name" data-gh-login=`,
		`enrichGhDisplayNames(document.getElementById('access-list'))`,
		// Add User picker: labels upgraded in place, dropdown re-rendered once.
		`function enrichGrantableUserLabels()`,
		`enrichGrantableUserLabels();`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML missing %q", want)
		}
	}
}
