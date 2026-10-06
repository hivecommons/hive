package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// SetOtherNeedsHumanReason installs the scanner's check for independent human
// hand-offs (notably the fix-loop ledger). An absent check fails closed.
func (c *Client) SetOtherNeedsHumanReason(fn func(repo string, number int) bool) {
	if c == nil {
		return
	}
	c.reporterTrustMu.Lock()
	defer c.reporterTrustMu.Unlock()
	c.otherNeedsHumanReason = fn
}

// clearReleasedReporterTrustNeedsHuman runs before the held partition is built,
// so a successful cleanup returns the PR to the automated lane on this poll.
func (c *Client) clearReleasedReporterTrustNeedsHuman(ctx context.Context, repo string, number int, labels []string) []string {
	if HasHoldLabel(labels) || !hasExactLabel(labels, issueNeedsHumanLabel) || strings.TrimSpace(c.appBotLogin) == "" {
		return labels
	}
	c.reporterTrustMu.RLock()
	otherReason := c.otherNeedsHumanReason
	c.reporterTrustMu.RUnlock()
	owner, name := c.splitRepo(repo)
	repoRef := owner + "/" + name
	if otherReason == nil || otherReason(repoRef, number) {
		return labels
	}
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return labels
	}
	var notice *gh.IssueComment
	for _, comment := range comments {
		if c.isTrustedAppBotCommentAuthor(comment) && IsReporterTrustHoldNotice(comment.GetBody()) && (notice == nil || !comment.GetCreatedAt().Before(notice.GetCreatedAt().Time)) {
			notice = comment
		}
	}
	if notice == nil || notice.GetCreatedAt().IsZero() {
		return labels
	}
	// Inspect the complete event history. A newer needs-human label event belongs
	// to another hand-off, even if the same App authored it. Never clear that.
	var hold, holdApplied, human *gh.IssueEvent
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, name, number, opts)
		if err != nil {
			return labels
		}
		for _, event := range events {
			if event == nil || (event.GetEvent() != "labeled" && event.GetEvent() != "unlabeled") {
				continue
			}
			switch strings.ToLower(event.GetLabel().GetName()) {
			case "hold":
				if event.GetEvent() == "labeled" && (holdApplied == nil || !event.GetCreatedAt().Before(holdApplied.GetCreatedAt().Time)) {
					holdApplied = event
				}
				if hold == nil || !event.GetCreatedAt().Before(hold.GetCreatedAt().Time) {
					hold = event
				}
			case issueNeedsHumanLabel:
				if human == nil || !event.GetCreatedAt().Before(human.GetCreatedAt().Time) {
					human = event
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if hold == nil || hold.GetCreatedAt().IsZero() || hold.GetEvent() != "unlabeled" || hold.GetCreatedAt().Before(notice.GetCreatedAt().Time) {
		return labels
	}
	actor := hold.GetActor()
	if actor == nil || strings.TrimSpace(actor.GetLogin()) == "" || strings.EqualFold(actor.GetLogin(), c.appBotLogin) || !strings.EqualFold(actor.GetType(), "User") {
		return labels
	}
	if holdApplied == nil || !strings.EqualFold(safeGetLogin(holdApplied.GetActor()), c.appBotLogin) || holdApplied.GetCreatedAt().After(notice.GetCreatedAt().Time) {
		return labels
	}
	if human == nil || human.GetCreatedAt().IsZero() || human.GetCreatedAt().Before(holdApplied.GetCreatedAt().Time) || human.GetEvent() != "labeled" || !strings.EqualFold(safeGetLogin(human.GetActor()), c.appBotLogin) || human.GetCreatedAt().After(notice.GetCreatedAt().Time) {
		return labels
	}
	// Recheck the live ledger immediately before the gated write.
	if otherReason(repoRef, number) {
		return labels
	}
	if err := c.RemoveLabel(ctx, repoRef, number, issueNeedsHumanLabel); err != nil {
		return labels
	}
	return removeIssueLabel(labels, issueNeedsHumanLabel)
}
