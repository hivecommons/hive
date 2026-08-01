package dashboard

import "testing"

// TestAssignedToOthers exercises the #2357 skip-assigned guard: an issue is
// skipped only when it has assignees and none of them is the contributor.
func TestAssignedToOthers(t *testing.T) {
	const me = "octocat"

	cases := []struct {
		name      string
		assignees []string
		want      bool
	}{
		{"unassigned nil", nil, false},
		{"unassigned empty", []string{}, false},
		{"assigned to someone else", []string{"alice"}, true},
		{"assigned to multiple others", []string{"alice", "bob"}, true},
		{"assigned to me only", []string{me}, false},
		{"assigned to me and others", []string{"alice", me}, false},
		{"assigned to me case-insensitive", []string{"OctoCat"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assignedToOthers(tc.assignees, me); got != tc.want {
				t.Errorf("assignedToOthers(%v, %q) = %v, want %v", tc.assignees, me, got, tc.want)
			}
		})
	}
}
