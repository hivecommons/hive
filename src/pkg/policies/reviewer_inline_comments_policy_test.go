package policies

import (
	"strings"
	"testing"
)

// TestReviewerQueuePolicyAnchorsFindings pins hivecommons/hive#18531: the
// reviewer posts line findings as inline threads through --comments-file,
// all in one review, instead of one long comment.
func TestReviewerQueuePolicyAnchorsFindings(t *testing.T) {
	data, err := DefaultPolicies.ReadFile("defaults/reviewer-queue.md")
	if err != nil {
		t.Fatalf("read reviewer-queue.md: %v", err)
	}
	body := string(data)
	for _, want := range []string{"--comments-file", `"path"`, `"line"`, `"side": "LEFT"`, "**one** review", "Never\n  post findings one at a time"} {
		if !strings.Contains(body, want) {
			t.Errorf("reviewer-queue.md must say %q", want)
		}
	}
}
