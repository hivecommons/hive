package main

import (
	"os"
	"testing"
)

func TestManagedRoleBeadStoreRequiresExactRolePath(t *testing.T) {
	for _, test := range []struct {
		name string
		role string
		dir  string
		want bool
	}{
		{name: "exact", role: "quality", dir: "/data/beads/quality", want: true},
		{name: "other role", role: "quality", dir: "/data/beads/scanner"},
		{name: "nested", role: "quality", dir: "/data/beads/quality/nested"},
		{name: "custom", role: "quality", dir: "/tmp/quality"},
		{name: "current directory", role: ".", dir: "/data/beads"},
		{name: "parent directory", role: "..", dir: "/data"},
		{name: "invalid role", role: "../quality", dir: "/data/quality"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isManagedRoleBeadStore(test.role, test.dir); got != test.want {
				t.Fatalf("isManagedRoleBeadStore(%q, %q) = %v, want %v", test.role, test.dir, got, test.want)
			}
		})
	}
}

func TestManagedRoleBeadStoreRemainsGroupSharedAcrossReopen(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix role-store mode semantics")
	}
	root := t.TempDir()
	dir := root + string(os.PathSeparator) + "quality"
	store, err := openRoleBeadStoreUnderRoot("quality", dir, root)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("managed role store is nil")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o770 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("managed role store mode = %v, want setgid 0770", info.Mode())
	}
	if _, err := openRoleBeadStoreUnderRoot("quality", dir, root); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o770 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("reopened managed role store mode = %v, want setgid 0770", info.Mode())
	}
}
