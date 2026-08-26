package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDanglingKeyFile(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
		// setup returns a cleanup func; nil means no setup needed.
		setup func(t *testing.T) string
	}{
		{
			name: "empty_keyfile",
			key:  "",
			want: false,
		},
		{
			name: "non_pvc_path",
			key:  "/etc/keys/app.pem",
			want: false,
		},
		{
			name: "short_path",
			key:  "/dat",
			want: false,
		},
		{
			name: "pvc_path_file_missing",
			key:  "/data/missing-key-file-that-does-not-exist.pem",
			want: true,
		},
		{
			name: "pvc_path_file_exists",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				// TempDir is under /tmp, not /data — create a file under /data
				// by using a symlink trick won't work; instead just test with
				// a real /data path if writable, otherwise skip.
				p := filepath.Join(dir, "key.pem")
				if err := os.WriteFile(p, []byte("pem"), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want: false, // overridden below — file exists but path may not start with /data/
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key
			want := tc.want
			if tc.setup != nil {
				key = tc.setup(t)
				// If the temp path doesn't start with /data/, DanglingKeyFile
				// returns false (non-PVC path), which is still correct behavior.
				if len(key) < 6 || key[:6] != "/data/" {
					want = false
				}
			}
			gh := GitHubConfig{KeyFile: key}
			if got := DanglingKeyFile(gh); got != want {
				t.Errorf("DanglingKeyFile(KeyFile=%q) = %v, want %v", key, got, want)
			}
		})
	}
}

func TestDanglingKeyFile_RealDataPath(t *testing.T) {
	// If we can write to /data, test the exists-under-/data branch directly.
	dir := "/data/test-dangling-keyfile-" + t.Name()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create %s: %v", dir, err)
	}
	defer os.RemoveAll(dir)

	existing := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(existing, []byte("pem-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// File exists under /data → not dangling.
	if got := DanglingKeyFile(GitHubConfig{KeyFile: existing}); got {
		t.Errorf("existing key under /data should not be dangling")
	}

	// File does not exist under /data → dangling.
	missing := filepath.Join(dir, "gone.pem")
	if got := DanglingKeyFile(GitHubConfig{KeyFile: missing}); !got {
		t.Errorf("missing key under /data should be dangling")
	}
}

func TestContainsFold(t *testing.T) {
	tests := []struct {
		s, sub string
		want   bool
	}{
		{"", "", true},
		{"abc", "", true},
		{"", "a", false},
		{"hello world", "WORLD", true},
		{"HELLO", "hello", true},
		{"abc", "abcd", false},
		{"FooBarBaz", "obar", true},
		{"abc", "xyz", false},
	}
	for _, tc := range tests {
		if got := containsFold(tc.s, tc.sub); got != tc.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", tc.s, tc.sub, got, tc.want)
		}
	}
}

// TestDanglingKeyFile_SecretsMount covers the prefix this predicate used to skip
// (#4368).
//
// The provisioning template seeded key_file=/secrets/gh-app-key.pem for every
// App-using hive but created that Secret entry only for hives provisioned with
// an inline private key. Four hives shipped naming a file that would never
// exist — and because the check looked only at /data/, the detector written for
// exactly this symptom returned false for all four, so nothing surfaced until
// the first App call failed.
func TestDanglingKeyFile_SecretsMount(t *testing.T) {
	// A missing file under the provisioning mount is dangling. Named uniquely so
	// the assertion cannot be perturbed by whatever the host has at /secrets.
	missing := "/secrets/missing-4368-" + t.Name() + ".pem"
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Skipf("%s unexpectedly exists on this host", missing)
	}
	if !DanglingKeyFile(GitHubConfig{KeyFile: missing}) {
		t.Errorf("a missing key under /secrets/ must be dangling; this is the #4368 shape")
	}

	// An operator path outside both delivery locations is still left alone: they
	// are entitled to keep a key somewhere this build has never heard of.
	if DanglingKeyFile(GitHubConfig{KeyFile: "/opt/operator/custom-" + t.Name() + ".pem"}) {
		t.Error("a path outside /data/ and /secrets/ must not be reported as dangling")
	}

	// A short path that merely shares a prefix character must not panic or match.
	for _, short := range []string{"/sec", "/secrets", "/dat", ""} {
		if DanglingKeyFile(GitHubConfig{KeyFile: short}) {
			t.Errorf("DanglingKeyFile(%q) = true, want false", short)
		}
	}
}

// TestDanglingKeyFile_SecretsMountExisting proves the widened prefix does not
// invent a fault when the projected Secret entry IS present — the healthy shape
// for a hive provisioned with an inline key.
func TestDanglingKeyFile_SecretsMountExisting(t *testing.T) {
	dir := "/secrets/test-dangling-4368-" + t.Name()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create %s: %v", dir, err)
	}
	defer os.RemoveAll(dir)

	existing := filepath.Join(dir, "gh-app-key.pem")
	if err := os.WriteFile(existing, []byte("pem-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if DanglingKeyFile(GitHubConfig{KeyFile: existing}) {
		t.Error("an existing key under /secrets must not be dangling")
	}
}

// TestHasPrefix covers the helper DanglingKeyFile's prefix test is built on.
// The two mount-point tests above can only assert the exists-branch on a host
// where /data or /secrets is writable, and skip everywhere else — so the prefix
// logic itself is pinned here, where nothing can skip it.
func TestHasPrefix(t *testing.T) {
	tests := []struct {
		s, prefix string
		want      bool
	}{
		{"/data/gh-app-key.pem", "/data/", true},
		{"/secrets/gh-app-key.pem", "/secrets/", true},
		{"/secrets/gh-app-key.pem", "/data/", false},
		{"/data/gh-app-key.pem", "/secrets/", false},
		{"/opt/keys/app.pem", "/data/", false},
		// Shorter than the prefix: must be false, not a slice panic.
		{"/dat", "/data/", false},
		{"/secrets", "/secrets/", false},
		{"", "/data/", false},
		{"anything", "", true},
	}
	for _, tc := range tests {
		if got := hasPrefix(tc.s, tc.prefix); got != tc.want {
			t.Errorf("hasPrefix(%q, %q) = %v, want %v", tc.s, tc.prefix, got, tc.want)
		}
	}
}
