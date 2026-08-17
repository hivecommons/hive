package hub

import (
	"path/filepath"
	"testing"
)

// TestSaaSUserFilePaths_NonGitHubHasNoLegacyFallback: a non-GitHub provider
// (google, ibmid, redhat) produces ONLY the canonical filename path — there is
// no legacy "<subject>.json" to try. A bug here would make loadSaaSUser read
// the wrong file or miss a canonical record entirely.
func TestSaaSUserFilePaths_NonGitHubHasNoLegacyFallback(t *testing.T) {
	paths := saaSUserFilePaths("google:1078")
	if len(paths) != 1 {
		t.Fatalf("saaSUserFilePaths(google:1078) = %v, want exactly 1 path", paths)
	}
	if filepath.Base(paths[0]) != "google.1078.json" {
		t.Errorf("expected google.1078.json, got %q", filepath.Base(paths[0]))
	}
}

// TestSaaSUserFilePaths_GitHubHasLegacyFallback: a github: or bare identity
// produces TWO paths — canonical first, then legacy fallback.
func TestSaaSUserFilePaths_GitHubHasLegacyFallback(t *testing.T) {
	for _, id := range []string{"github:alice", "alice"} {
		paths := saaSUserFilePaths(id)
		if len(paths) != 2 {
			t.Fatalf("saaSUserFilePaths(%q) = %v, want 2 paths", id, paths)
		}
		if filepath.Base(paths[0]) != "github.alice.json" {
			t.Errorf("first path for %q: got %q, want github.alice.json", id, filepath.Base(paths[0]))
		}
		if filepath.Base(paths[1]) != "alice.json" {
			t.Errorf("second path for %q: got %q, want alice.json", id, filepath.Base(paths[1]))
		}
	}
}

// TestSaveSaaSUserPath_InvalidIdentityErrors: saveSaaSUserPath must reject a
// GitHubUsername that does not parse as a valid canonical identity. A silent
// fallback here could write user data to a garbage path.
func TestSaveSaaSUserPath_InvalidIdentityErrors(t *testing.T) {
	_, err := saveSaaSUserPath(&SaaSUser{GitHubUsername: ""})
	if err == nil {
		t.Error("saveSaaSUserPath with empty identity should error")
	}
	_, err = saveSaaSUserPath(&SaaSUser{GitHubUsername: "bogus:x"})
	if err == nil {
		t.Error("saveSaaSUserPath with unknown provider should error")
	}
}

// TestEnsureSaaSUser_CanonicalAdminGetsUnlimitedQuota: when a hub admin logs in
// with a canonical identity ("github:clubanderson"), ensureSaaSUser must assign
// quota=-1 (unlimited). A failure here means the admin loses unlimited quota on
// the multi-provider path and hits rate limits on their own hub.
func TestEnsureSaaSUser_CanonicalAdminGetsUnlimitedQuota(t *testing.T) {
	dir := withTempUsersDir(t)
	_ = dir

	// Default admin is github:clubanderson. Bare and canonical forms should
	// both produce quota=-1.
	for _, id := range []string{"clubanderson", "github:clubanderson"} {
		u := ensureSaaSUser(id)
		if u == nil {
			t.Fatalf("ensureSaaSUser(%q) returned nil", id)
		}
		if u.SaaSQuota != -1 {
			t.Errorf("ensureSaaSUser(%q).SaaSQuota = %d, want -1 (unlimited for admin)", id, u.SaaSQuota)
		}
	}

	// A non-admin user gets quota=0.
	u := ensureSaaSUser("someuser")
	if u == nil {
		t.Fatal("ensureSaaSUser(someuser) returned nil")
	}
	if u.SaaSQuota != 0 {
		t.Errorf("ensureSaaSUser(someuser).SaaSQuota = %d, want 0", u.SaaSQuota)
	}
}

// TestEnsureSaaSUser_ExistingUserUpdatesLastLogin: when ensureSaaSUser finds a
// pre-existing record, it updates LastLogin and saves. A regression here would
// silently stop tracking login recency.
func TestEnsureSaaSUser_ExistingUserUpdatesLastLogin(t *testing.T) {
	dir := withTempUsersDir(t)
	writeRawUser(t, dir, "existing.json", SaaSUser{
		GitHubUsername: "existing",
		CreatedAt:      "2024-01-01T00:00:00Z",
		LastLogin:      "2024-01-01T00:00:00Z",
		Hives:          map[string]string{},
	})

	u := ensureSaaSUser("existing")
	if u == nil {
		t.Fatal("ensureSaaSUser(existing) returned nil")
	}
	if u.LastLogin == "2024-01-01T00:00:00Z" {
		t.Error("ensureSaaSUser should update LastLogin, but it was not changed")
	}
	if u.CreatedAt != "2024-01-01T00:00:00Z" {
		t.Errorf("ensureSaaSUser should preserve CreatedAt, got %q", u.CreatedAt)
	}
}

// TestListAllSaaSUsers_DuplicateFiles: if both "alice.json" (legacy) AND
// "github.alice.json" (canonical) exist on disk — possible during a partial
// migration — listAllSaaSUsers returns entries for both filenames. This test
// documents the current behavior so a future dedup does not regress silently.
func TestListAllSaaSUsers_DuplicateFiles(t *testing.T) {
	dir := withTempUsersDir(t)
	writeRawUser(t, dir, "alice.json", SaaSUser{GitHubUsername: "alice"})
	writeRawUser(t, dir, "github.alice.json", SaaSUser{GitHubUsername: "github:alice"})

	users := listAllSaaSUsers()
	// Both files are enumerated. The legacy "alice.json" loads as "alice", the
	// canonical "github.alice.json" decodes to "github:alice" and also loads
	// (the dual-read finds the canonical file). Count how many we get.
	count := 0
	for _, u := range users {
		if u.GitHubUsername == "alice" || u.GitHubUsername == "github:alice" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("expected both legacy and canonical entries enumerated, got %d matching users out of %v", count, users)
	}
}

// TestReadSaaSUserFile_NonExistentReturnsError: readSaaSUserFile for a user with
// no file on disk returns an error (not nil data).
func TestReadSaaSUserFile_NonExistentReturnsError(t *testing.T) {
	_ = withTempUsersDir(t)
	data, err := readSaaSUserFile("no-such-user")
	if err == nil {
		t.Errorf("readSaaSUserFile(no-such-user) returned nil error with data=%v", data)
	}
}

// TestEnsureSaaSUser_EnvOverrideAdminQuota: when HIVE_HUB_ADMINS overrides the
// admin set, ensureSaaSUser gives unlimited quota to the NEW admin and quota=0
// to the old default admin (since they are no longer in the set).
func TestEnsureSaaSUser_EnvOverrideAdminQuota(t *testing.T) {
	_ = withTempUsersDir(t)
	t.Setenv("HIVE_HUB_ADMINS", "github:newadmin")

	u := ensureSaaSUser("newadmin")
	if u == nil {
		t.Fatal("ensureSaaSUser(newadmin) returned nil")
	}
	if u.SaaSQuota != -1 {
		t.Errorf("ensureSaaSUser(newadmin).SaaSQuota = %d, want -1 (new admin)", u.SaaSQuota)
	}

	// Old default admin is no longer in the set → quota=0.
	u2 := ensureSaaSUser("clubanderson")
	if u2 == nil {
		t.Fatal("ensureSaaSUser(clubanderson) returned nil")
	}
	if u2.SaaSQuota != 0 {
		t.Errorf("ensureSaaSUser(clubanderson).SaaSQuota = %d, want 0 (not admin when overridden)", u2.SaaSQuota)
	}
}

// TestCanonicalEqual_OwnerMatchSites: the owner-match change at ~10 HTTP handler
// sites replaced EqualFold/== with canonicalEqual. This test asserts the specific
// identity pairs those sites will encounter: a bare Owner field stored on-disk
// matched against the authenticated canonical identity from the session cookie.
func TestCanonicalEqual_OwnerMatchSites(t *testing.T) {
	cases := []struct {
		owner, authenticated string
		want                 bool
	}{
		// Legacy owner, canonical auth — the core migration shim.
		{"clubanderson", "github:clubanderson", true},
		// Legacy owner, bare auth — pre-migration path, still works.
		{"clubanderson", "clubanderson", true},
		// Case-insensitive GitHub login.
		{"ClubAnderson", "github:clubanderson", true},
		// SECURITY: cross-provider owner spoof must fail.
		{"clubanderson", "google:clubanderson", false},
		{"clubanderson", "ibmid:clubanderson", false},
		// Non-GitHub owner matches its own canonical form.
		{"google:1078", "google:1078", true},
		// Non-GitHub owner does NOT match a different provider.
		{"google:1078", "ibmid:1078", false},
	}
	for _, c := range cases {
		got := canonicalEqual(c.owner, c.authenticated)
		if got != c.want {
			t.Errorf("canonicalEqual(%q, %q) = %v, want %v", c.owner, c.authenticated, got, c.want)
		}
	}
}
