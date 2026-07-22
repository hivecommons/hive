package main

import "testing"

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
