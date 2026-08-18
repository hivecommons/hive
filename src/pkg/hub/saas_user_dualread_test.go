package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withTempUsersDir points saasUsersDir at a fresh temp dir and restores it.
func withTempUsersDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "users")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := saasUsersDir
	saasUsersDir = dir
	t.Cleanup(func() { saasUsersDir = orig })
	return dir
}

func writeRawUser(t *testing.T, dir, filename string, u SaaSUser) {
	t.Helper()
	data, _ := json.MarshalIndent(&u, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDualRead_LegacyAndCanonical: a pre-migration legacy file "<login>.json" and
// a canonical "<provider>.<subject>.json" both resolve through loadSaaSUser, by
// bare login, by github: canonical, and by non-github canonical.
func TestDualRead_LegacyAndCanonical(t *testing.T) {
	dir := withTempUsersDir(t)

	// Legacy GitHub user on disk as "alice.json" (no provider prefix).
	writeRawUser(t, dir, "alice.json", SaaSUser{GitHubUsername: "alice"})
	// Canonical Google user on disk as "google.1078.json".
	writeRawUser(t, dir, "google.1078.json", SaaSUser{GitHubUsername: "google:1078"})

	// Legacy user resolves by bare login AND by github: canonical.
	if u := loadSaaSUser("alice"); u == nil || u.GitHubUsername != "alice" {
		t.Errorf("loadSaaSUser(alice) = %+v", u)
	}
	if u := loadSaaSUser("github:alice"); u == nil || u.GitHubUsername != "alice" {
		t.Errorf("loadSaaSUser(github:alice) should resolve the legacy file, got %+v", u)
	}
	// Google user resolves by its canonical id (and NOT via a legacy path).
	if u := loadSaaSUser("google:1078"); u == nil || u.GitHubUsername != "google:1078" {
		t.Errorf("loadSaaSUser(google:1078) = %+v", u)
	}
	// A non-existent user is nil.
	if u := loadSaaSUser("nobody"); u != nil {
		t.Errorf("loadSaaSUser(nobody) = %+v, want nil", u)
	}
}

// TestSave_LegacyStaysInPlace_CanonicalWritesCanonical: saving a GitHub user
// updates the legacy "<login>.json" (no rename/migration), while a non-GitHub
// user writes the canonical filename.
func TestSave_LegacyStaysInPlace_CanonicalWritesCanonical(t *testing.T) {
	dir := withTempUsersDir(t)

	// GitHub user → legacy file, in place.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "bob", Hives: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bob.json")); err != nil {
		t.Errorf("expected legacy bob.json, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "github.bob.json")); err == nil {
		t.Error("GitHub user must NOT create a canonical github.bob.json (no migration on save)")
	}

	// Google user → canonical file.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "google:2222", Hives: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "google.2222.json")); err != nil {
		t.Errorf("expected canonical google.2222.json, got %v", err)
	}
	// And it round-trips through load.
	if u := loadSaaSUser("google:2222"); u == nil || u.GitHubUsername != "google:2222" {
		t.Errorf("round-trip loadSaaSUser(google:2222) = %+v", u)
	}
}

// TestListAllSaaSUsers_MixedFilenames: enumeration decodes canonical filenames
// and leaves legacy ones as-is, returning both users.
func TestListAllSaaSUsers_MixedFilenames(t *testing.T) {
	dir := withTempUsersDir(t)
	writeRawUser(t, dir, "carol.json", SaaSUser{GitHubUsername: "carol"})
	writeRawUser(t, dir, "google.3333.json", SaaSUser{GitHubUsername: "google:3333"})

	users := listAllSaaSUsers()
	got := map[string]bool{}
	for _, u := range users {
		got[u.GitHubUsername] = true
	}
	if !got["carol"] || !got["google:3333"] {
		t.Errorf("listAllSaaSUsers missing an entry: %+v", got)
	}
}
