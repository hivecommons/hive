package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/gfi"
)

// Tracking / archive issues (hivecommons/hive#14613): the `hive/tracking`
// label, the `<!-- hive:tracking -->` body marker or the literal "Keep this
// issue open" sentence mark an issue the hive must recognise and leave
// alone. The vocabulary lives in pkg/gfi (stdlib-only, shared with the
// good-first-issue picker); this file is the pkg/github side: the
// enumeration bucket, the close guard and the PR-link nudge.

// trackingFilterReason is the enumeration's filtered-bucket reason for a
// tracking issue; the repo card and the stage engine read it verbatim.
const trackingFilterReason = gfi.SkipTracking

// ErrTrackingIssue is returned by CloseIssue for a tracking/archive issue
// unless the caller set IssueCloseOptions.AllowTrackingIssue.
var ErrTrackingIssue = errors.New("issue is a tracking/archive issue (hive/tracking label, <!-- hive:tracking --> marker or \"Keep this issue open\" in the body); the hive does not close it")

// trackingIssueLinkMarkerPrefix opens the one-line PR nudge the hive posts
// when a PR body says Closes/Fixes #N for a tracking issue.
const trackingIssueLinkMarkerPrefix = "<!-- hive:tracking-issue-link"

// isTrackingIssue reports whether issue is a tracking/archive issue and why.
func isTrackingIssue(issue *gh.Issue) (string, bool) {
	if issue == nil {
		return "", false
	}
	return gfi.TrackingReason(issueLabelNames(issue.Labels), issue.GetBody())
}

// trackingIssueLinkMarker is the per-issue idempotency marker for the nudge.
func trackingIssueLinkMarker(issue int) string {
	return fmt.Sprintf("%s issue=%d -->", trackingIssueLinkMarkerPrefix, issue)
}

// trackingIssueLinkComment is the one-line reviewer nudge.
func trackingIssueLinkComment(issue int, why string) string {
	return fmt.Sprintf("%s\nNote for reviewers: #%d is a persistent tracking/archive issue (%s), not a bug or feature request. The closing keyword for it in this PR's body will be ignored by the hive and should be `Refs #%d`; merging this PR does not resolve that issue.",
		trackingIssueLinkMarker(issue), issue, trackingIssueLinkWhy(why), issue)
}

func trackingIssueLinkWhy(why string) string {
	switch why {
	case "label":
		return "`" + gfi.TrackingLabel + "` label"
	case "marker":
		return "`" + gfi.TrackingMarker + "` marker"
	case "keep-open":
		return "its body says \"" + gfi.TrackingKeepOpenSentence + "\""
	}
	return "tracking issue"
}

// NudgeTrackingIssueClosingRefs posts ONE idempotent comment on the PR for
// each same-repo issue its body closes that is a tracking/archive issue, so
// the reviewer (and the author) learn before merge that the keyword will not
// resolve anything. It never edits the body: a contributor's PR body is
// theirs, and the hive-opened path rewrites `Closes` to `Refs` before the PR
// exists (incompleteIssueReason). Best-effort: errors are logged, never
// returned, and a failed comment is retried on the next call. Returns the
// tracking issue numbers found.
func (c *Client) NudgeTrackingIssueClosingRefs(ctx context.Context, repo string, prNumber int, body string, logger *slog.Logger) []int {
	if c == nil || c.client == nil || prNumber <= 0 || strings.TrimSpace(body) == "" {
		return nil
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" {
		return nil
	}
	if logger == nil {
		logger = c.logger
	}
	var found []int
	for issueNumber := range closingIssueRefs(body, owner, name) {
		issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:tracking_issue_link"), owner, name, issueNumber)
		if err != nil || issue == nil || issue.IsPullRequest() {
			continue
		}
		why, ok := isTrackingIssue(issue)
		if !ok {
			continue
		}
		found = append(found, issueNumber)
		marker := trackingIssueLinkMarker(issueNumber)
		if present, err := hasMarkerComment(ctx, c.client, owner, name, prNumber, marker); err != nil || present {
			if err != nil && logger != nil {
				logger.Warn("tracking-issue nudge: reading PR comments failed; will retry", "repo", repo, "pr", prNumber, "issue", issueNumber, "error", err)
			}
			continue
		}
		if err := c.CreateIssueComment(WithRESTCaller(ctx, "hive:tracking_issue_link"), owner+"/"+name, prNumber, trackingIssueLinkComment(issueNumber, why)); err != nil {
			if logger != nil {
				logger.Warn("tracking-issue nudge: posting PR comment failed; will retry", "repo", repo, "pr", prNumber, "issue", issueNumber, "error", err)
			}
			continue
		}
		if logger != nil {
			logger.Info("tracking-issue nudge: PR closes a tracking issue; told the reviewer the keyword is ignored", "repo", repo, "pr", prNumber, "issue", issueNumber, "why", why)
		}
	}
	return found
}
