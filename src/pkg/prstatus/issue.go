package prstatus

import (
	"fmt"
	"strings"
)

// Section keys of the issue-side status card (github.notifications.
// attention_pilot, hivecommons/hive#16358). Each replaces a routine issue
// note that used to be its own comment; as with the PR keys, the legacy
// marker stays inside the section text so marker readers still find it.
const (
	// KeyIssueClose is close/auto-close narration: "Fixed by #N",
	// awaiting-confirmation and reopened-after-merge notes
	// (<!-- hive:close-on-merge -->). The native close event is what
	// notifies; the card keeps the /reopen guidance.
	KeyIssueClose = "issue-close"
	// KeyIssueReopened records that a person reopened the issue after a
	// merge, so Hive will not close it for that PR again.
	KeyIssueReopened = "issue-reopened"
	// KeyIssuePartialPrefix + PR number is the partial-work/remainder note
	// a non-closing merged PR leaves on the issue.
	KeyIssuePartialPrefix = "issue-partial:"
	// KeyIssuePRLink is the stage note naming the PR that covers the issue.
	KeyIssuePRLink = "issue-pr-link"
	// KeyIssueAttentionPrefix + cause is a newly required action recorded
	// on the card; IssueAttentionMarker tags the one explicit comment that
	// announces it.
	KeyIssueAttentionPrefix = "attention:"
)

// IssueAttentionMarker tags the one explicit attention comment posted for
// cause, so a later pass that sees it never posts a second.
func IssueAttentionMarker(cause string) string {
	return fmt.Sprintf("<!-- hive:attention cause=%s -->", strings.TrimSpace(cause))
}

// KeyIssuePartial is the partial-work section key for pr.
func KeyIssuePartial(pr int) string {
	return fmt.Sprintf("%s%d", KeyIssuePartialPrefix, pr)
}
