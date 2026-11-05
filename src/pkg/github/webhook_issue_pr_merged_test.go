package github

import (
	"encoding/json"
	"testing"
	"time"
)

// TestWebhookItemHintIssueCommentOnMergedPR (hivecommons/hive#16658):
// an issues / issue_comment delivery on a pull request reads the merge
// from issue.pull_request.merged_at, so a bot's comment on a PR that just
// merged is a merged hint, not a closed-unmerged one; an open PR's null
// merged_at and a plain issue stay unmerged.
func TestWebhookItemHintIssueCommentOnMergedPR(t *testing.T) {
	at := time.Date(2026, 10, 20, 19, 35, 26, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		body   string
		isPR   bool
		merged bool
	}{
		{"merged PR", `{"issue":{"number":16644,"state":"closed","pull_request":{"url":"u","merged_at":"2026-10-20T19:35:26Z"}}}`, true, true},
		{"open PR", `{"issue":{"number":16644,"state":"open","pull_request":{"url":"u","merged_at":null}}}`, true, false},
		{"closed unmerged PR", `{"issue":{"number":16644,"state":"closed","pull_request":{"url":"u"}}}`, true, false},
		{"issue", `{"issue":{"number":16644,"state":"closed"}}`, false, false},
	} {
		var p webhookEventPayload
		if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
			t.Fatal(err)
		}
		for _, event := range []string{"issue_comment", "issues"} {
			h := webhookItemHint(event, p)
			if h == nil || h.Number != 16644 || h.IsPR != tc.isPR || h.Merged != tc.merged {
				t.Errorf("%s/%s: hint = %+v, want is_pr=%v merged=%v", tc.name, event, h, tc.isPR, tc.merged)
			}
		}
	}
	raw := json.RawMessage(`{"merged_at":"` + at.Format(time.RFC3339) + `"}`)
	if !webhookIssuePullRequestMerged(&raw) || webhookIssuePullRequestMerged(nil) {
		t.Fatal("webhookIssuePullRequestMerged: want true for a merge instant, false for no object")
	}
}
