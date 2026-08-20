package hub

import (
	"strings"
	"testing"
)

// Regression tests for the hub half of #4299: the allowlist the hub delivers
// to a direct-route spoke (provisioning env + heartbeat) must carry canonical
// provider identities INTACT. The old hive-id sanitize() lower-cased and
// stripped every character outside [a-z0-9-], so "ibmid:310002BM0V" was
// delivered as "ibmid310002bm0v" — an entry that could never match the user's
// real identity at login, silently locking a granted non-GitHub user out.

func TestAuthorizedUsersForHivePreservesCanonicalIdentities(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const hiveID = "hosted-canon"
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "ibmid:310002BM0V",
		Hives:          map[string]string{hiveID: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "dwaddington",
		Hives:          map[string]string{hiveID: "read-write"},
	}); err != nil {
		t.Fatal(err)
	}

	got := authorizedUsersForHive(&SaaSHive{ID: hiveID, Owner: "clubanderson"})
	for _, want := range []string{"clubanderson:owner", "ibmid:310002BM0V:owner", "dwaddington:read-write"} {
		if !strings.Contains(got, want) {
			t.Errorf("allowlist %q missing entry %q", got, want)
		}
	}
	if strings.Contains(got, "ibmid310002bm0v") {
		t.Errorf("allowlist %q carries the corrupted (stripped) ibmid entry", got)
	}
}

func TestSanitizeAllowlistIdentity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dwaddington", "dwaddington"},
		{"github:dwaddington", "github:dwaddington"},
		{"ibmid:310002BM0V", "ibmid:310002BM0V"}, // case and colon preserved
		{"google:107.8_x-1", "google:107.8_x-1"},
		{"evil,entry:owner", "evilentry:owner"}, // list separator stripped
		{"sp ace\"'$;`", "space"},               // env-hostile bytes stripped
	}
	for _, c := range cases {
		if got := sanitizeAllowlistIdentity(c.in); got != c.want {
			t.Errorf("sanitizeAllowlistIdentity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
