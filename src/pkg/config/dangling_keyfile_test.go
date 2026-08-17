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
