package hub

import (
	"regexp"
	"testing"
)

// TestRepoDisplayLine pins the doubling-safe derivation of the My Hives
// name-cell second line (and the ownership-timeline audit string) from a hive's
// org + primary repo.
//
// The defect this guards: a github.io / GitHub Enterprise hive was recorded
// with a HOST in the org field and a full owner/repo path in primaryRepo, so
// the old org + "/" + primaryRepo join rendered the owner twice under a fake
// org — "castrojo.github.io/castrojo/endusers" instead of "castrojo/endusers".
func TestRepoDisplayLine(t *testing.T) {
	cases := []struct {
		name        string
		org         string
		primaryRepo string
		want        string
	}{
		{
			// Normal hive: bare repo name, real org -> qualify with org.
			name:        "normal hive qualifies bare repo with org",
			org:         "myorg",
			primaryRepo: "repo",
			want:        "myorg/repo",
		},
		{
			// GHE / github.io hive: org is a mis-parsed HOST and primaryRepo is
			// already owner/repo. The owner must NOT be doubled, and the host
			// must NOT appear as if it were the org.
			name:        "ghe hive with owner/repo primary is not doubled",
			org:         "castrojo.github.io",
			primaryRepo: "castrojo/endusers",
			want:        "castrojo/endusers",
		},
		{
			// A legitimate owner/repo primary with a real org still must not
			// double: the path is already complete.
			name:        "owner/repo primary with real org is not doubled",
			org:         "myorg",
			primaryRepo: "myorg/repo",
			want:        "myorg/repo",
		},
		{
			name:        "org only leaves no dangling slash",
			org:         "myorg",
			primaryRepo: "",
			want:        "myorg",
		},
		{
			name:        "repo only leaves no dangling slash",
			org:         "",
			primaryRepo: "owner/repo",
			want:        "owner/repo",
		},
		{
			name:        "neither yields empty",
			org:         "",
			primaryRepo: "",
			want:        "",
		},
		{
			name:        "whitespace is trimmed",
			org:         "  myorg  ",
			primaryRepo: "  repo  ",
			want:        "myorg/repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoDisplayLine(tc.org, tc.primaryRepo); got != tc.want {
				t.Errorf("repoDisplayLine(%q, %q) = %q, want %q",
					tc.org, tc.primaryRepo, got, tc.want)
			}
		})
	}
}

// TestHiveLabelJSMirrorsDoublingGuard asserts the dashboard JS hiveLabel keeps
// the same doubling guard as repoDisplayLine: a primaryRepo that already
// contains a slash is used as-is rather than being prefixed with the org. If
// the JS reverts to a naive org + '/' + repo join, the github.io/GHE row will
// once again render "host/owner/repo" and this test fails.
func TestHiveLabelJSMirrorsDoublingGuard(t *testing.T) {
	// The JS must branch on an already-qualified repo path before qualifying
	// with the org.
	guard := regexp.MustCompile(`fallbackRepo\.indexOf\('/'\) !== -1`)
	if !guard.MatchString(dashboardHTML) {
		t.Error("dashboard JS hiveLabel is missing the owner-doubling guard " +
			"(fallbackRepo.indexOf('/') !== -1); a github.io/GHE hive whose " +
			"primaryRepo is already owner/repo will render host/owner/repo again")
	}
}
