package policies

import (
	"strings"
	"testing"
)

// The harness nudges any agent that polls CI repeatedly within one kick
// (#13505 caught a reviewer doing so). The reviewer lane must carry the same
// do-not-poll guidance as the other lanes so the nudge is a backstop only.
func TestReviewerLaneForbidsCIPolling(t *testing.T) {
	data, err := DefaultPolicies.ReadFile("defaults/reviewer-lane.md")
	if err != nil {
		t.Fatalf("read embedded reviewer-lane policy: %v", err)
	}
	policy := string(data)
	for _, want := range []string{"Do Not Wait for CI", "gh run watch", "gh pr checks"} {
		if !strings.Contains(policy, want) {
			t.Errorf("reviewer-lane policy lacks %q; reviewers will poll CI (#13505)", want)
		}
	}
}
