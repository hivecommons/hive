package main

import "testing"

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
