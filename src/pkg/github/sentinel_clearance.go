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

// sentinelReleasedNote is the same closing line for a release made by a
// dashboard Approve bound to this head.
const sentinelReleasedNote = "_@%s approved this head from the Hive dashboard, so Hive treats it as reviewed and will not re-apply the `%s` label unless new commits are pushed._"

// sentinelClearedAfterLatestPush reports whether the newest label event for
// label on the PR is an `unlabeled` by a human who is not one of the hive's
// own accounts, and whether it happened after head was pushed.
//
// The push time comes only from signals GitHub stamps server-side when the
// SHA arrives, never from anything in the commit itself: a committer date
// is set by whoever made the commit, so a contributor could backdate a
// commit pushed after the maintainer's clearance and have the stale
// clearance honoured on new code (#13602 review). In order: the
// `head_ref_force_pushed` timeline event whose commit_id is head; for an
// ordinary push, the earliest check suite or commit status created for
// head. Without any of those nothing is cleared: not knowing is never a
// clearance.
func (c *Client) sentinelClearedAfterLatestPush(ctx context.Context, owner, repo string, number int, head, label string) (bool, string, error) {
	opts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	var (
		lastLabelKind  string
		lastLabelActor *gh.User
		lastLabelAt    time.Time
		forcePushedAt  time.Time
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
			case "head_ref_force_pushed":
				if !strings.EqualFold(strings.TrimSpace(ev.GetCommitID()), head) {
					continue
				}
				if at := ev.GetCreatedAt().Time; at.After(forcePushedAt) {
					forcePushedAt = at
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if lastLabelKind != "unlabeled" {
		return false, "", nil
	}
	actor := strings.TrimSpace(lastLabelActor.GetLogin())
	if actor == "" || c.isHiveAccount(lastLabelActor) {
		return false, "", nil
	}
	if strings.EqualFold(lastLabelActor.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(actor), "[bot]") {
		return false, "", nil
	}
	pushedAt := forcePushedAt
	if pushedAt.IsZero() {
		arrived, err := c.sentinelHeadArrivedAt(ctx, owner, repo, head)
		if err != nil {
			return false, "", err
		}
		pushedAt = arrived
	}
	if pushedAt.IsZero() || !lastLabelAt.After(pushedAt) {
		return false, "", nil
	}
	return true, actor, nil
}

// sentinelReleasedByDashboardApproval reports the login on whose behalf the
// hive posted a dashboard approval command bound to head
// (renderPRApprovalCommandComment), or "" when no such comment exists.
//
// A dashboard Approve takes the alert label off as the hive's own account,
// so sentinelClearedAfterLatestPush rightly refuses to read the `unlabeled`
// event as a human clearance — and the sweep then re-alerted the head the
// owner had just approved (dibs#510: approved 00:32Z, re-labelled 00:33Z).
// The approval comment is the durable, hive-authored record of that
// release: its marker names the head the person saw, so it is never
// honoured on a later push. Only comments the hive itself wrote count;
// anyone can type the marker.
func (c *Client) sentinelReleasedByDashboardApproval(ctx context.Context, owner, repo string, number int, head string) (string, error) {
	head = strings.ToLower(strings.TrimSpace(head))
	if head == "" {
		return "", nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("listing comments of %s/%s#%d for sentinel release: %w", owner, repo, number, err)
	}
	for _, comment := range comments {
		if comment == nil || !c.markerAuthorTrusted(comment.GetUser()) {
			continue
		}
		_, actor, boundHead, ok := ParsePRApprovalCommandComment(comment.GetBody())
		if ok && boundHead == head && actor != "" {
			return actor, nil
		}
	}
	return "", nil
}

// sentinelHeadArrivedAt returns the earliest server-stamped created_at of
// a check suite or commit status for sha, which GitHub records when the SHA
// first reaches the repository; zero when the SHA has neither.
func (c *Client) sentinelHeadArrivedAt(ctx context.Context, owner, repo, sha string) (time.Time, error) {
	var earliest time.Time
	note := func(at time.Time) {
		if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	suiteOpts := &gh.ListCheckSuiteOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		suites, resp, err := c.client.Checks.ListCheckSuitesForRef(ctx, owner, repo, sha, suiteOpts)
		if err != nil {
			return time.Time{}, fmt.Errorf("listing check suites of %s/%s@%s for sentinel clearance: %w", owner, repo, sha, err)
		}
		if suites != nil {
			for _, suite := range suites.CheckSuites {
				if suite != nil {
					note(suite.GetCreatedAt().Time)
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		suiteOpts.Page = resp.NextPage
	}
	statusOpts := &gh.ListOptions{PerPage: 100}
	for {
		statuses, resp, err := c.client.Repositories.ListStatuses(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			return time.Time{}, fmt.Errorf("listing statuses of %s/%s@%s for sentinel clearance: %w", owner, repo, sha, err)
		}
		for _, status := range statuses {
			if status != nil {
				note(status.GetCreatedAt().Time)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}
	return earliest, nil
}

// recordSentinelClearance makes a clearance accepted by
// sentinelClearedAfterLatestPush or sentinelReleasedByDashboardApproval
// durable: the hive's newest marker comment on the PR is rewritten as the
// alert for the current head followed by note (sentinelClearedNote or
// sentinelReleasedNote, already formatted), in the form
// sentinelClearedByMaintainer reads back, so a hub restart (empty
// sentinelHeads) finds "alert for this head, label absent" and stays quiet.
// When the hive has no marker comment on the PR one is posted.
func (c *Client) recordSentinelClearance(ctx context.Context, owner, repo string, number int, note, alert string) error {
	body := alert + "\n" + note + "\n"
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
