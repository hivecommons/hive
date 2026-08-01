package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagedRoleStorePathIsOneExactChild(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/data/beads/quality", want: true},
		{path: "/data/beads/scanner", want: true},
		{path: "/data/beads"},
		{path: "/data/beads/quality/nested"},
		{path: "/data/other"},
		{path: "/tmp/store"},
	} {
		if got := isManagedRoleStore(test.path); got != test.want {
			t.Fatalf("isManagedRoleStore(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestManagedRoleStoreDoesNotRequireEnvironmentHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shared role-store mode requires Unix permission bits")
	}
	t.Setenv("HIVE_SHARED_ROLE_BEADS", "")
	previousRoot := managedRoleStoreRoot
	managedRoleStoreRoot = t.TempDir()
	t.Cleanup(func() { managedRoleStoreRoot = previousRoot })

	dir := filepath.Join(managedRoleStoreRoot, "quality")
	if !usesSharedStore(dir) {
		t.Fatalf("managed role store %q did not select shared mode without an environment hint", dir)
	}
	if _, err := openStoreAt(dir); err != nil {
		t.Fatalf("open managed role store without environment hint: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat managed role store: %v", err)
	}
	if got, want := info.Mode()&(os.ModePerm|os.ModeSetgid), os.FileMode(0o770)|os.ModeSetgid; got != want {
		t.Fatalf("managed role store mode = %v, want %v", got, want)
	}
}

func TestPrivateStorePathRemainsPrivate(t *testing.T) {
	if usesSharedStore(t.TempDir()) {
		t.Fatal("ordinary private store path selected shared mode")
	}
}
