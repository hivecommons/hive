package github

import "testing"

// MergeVerdictKey is the contract between the governor (which records
// verdicts) and the dashboard (which looks them up): both must derive the
// same key from the same PullRequest, spelled exactly as the enumeration
// spelled Repo (#7478). Pin the format for both spellings.
func TestMergeVerdictKey(t *testing.T) {
	cases := []struct {
		name string
		pr   PullRequest
		want string
	}{
		{"bare repo", PullRequest{Repo: "hive", Number: 42}, "hive#42"},
		{"owner/name repo", PullRequest{Repo: "hivecommons/hive", Number: 7478}, "hivecommons/hive#7478"},
		{"zero value", PullRequest{}, "#0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeVerdictKey(tc.pr); got != tc.want {
				t.Errorf("MergeVerdictKey(%q, %d) = %q, want %q", tc.pr.Repo, tc.pr.Number, got, tc.want)
			}
		})
	}
}
