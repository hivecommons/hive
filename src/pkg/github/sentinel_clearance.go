package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// sentinelClearedNote closes the marker comment the sweep rewrites when it
// accepts a label removal on a head it never alerted on (#13602); the
// arguments are the login that removed the label and the label name.
const sentinelClearedNote = "_@%s removed the `%s` label after this head was pushed, so Hive treats this head as reviewed and will not re-apply the label unless new commits are pushed._"

// sentinelClearedAfterLatestPush reports whether the newest label event for
// label on the PR is an `unlabeled` by a human who is not one of the hive's
// own accounts, and whether it happened after the PR's most recent push.
// The push time is the latest of the committer dates of the commits on the
// PR's timeline and the latest `head_ref_force_pushed` event, so a rebase
// that moved the head before the maintainer reviewed counts as reviewed,
// while a push that landed after the removal does not (#13602). Without a
// known push time nothing is cleared: not knowing is never a clearance.
func (c *Client) sentinelClearedAfterLatestPush(ctx context.Context, owner, repo string, number int, label string) (bool, string, error) {
	opts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	var (
		lastLabelKind  string
		lastLabelActor *gh.User
		lastLabelAt    time.Time
		pushedAt       time.Time
	)
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return false, "", fmt.Errorf("reading timeline of %s/%s#%d for sentinel clearance: %w", owner, repo, number, err)
		}
		for _, ev := range page {
			if ev == nil {
				continue
			}
			switch strings.ToLower(ev.GetEvent()) {
			case "labeled", "unlabeled":
				if !strings.EqualFold(strings.TrimSpace(ev.GetLabel().GetName()), label) {
					continue
				}
				at := ev.GetCreatedAt().Time
				if at.IsZero() || !at.After(lastLabelAt) {
					continue
				}
				lastLabelKind = strings.ToLower(ev.GetEvent())
				lastLabelActor = ev.GetActor()
				lastLabelAt = at
			case "committed":
				if at := ev.GetCommitter().GetDate().Time; at.After(pushedAt) {
					pushedAt = at
				}
			case "head_ref_force_pushed":
				if at := ev.GetCreatedAt().Time; at.After(pushedAt) {
					pushedAt = at
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if lastLabelKind != "unlabeled" || pushedAt.IsZero() || !lastLabelAt.After(pushedAt) {
		return false, "", nil
	}
	actor := strings.TrimSpace(lastLabelActor.GetLogin())
	if actor == "" || c.isHiveAccount(lastLabelActor) {
		return false, "", nil
	}
	if strings.EqualFold(lastLabelActor.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(actor), "[bot]") {
		return false, "", nil
	}
	return true, actor, nil
}

// recordSentinelClearance makes a clearance accepted by
// sentinelClearedAfterLatestPush durable: the hive's newest marker comment
// on the PR is rewritten as the alert for the current head, in the form
// sentinelClearedByMaintainer reads back, so a hub restart (empty
// sentinelHeads) finds "alert for this head, label absent" and stays quiet.
// When the hive has no marker comment on the PR one is posted.
func (c *Client) recordSentinelClearance(ctx context.Context, owner, repo string, number int, label, clearedBy, alert string) error {
	body := alert + "\n" + fmt.Sprintf(sentinelClearedNote, clearedBy, label) + "\n"
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("listing comments for sentinel clearance: %w", err)
	}
	var own *gh.IssueComment
	for _, comment := range comments {
		if comment == nil || !strings.Contains(comment.GetBody(), SentinelMarker) || !c.markerAuthorTrusted(comment.GetUser()) {
			continue
		}
		own = comment
	}
	if own != nil {
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, own.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return fmt.Errorf("recording sentinel clearance: %w", err)
		}
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("recording sentinel clearance: %w", err)
	}
	return nil
}
