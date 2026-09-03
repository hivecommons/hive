package advisory

import "testing"

// Direct branch coverage for splitFilePathRef and isFilePathRef — the parsers
// that decide which finding refs VerifyFindingPaths checks for existence and
// what path it checks. snapshot_test.go exercises them only through the happy
// VerifyFindingPaths flow; these tests pin the edge branches so a parser
// regression shows up here rather than as a wrongly-stale (or wrongly-live)
// finding in a posted digest.

func TestSplitFilePathRef(t *testing.T) {
	cases := []struct {
		name, ref, want string
	}{
		{"path with line suffix", "pkg/mint/tokenreview.go:365", "pkg/mint/tokenreview.go"},
		{"path without colon", "docs/install.md", "docs/install.md"},
		{"non-numeric suffix kept", "cmd/hive:main", "cmd/hive:main"},
		{"leading colon kept", ":123", ":123"},
		{"empty ref", "", ""},
		{"trailing colon kept", "pkg/file.go:", "pkg/file.go:"},
		{"only last numeric segment stripped", "a:1:2", "a:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitFilePathRef(tc.ref); got != tc.want {
				t.Errorf("splitFilePathRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestIsFilePathRef(t *testing.T) {
	cases := []struct {
		name, ref string
		want      bool
	}{
		{"empty is not a path", "", false},
		{"gh-number ref", "gh-123", false},
		{"repo#number ref", "hive#123", false},
		{"owner/repo#number ref", "hivecommons/hive#123", false},
		{"plain file path", "docs/install.md", true},
		{"path with line", "pkg/advisory/advisory.go:812", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFilePathRef(tc.ref); got != tc.want {
				t.Errorf("isFilePathRef(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
