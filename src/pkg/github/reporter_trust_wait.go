package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const reporterTrustWaitMarkerPrefix = "<!-- hive:reporter-trust-wait "

func reporterTrustWaitMarker(repo, addedLabel string) string {
	if strings.TrimSpace(addedLabel) == "" {
		return fmt.Sprintf("%srepo=%s -->", reporterTrustWaitMarkerPrefix, repo)
	}
	return fmt.Sprintf("%srepo=%s added-label=%s -->", reporterTrustWaitMarkerPrefix, repo, url.QueryEscape(strings.TrimSpace(addedLabel)))
}

func reporterTrustRequiredLabelsForNotice(ra ReporterAdmitter) []string {
	if nc, ok := ra.(ReporterTrustNoticeConfig); ok {
		if labels := trimNonEmpty(nc.ReporterTrustRequiredLabelsForNotice()); len(labels) > 0 {
			return labels
		}
	}
	return []string{"triage/accepted"}
}

func reporterTrustTrustedAssociationsForNotice(ra ReporterAdmitter) []string {
	if nc, ok := ra.(ReporterTrustNoticeConfig); ok {
		if labels := trimNonEmpty(nc.ReporterTrustTrustedAssociationsForNotice()); len(labels) > 0 {
			return labels
		}
	}
	return []string{"OWNER", "MEMBER", "COLLABORATOR"}
}

func reporterTrustAwaitingLabel(ra ReporterAdmitter) string {
	if nc, ok := ra.(ReporterTrustNoticeConfig); ok {
		return strings.TrimSpace(nc.ReporterTrustAwaitingLabel())
	}
	return "needs-triage"
}

func reporterTrustCommentEnabled(ra ReporterAdmitter) bool {
	if nc, ok := ra.(ReporterTrustNoticeConfig); ok {
		return nc.ReporterTrustCommentEnabled()
	}
	return true
}

func trimNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func reporterTrustWaitComment(repo, addedLabel string, ra ReporterAdmitter) string {
	trusted := strings.Join(reporterTrustTrustedAssociationsForNotice(ra), ", ")
	required := reporterTrustRequiredLabelsForNotice(ra)
	labelText := "the label `" + required[0] + "`"
	if len(required) > 1 {
		quoted := make([]string, 0, len(required))
		for _, label := range required {
			quoted = append(quoted, "`"+label+"`")
		}
		labelText = "one of the labels " + strings.Join(quoted, ", ")
	}
	return reporterTrustWaitMarker(repo, addedLabel) + "\n" +
		"Thanks — this hive only works issues from " + trusted + " automatically. " +
		"A maintainer can admit this one by adding " + labelText + " (configured in `issue_filter.reporter_trust.untrusted_require_labels`). " +
		"Until then the hive will not claim, label, or open PRs for it."
}

func (c *Client) markReporterTrustAwaiting(ctx context.Context, repo string, issue *gh.Issue, labels []string, ra ReporterAdmitter) {
	number := issue.GetNumber()
	if !reporterTrustCommentEnabled(ra) {
		return
	}
	comments, err := c.reporterTrustWaitComments(ctx, repo, number)
	if err != nil {
		c.warnReporterTrustWait("comment scan failed", repo, number, err)
		return
	}
	if len(comments) > 0 {
		return
	}

	label := reporterTrustAwaitingLabel(ra)
	addedLabel := ""
	if label != "" && !hasExactLabel(labels, label) {
		if err := c.EnsureIssueLabel(ctx, repo, label, "ededed", "Awaiting maintainer triage before the hive works it"); err != nil {
			c.warnReporterTrustWait("label ensure failed", repo, number, err)
		} else if err := c.AddLabels(ctx, repo, number, []string{label}); err != nil {
			c.warnReporterTrustWait("label add failed", repo, number, err)
		} else {
			addedLabel = label
		}
	}
	if err := c.CreateIssueComment(ctx, repo, number, reporterTrustWaitComment(repo, addedLabel, ra)); err != nil {
		c.warnReporterTrustWait("comment post failed", repo, number, err)
		if addedLabel != "" {
			if removeErr := c.RemoveLabel(ctx, repo, number, addedLabel); removeErr != nil {
				c.warnReporterTrustWait("label cleanup failed", repo, number, removeErr)
			}
		}
	}
}

func (c *Client) clearReporterTrustAwaiting(ctx context.Context, repo string, issue *gh.Issue, labels []string, ra ReporterAdmitter) []string {
	label := reporterTrustAwaitingLabel(ra)
	if label == "" || !hasExactLabel(labels, label) {
		return labels
	}
	comments, err := c.reporterTrustWaitComments(ctx, repo, issue.GetNumber())
	if err != nil {
		c.warnReporterTrustWait("comment scan failed", repo, issue.GetNumber(), err)
		return labels
	}
	if !reporterTrustWaitMarkedAdded(comments, label) {
		return labels
	}
	if err := c.RemoveLabel(ctx, repo, issue.GetNumber(), label); err != nil {
		c.warnReporterTrustWait("label remove failed", repo, issue.GetNumber(), err)
		return labels
	}
	return withoutExactLabel(labels, label)
}

func (c *Client) reporterTrustWaitComments(ctx context.Context, repo string, number int) ([]string, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []string
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, err
		}
		for _, comment := range comments {
			body := comment.GetBody()
			if strings.Contains(body, reporterTrustWaitMarkerPrefix) {
				out = append(out, body)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func reporterTrustWaitMarkedAdded(comments []string, label string) bool {
	want := strings.TrimSpace(label)
	for _, body := range comments {
		start := strings.Index(body, reporterTrustWaitMarkerPrefix)
		if start < 0 {
			continue
		}
		rest := body[start+len(reporterTrustWaitMarkerPrefix):]
		end := strings.Index(rest, "-->")
		if end < 0 {
			continue
		}
		for _, field := range strings.Fields(rest[:end]) {
			raw, ok := strings.CutPrefix(field, "added-label=")
			if !ok {
				continue
			}
			got, err := url.QueryUnescape(raw)
			if err == nil && strings.EqualFold(strings.TrimSpace(got), want) {
				return true
			}
		}
	}
	return false
}

func (c *Client) warnReporterTrustWait(msg, repo string, number int, err error) {
	if c == nil || c.logger == nil || err == nil {
		return
	}
	c.logger.Warn("reporter trust wait "+msg, slog.String("repo", repo), slog.Int("number", number), slog.String("error", err.Error()))
}

func hasExactLabel(labels []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}

func withoutExactLabel(labels []string, drop string) []string {
	out := labels[:0]
	for _, label := range labels {
		if !strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(drop)) {
			out = append(out, label)
		}
	}
	return out
}
