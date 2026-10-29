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

// Agents never watch CI: failures reach them out of band, from the webhook
// (hivecommons/hive#15291) or the sweep. The reviewer lane keeps its
// mandatory "Do Not Wait for CI" section, heading included.
func TestReviewerLaneKeepsDoNotWaitForCISection(t *testing.T) {
	data, err := DefaultPolicies.ReadFile("defaults/reviewer-lane.md")
	if err != nil {
		t.Fatalf("read embedded reviewer-lane policy: %v", err)
	}
	const heading = "## After You Push: Do Not Wait for CI (MANDATORY)"
	if !strings.Contains(string(data), heading) {
		t.Fatalf("reviewer-lane policy lacks %q (#15291)", heading)
	}
}
